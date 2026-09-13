package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func (a *activities) define(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Word string `json:"word"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	word := strings.TrimSpace(args.Word)
	if word == "" || len(word) > 200 {
		return nil, fmt.Errorf("provide a word of at most 200 bytes")
	}
	var entries []struct {
		Word     string `json:"word"`
		Meanings []struct {
			PartOfSpeech string `json:"partOfSpeech"`
			Definitions  []struct {
				Definition string `json:"definition"`
				Example    string `json:"example"`
			} `json:"definitions"`
		} `json:"meanings"`
	}
	if err := a.getJSON(ctx, "https://api.dictionaryapi.dev/api/v2/entries/en/"+url.PathEscape(word), &entries); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		for _, meaning := range entry.Meanings {
			for _, definition := range meaning.Definitions {
				if strings.TrimSpace(definition.Definition) != "" {
					return map[string]any{"word": entry.Word, "part_of_speech": meaning.PartOfSpeech, "definition": definition.Definition, "example": definition.Example}, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("no definition found for %q", word)
}

func (a *activities) urban(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Phrase string `json:"phrase"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	phrase := strings.TrimSpace(args.Phrase)
	if phrase == "" || len(phrase) > 500 {
		return nil, fmt.Errorf("provide a phrase of at most 500 bytes")
	}
	var result struct {
		List []struct {
			Word       string `json:"word"`
			Definition string `json:"definition"`
			Example    string `json:"example"`
			Permalink  string `json:"permalink"`
		} `json:"list"`
	}
	if err := a.getJSON(ctx, "https://api.urbandictionary.com/v0/define?term="+url.QueryEscape(phrase), &result); err != nil {
		return nil, err
	}
	for _, entry := range result.List {
		if strings.TrimSpace(entry.Definition) != "" {
			clean := strings.NewReplacer("[", "", "]", "")
			return map[string]any{"word": entry.Word, "definition": clean.Replace(entry.Definition), "example": clean.Replace(entry.Example), "source": entry.Permalink}, nil
		}
	}
	return nil, fmt.Errorf("no Urban Dictionary definition found for %q", phrase)
}
