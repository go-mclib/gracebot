package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"time"
)

type activities struct {
	store    *playerStore
	send     func(context.Context, string) error
	client   *http.Client
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	song     *songSession
	lastSong songState
	wg       sync.WaitGroup
}

func newActivities(store *playerStore, send func(context.Context, string) error) *activities {
	ctx, cancel := context.WithCancel(context.Background())
	return &activities{store: store, send: send, client: &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 || req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
				return fmt.Errorf("lookup redirect refused")
			}
			return nil
		},
	}, ctx: ctx, cancel: cancel}
}

func (a *activities) close() {
	a.mu.Lock()
	a.closed = true
	a.cancel()
	a.mu.Unlock()
	a.wg.Wait()
	a.client.CloseIdleConnections()
}

func (a *activities) tools() []toolSpec {
	return []toolSpec{
		makeTool("roll_dice", "Roll random dice. Defaults to one six-sided die.", map[string]any{
			"count": map[string]any{"type": "integer", "minimum": 1, "maximum": 100},
			"sides": map[string]any{"type": "integer", "minimum": 2, "maximum": 1000},
		}, nil, a.rollDice),
		makeTool("flip_coin", "Flip a random fair coin.", nil, nil, a.flipCoin),
		makeTool("choose", "Choose one of the supplied options at random.", map[string]any{
			"options": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 2, "maxItems": 100},
		}, []string{"options"}, a.choose),
		makeTool("eight_ball", "Give a random magic eight-ball answer for entertainment, not factual advice.", map[string]any{
			"question": map[string]any{"type": "string"},
		}, []string{"question"}, a.eightBall),
		makeTool("quote", "Fetch a random quotation and its author.", nil, nil, a.quote),
		makeTool("fact", "Fetch a random fun fact.", nil, nil, a.fact),
		makeTool("define", "Look up an English word in the dictionary.", map[string]any{
			"word": map[string]any{"type": "string"},
		}, []string{"word"}, a.define),
		makeTool("urban", "Look up slang in Urban Dictionary. Results may contain adult language.", map[string]any{
			"phrase": map[string]any{"type": "string"},
		}, []string{"phrase"}, a.urban),
		makeTool("sing", "Find lyrics and start singing in chat asynchronously. Requires admin or trusted rank. Returns the selected track and playback state.", map[string]any{
			"query": map[string]any{"type": "string", "description": "Song title and optionally artist; not a URL."},
		}, []string{"query"}, a.sing),
		makeTool("stop_singing", "Stop the current song or lyrics search. Only its owner or an admin may stop it.", nil, nil, a.stopSinging),
		makeTool("singing_status", "Get current or most recent song playback state, including playback errors.", nil, nil, a.singingStatus),
	}
}

func (a *activities) rollDice(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	args := struct {
		Count int `json:"count"`
		Sides int `json:"sides"`
	}{Count: 1, Sides: 6}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Count < 1 || args.Count > 100 || args.Sides < 2 || args.Sides > 1000 {
		return nil, fmt.Errorf("use 1–100 dice with 2–1000 sides")
	}
	rolls := make([]int, args.Count)
	total := 0
	for i := range rolls {
		rolls[i] = rand.IntN(args.Sides) + 1
		total += rolls[i]
	}
	return map[string]any{"rolls": rolls, "total": total, "sides": args.Sides}, nil
}

func (a *activities) flipCoin(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	return map[string]any{"result": [2]string{"heads", "tails"}[rand.IntN(2)]}, nil
}

func (a *activities) choose(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Options []string `json:"options"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Options) < 2 || len(args.Options) > 100 {
		return nil, fmt.Errorf("provide 2–100 options")
	}
	for i, option := range args.Options {
		args.Options[i] = strings.TrimSpace(option)
		if args.Options[i] == "" {
			return nil, fmt.Errorf("options must not be blank")
		}
	}
	return map[string]any{"choice": args.Options[rand.IntN(len(args.Options))]}, nil
}

func (a *activities) eightBall(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Question string `json:"question"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(args.Question) == "" {
		return nil, fmt.Errorf("provide a question")
	}
	responses := [...]string{
		"It is certain.", "Without a doubt.", "Yes, definitely.", "You may rely on it.",
		"As I see it, yes.", "Most likely.", "Outlook good.", "Yes.", "Signs point to yes.",
		"Reply hazy, try again.", "Ask again later.", "Better not tell you now.", "Cannot predict now.",
		"Concentrate and ask again.", "Don't count on it.", "My reply is no.", "My sources say no.",
		"Outlook not so good.", "Very doubtful.",
	}
	return map[string]any{"answer": responses[rand.IntN(len(responses))]}, nil
}

func (a *activities) quote(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	var quotes []struct {
		Text   string `json:"q"`
		Author string `json:"a"`
	}
	if err := a.getJSON(ctx, "https://zenquotes.io/api/random", &quotes); err != nil {
		return nil, err
	}
	if len(quotes) == 0 || strings.TrimSpace(quotes[0].Text) == "" {
		return nil, fmt.Errorf("quote service returned no quotation")
	}
	return map[string]any{"quote": quotes[0].Text, "author": quotes[0].Author}, nil
}

func (a *activities) fact(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	var fact struct {
		Text   string `json:"text"`
		Source string `json:"source_url"`
	}
	if err := a.getJSON(ctx, "https://uselessfacts.jsph.pl/api/v2/facts/random?language=en", &fact); err != nil {
		return nil, err
	}
	if strings.TrimSpace(fact.Text) == "" {
		return nil, fmt.Errorf("fact service returned no fact")
	}
	return map[string]any{"fact": fact.Text, "source": fact.Source}, nil
}

// Callers construct URLs from fixed service endpoints, never from model-provided URLs.
func (a *activities) getJSON(ctx context.Context, endpoint string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create lookup request: %w", err)
	}
	req.Header.Set("User-Agent", "GraceBot/1.0")
	req.Header.Set("Accept", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("lookup %s: %w", req.URL.Host, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lookup %s returned HTTP %d", req.URL.Host, resp.StatusCode)
	}
	const maxBody = 2 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("read %s response: %w", req.URL.Host, err)
	}
	if len(body) > maxBody {
		return fmt.Errorf("%s response exceeds %d bytes", req.URL.Host, maxBody)
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("decode %s response: %w", req.URL.Host, err)
	}
	return nil
}
