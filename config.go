package main

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	OpenRouterAPIKey string
	SystemPrompt     string
	BotAliases       []string
	AzureClientID    string
	MaxChatLog       int // max messages to store before triggering LLM check
}

func LoadConfig() Config {
	maxChatLog := 30 // default
	if val := os.Getenv("MAX_CHAT_LOG"); val != "" {
		if parsed, err := strconv.Atoi(val); err == nil && parsed > 0 {
			maxChatLog = parsed
		}
	}

	// load system prompt
	systemPrompt := os.Getenv("SYSTEM_PROMPT")
	if promptFile := os.Getenv("SYSTEM_PROMPT_FILE"); promptFile != "" {
		if content, err := os.ReadFile(promptFile); err == nil {
			systemPrompt = string(content)
		}
	}

	// append personality
	if personalityFile := os.Getenv("PERSONALITY_FILE"); personalityFile != "" {
		if content, err := os.ReadFile(personalityFile); err == nil {
			personality := strings.TrimSpace(string(content))
			if personality != "" {
				systemPrompt += "\n\n" + personality
			}
		}
	}

	// parse aliases
	aliasesStr := os.Getenv("BOT_ALIASES")
	var aliases []string
	if aliasesStr != "" {
		for _, alias := range strings.Split(aliasesStr, ",") {
			alias = strings.TrimSpace(strings.ToLower(alias))
			if alias != "" {
				aliases = append(aliases, alias)
			}
		}
	}

	return Config{
		OpenRouterAPIKey: os.Getenv("OPENROUTER_API_KEY"),
		SystemPrompt:     systemPrompt,
		BotAliases:       aliases,
		AzureClientID:    os.Getenv("AZURE_CLIENT_ID"),
		MaxChatLog:       maxChatLog,
	}
}
