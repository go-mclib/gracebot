package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-mclib/client/client"
)

// InitLexicon registers lexicon-related commands
func InitLexicon() {
	RegisterCommand("define", "Define a word", "!define <word>", nil, defineCommand)
	RegisterCommand("urban", "Urban Dictionary lookup", "!urban <phrase>", nil, urbanCommand)
}

// dictionaryResponse represents the response from dictionary API
type dictionaryResponse struct {
	Word     string `json:"word"`
	Meanings []struct {
		PartOfSpeech string `json:"partOfSpeech"`
		Definitions  []struct {
			Definition string `json:"definition"`
		} `json:"definitions"`
	} `json:"meanings"`
}

// defineCommand looks up a word definition
func defineCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Usage: !define <word>")
		return
	}

	word := strings.ToLower(args[0])
	apiURL := fmt.Sprintf("https://api.dictionaryapi.dev/api/v2/entries/en/%s", url.QueryEscape(word))

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		c.SendChatMessage("Failed to look up definition.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		c.SendChatMessage(fmt.Sprintf("No definition found for '%s'", word))
		return
	}

	if resp.StatusCode != http.StatusOK {
		c.SendChatMessage("Failed to look up definition.")
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.SendChatMessage("Failed to read definition.")
		return
	}

	var results []dictionaryResponse
	if err := json.Unmarshal(body, &results); err != nil || len(results) == 0 {
		c.SendChatMessage("Failed to parse definition.")
		return
	}

	// Get first definition
	result := results[0]
	if len(result.Meanings) == 0 || len(result.Meanings[0].Definitions) == 0 {
		c.SendChatMessage(fmt.Sprintf("No definition found for '%s'", word))
		return
	}

	partOfSpeech := result.Meanings[0].PartOfSpeech
	definition := result.Meanings[0].Definitions[0].Definition

	// Truncate if too long
	if len(definition) > 200 {
		definition = definition[:197] + "..."
	}

	c.SendChatMessage(fmt.Sprintf("%s (%s): %s", word, partOfSpeech, definition))
}

// urbanResponse represents the response from Urban Dictionary API
type urbanResponse struct {
	List []struct {
		Word       string `json:"word"`
		Definition string `json:"definition"`
		Example    string `json:"example"`
		ThumbsUp   int    `json:"thumbs_up"`
		ThumbsDown int    `json:"thumbs_down"`
	} `json:"list"`
}

// urbanCommand looks up a phrase on Urban Dictionary
func urbanCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Usage: !urban <phrase>")
		return
	}

	phrase := strings.Join(args, " ")
	apiURL := fmt.Sprintf("https://api.urbandictionary.com/v0/define?term=%s", url.QueryEscape(phrase))

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		c.SendChatMessage("Failed to look up definition.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		c.SendChatMessage("Failed to look up definition.")
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.SendChatMessage("Failed to read definition.")
		return
	}

	var result urbanResponse
	if err := json.Unmarshal(body, &result); err != nil {
		c.SendChatMessage("Failed to parse definition.")
		return
	}

	if len(result.List) == 0 {
		c.SendChatMessage(fmt.Sprintf("No definition found for '%s'", phrase))
		return
	}

	// Get top definition
	entry := result.List[0]
	definition := entry.Definition

	// Clean up Urban Dictionary's bracket notation
	definition = strings.ReplaceAll(definition, "[", "")
	definition = strings.ReplaceAll(definition, "]", "")
	definition = strings.ReplaceAll(definition, "\r\n", " ")
	definition = strings.ReplaceAll(definition, "\n", " ")

	// Truncate if too long
	if len(definition) > 200 {
		definition = definition[:197] + "..."
	}

	c.SendChatMessage(fmt.Sprintf("%s: %s", entry.Word, definition))
}
