package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
)

func TestWebURLPolicy(t *testing.T) {
	for _, raw := range []string{
		"http://en.wikipedia.org/wiki/Stone", "https://evil.test/wiki/Stone",
		"https://en.wikipedia.org.evil.test/wiki/Stone", "https://en.wikipedia.org@evil.test/wiki/Stone",
		"https://user@en.wikipedia.org/wiki/Stone", "https://en.wikipedia.org:444/wiki/Stone",
		"https://en.wikipedia.org./wiki/Stone", "https://127.0.0.1/wiki/Stone", "https://[::1]/wiki/Stone",
		"https://en.wikipedia.org/w/api.php", "https://en.wikipedia.org/wiki/../w/api.php",
		"https://en.wikipedia.org/wiki/%2e%2e/w/api.php", "https://en.wikipedia.org/wiki/%252e%252e/w/api.php",
		"https://en.wikipedia.org/wiki/Stone?redirect=https://localhost", "https://www.bbc.com/newsletter/",
		"https://query1.finance.yahoo.com/v8/finance/chart/AAPL?interval=1d&range=1d",
	} {
		t.Run(raw, func(t *testing.T) {
			u, err := url.Parse(raw)
			if err == nil && validateWebURL(u, "website") == nil {
				t.Fatalf("accepted denied URL %q", raw)
			}
		})
	}
	for _, raw := range []string{"https://en.wikipedia.org/wiki/Stone#Uses", "https://minecraft.wiki/w/Redstone", "https://docs.papermc.io:443/paper/getting-started/", "https://www.bbc.com/news/articles/abc"} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateWebURL(u, "website"); err != nil {
			t.Errorf("denied article %q: %v", raw, err)
		}
	}
}

func TestWebPublicAddresses(t *testing.T) {
	for _, raw := range []string{
		"0.0.0.1", "10.1.2.3", "100.64.0.1", "127.0.0.1", "169.254.169.254", "172.31.1.1",
		"192.0.0.9", "192.0.2.1", "192.168.1.1", "192.88.99.1", "198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:192.168.0.1", "fc00::1", "fe80::1", "ff02::1", "64:ff9b::a00:1",
		"2001::1", "2001:db8::1", "2002:7f00:1::", "3fff::1", "2606:4700::1%en0",
	} {
		if publicWebIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted non-public IP %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !publicWebIP(netip.MustParseAddr(raw)) {
			t.Errorf("denied public IP %s", raw)
		}
	}
}

func TestWebDNSRejectsMixedAnswersAndPinsDial(t *testing.T) {
	dials, lookups := 0, 0
	lookup := func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("1.1.1.1"), netip.MustParseAddr("127.0.0.1")}, nil
	}
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials++
		return nil, errors.New("unexpected dial")
	}
	if _, err := dialPublicWeb(context.Background(), "tcp", "en.wikipedia.org:443", lookup, dial); err == nil || dials != 0 {
		t.Fatalf("mixed DNS answers reached dial: err=%v dials=%d", err, dials)
	}
	lookups = 0
	lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		lookups++
		if lookups > 1 {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		}
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	sentinel := errors.New("verified numeric destination")
	dial = func(_ context.Context, _, address string) (net.Conn, error) {
		if address != "1.1.1.1:443" {
			t.Errorf("dial used unpinned address %q", address)
		}
		return nil, sentinel
	}
	if _, err := dialPublicWeb(context.Background(), "tcp", "en.wikipedia.org:443", lookup, dial); !errors.Is(err, sentinel) || lookups != 1 {
		t.Fatalf("DNS rebinding defense failed: lookups=%d err=%v", lookups, err)
	}
}

type webTestTransport func(*http.Request) (*http.Response, error)

