package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func weatherTool() toolSpec {
	return makeTool("weather", "Get current weather for a named location.", map[string]any{
		"location": map[string]any{"type": "string", "description": "City or location name"},
	}, []string{"location"}, func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
		var args struct {
			Location string `json:"location"`
		}
		if err := decodeArgs(raw, &args); err != nil {
			return nil, err
		}
		location := strings.TrimSpace(args.Location)
		if location == "" || len(location) > 200 {
			return nil, fmt.Errorf("provide a location of at most 200 bytes")
		}
		endpoint := "https://wttr.in/" + url.PathEscape(location) + "?format=%25l:+%25C+%25t+%25h+%25w"
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", "curl/8.0 GraceBot")
		httpClient := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 || req.URL.Scheme != "https" || req.URL.Host != "wttr.in" {
				return fmt.Errorf("unexpected weather redirect")
			}
			return nil
		}}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("weather service returned HTTP %d", resp.StatusCode)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8193))
		if err != nil {
			return nil, err
		}
		if len(body) > 8192 {
			return nil, fmt.Errorf("weather response too large")
		}
		weather := strings.TrimSpace(string(body))
		if weather == "" || strings.Contains(weather, "Unknown location") {
			return nil, fmt.Errorf("unknown weather location")
		}
		return map[string]string{"location": location, "weather": weather}, nil
	})
}
