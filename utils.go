package main

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-mclib/client/client"
)

func init() {
	RegisterCommand("weather", "Get weather for a location", "!weather <location>", nil, weatherCommand)
}

// weatherCommand handles the !weather command
func weatherCommand(c *client.Client, sender string, args []string) {
	if len(args) < 1 {
		c.SendChatMessage("Usage: !weather <location>")
		return
	}

	location := strings.Join(args, " ")
	weather, err := fetchWeather(location)
	if err != nil {
		c.SendChatMessage(fmt.Sprintf("Failed to get weather: %s", err.Error()))
		return
	}

	c.SendChatMessage(weather)
}

// fetchWeather fetches weather from wttr.in
func fetchWeather(location string) (string, error) {
	// wttr.in format: %C = condition, %t = temp, %h = humidity, %w = wind
	// ?format=3 gives a compact one-line format
	encoded := url.QueryEscape(location)
	apiURL := fmt.Sprintf("https://wttr.in/%s?format=%%l:+%%C+%%t+%%h+%%w", encoded)

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "curl/7.64.1") // wttr.in prefers curl UA

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("weather service returned %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	result := strings.TrimSpace(string(body))
	if result == "" || strings.Contains(result, "Unknown location") {
		return "", fmt.Errorf("unknown location")
	}

	return result, nil
}