func (f webTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func webTestResponse(r *http.Request, code int, kind, body string) *http.Response {
	return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {kind}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func TestWebRedirectPolicyBeforeNextRequest(t *testing.T) {
	for _, target := range []string{"http://en.wikipedia.org/wiki/Stone", "https://evil.test/", "https://en.wikipedia.org/w/api.php", "https://127.0.0.1/wiki/Stone", "https://en.wikipedia.org/wiki/Stone?x=1"} {
		t.Run(target, func(t *testing.T) {
			calls := 0
			w := &webTools{client: &http.Client{Transport: webTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := webTestResponse(r, http.StatusFound, "text/html", "redirect")
				resp.Header.Set("Location", target)
				return resp, nil
			})}}
			_, _, _, err := w.request(context.Background(), "https://en.wikipedia.org/wiki/Stone", "website")
			if err == nil || calls != 1 {
				t.Fatalf("redirect escaped policy: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestWebResponseBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, kind, body string
		status           int
	}{
		{"binary", "image/png", "not text", 200},
		{"oversized", "text/plain", strings.Repeat("x", webBodyLimit+1), 200},
		{"challenge status", "text/html", "challenge", 202},
		{"rate limit", "text/html", "slow down", 429},
		{"invalid UTF8", "text/plain", "\xff", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &webTools{client: &http.Client{Transport: webTestTransport(func(r *http.Request) (*http.Response, error) {
				return webTestResponse(r, tc.status, tc.kind, tc.body), nil
			})}}
			if _, _, _, err := w.request(context.Background(), "https://en.wikipedia.org/wiki/Stone", "website"); err == nil {
				t.Fatal("accepted invalid response")
			}
		})
	}
	calls := 0
	w := &webTools{client: &http.Client{Transport: webTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		resp := webTestResponse(r, http.StatusFound, "text/html", "redirect")
		resp.Header.Set("Location", "/wiki/Another")
		return resp, nil
	})}}
	if _, _, _, err := w.request(context.Background(), "https://en.wikipedia.org/wiki/Stone", "website"); err == nil || calls != 4 {
		t.Fatalf("unbounded redirect chain: calls=%d err=%v", calls, err)
	}
}

func TestDuckDuckGoHTMLAndLiteResults(t *testing.T) {
	body := `<div class="result results_links"><h2 class="result__title"><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fminecraft.wiki%2Fw%2FRedstone&amp;rut=abc">Redstone &amp; <b>circuits</b></a></h2><a class="result__snippet">Use <b>redstone</b> &amp; repeaters.</a></div><div class="result"><a class="result__a" href="https://example.org/">Another</a></div>`
	results, err := parseDuckDuckGo([]byte(body), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Title != "Redstone & circuits" || results[0].URL != "https://minecraft.wiki/w/Redstone" || results[0].Snippet != "Use redstone & repeaters." || !results[0].FetchAllowed {
		t.Fatalf("incorrect HTML result: %#v", results)
	}
	lite := `<table><tr><td>1.</td><td><a class="result-link" href="https://example.org/article">Outside allowlist</a></td></tr><tr><td></td><td class="result-snippet">A <b>real</b> snippet &amp; details</td></tr></table>`
	results, err = parseDuckDuckGo([]byte(lite), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Snippet != "A real snippet & details" || results[0].FetchAllowed {
		t.Fatalf("incorrect lite result: %#v", results)
	}
}

func TestDuckDuckGoChallengeIsNotEmptySearch(t *testing.T) {
	for _, body := range []string{
		`<form id="challenge-form" action="//duckduckgo.com/anomaly.js"><div class="anomaly-modal__modal">Select all squares containing a duck</div></form>`,
		`<title>Just a moment...</title>`, `<html><body>Service temporarily unavailable</body></html>`,
	} {
		if _, err := parseDuckDuckGo([]byte(body), 5); err == nil {
			t.Fatal("challenge/error reported as zero results")
		}
	}
	results, err := parseDuckDuckGo([]byte(`<div class="no-results"><h1>No results found for missingquery</h1></div>`), 5)
	if err != nil || len(results) != 0 {
		t.Fatalf("genuine empty search not recognized: %#v %v", results, err)
	}
}

func TestReadableHTMLSkipsActiveContentAndBoundsUnicode(t *testing.T) {
	doc, err := html.Parse(strings.NewReader(`<main><p>A &amp; <b>B</b></p>after<script>steal()</script><style>bad</style><iframe src="https://evil.test">hidden frame</iframe><span hidden>secret</span><p>末尾</p></main>`))
	if err != nil {
		t.Fatal(err)
	}
	if got := htmlReadable(doc, 100); got != "A & B after 末尾" {
		t.Fatalf("unexpected readable text %q", got)
	}
	doc, err = html.Parse(strings.NewReader(`<p>` + strings.Repeat(" ", 100) + strings.Repeat("界", 100) + `</p>`))
	if err != nil {
		t.Fatal(err)
	}
	if got := htmlReadable(doc, 7); !utf8.ValidString(got) || got != strings.Repeat("界", 7) {
		t.Fatalf("incorrect Unicode bound %q", got)
	}
}

func TestYahooQuoteUsesProviderTimeAndRejectsInvalidData(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	meta := map[string]any{"symbol": "AAPL", "currency": "USD", "fullExchangeName": "NasdaqGS", "regularMarketPrice": 332.27, "regularMarketTime": int64(1789156801)}
	encode := func() []byte {
		body, err := json.Marshal(map[string]any{"chart": map[string]any{"result": []any{map[string]any{"meta": meta}}, "error": nil}})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	source := "https://query1.finance.yahoo.com/v8/finance/chart/AAPL?interval=1d&range=1d"
	quote, err := parseYahooQuote(encode(), "AAPL", source, now)
	if err != nil {
		t.Fatal(err)
	}
	if quote.Price != 332.27 || quote.QuotedAt.Unix() != 1789156801 || quote.FetchedAt != now || quote.Source != source || quote.Currency != "USD" {
		t.Fatalf("quote loses price/time/source: %#v", quote)
	}
	for _, tc := range []struct {
		key   string
		value any
	}{{"symbol", "MSFT"}, {"regularMarketPrice", nil}, {"regularMarketPrice", -1}, {"regularMarketTime", 0}, {"regularMarketTime", now.Add(time.Hour).Unix()}, {"currency", ""}} {
		old := meta[tc.key]
		meta[tc.key] = tc.value
		if _, err := parseYahooQuote(encode(), "AAPL", source, now); err == nil {
			t.Errorf("accepted invalid %s=%v", tc.key, tc.value)
		}
		meta[tc.key] = old
	}
	for _, body := range []string{`{"chart":{"result":null,"error":{"code":"Not Found","description":"No data found, symbol may be delisted"}}}`, `{"chart":{"result":[]}}`, `<html>Denied</html>`} {
		if _, err := parseYahooQuote([]byte(body), "AAPL", source, now); err == nil {
			t.Fatal("quote error produced a price")
		}
	}
}

func TestDuckDuckGoFallsBackAfterChallenge(t *testing.T) {
	for _, bothBlocked := range []bool{false, true} {
		calls := 0
		w := &webTools{client: &http.Client{Transport: webTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host == "html.duckduckgo.com" || bothBlocked {
				return webTestResponse(r, http.StatusAccepted, "text/html", `<form id="challenge-form">challenge</form>`), nil
			}
			return webTestResponse(r, http.StatusOK, "text/html", `<table><tr><td><a class="result-link" href="https://minecraft.wiki/w/Diamond">Diamond</a></td></tr><tr><td class="result-snippet">Mining details</td></tr></table>`), nil
		})}}
		result, err := w.searchTool(context.Background(), "player", json.RawMessage(`{"query":"minecraft diamond"}`))
		if calls != 2 {
			t.Fatalf("expected bounded HTML/lite attempts, got %d", calls)
		}
		if bothBlocked {
			if err == nil {
				t.Fatal("both challenges reported as search success")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		data := result.(map[string]any)
		results := data["results"].([]webSearchResult)
		if len(results) != 1 || results[0].URL != "https://minecraft.wiki/w/Diamond" || !strings.HasPrefix(data["source"].(string), "https://lite.duckduckgo.com/lite/") {
			t.Fatalf("fallback lost source/results: %#v", data)
		}
	}
}
