package main

import (
	"os"
	"time"
)

const (
	// Timing constants
	ChatCheckInterval = 2 * time.Second // How often to check if we should respond
	InitialDelay      = 1 * time.Second // Delay before bot starts monitoring chat
	MaxMessageLen     = 255             // Minecraft chat message length limit

	// Context management
	MaxContextTokens     = 6000             // Max tokens for context window (leave room for model limits)
	MaxMessageHistoryAge = 30 * time.Minute // Prune messages older than this

	// Engagement parameters - tuned for human-like behavior
	BaseProbability       = 0.08            // 8% chance to respond to general chat
	MentionProbability    = 0.95            // 95% chance when mentioned directly
	QuestionProbability   = 0.25            // 25% chance when question detected
	ConversationBoost     = 0.30            // +30% during active conversations
	MinMessagesSinceReply = 5               // Wait at least 5 messages before responding again
	MaxSilentMessages     = 24              // Forced response after 24 messages of silence
	MinTimeBetweenReplies = 8 * time.Second // Minimum 8s between responses

	// Multiline response settings
	MultilineDelayMin = 2 * time.Second // Minimum delay between lines
	MultilineDelayMax = 4 * time.Second // Maximum delay between lines
	MaxResponseTokens = 150             // Max tokens for LLM response (allows multiline)

	// Conversation tracking
	ConversationTimeout = 2 * time.Minute // Conversation inactive after 2 min
	EngagementDecayRate = 0.85            // Engagement decays by 15% per tick
)

type Config struct {
	GroqAPIKey    string
	SystemPrompt  string
	AzureClientID string
	BotAliases    []string // Aliases that can trigger the bot (e.g., "grace", "bot")
}

func LoadConfig() *Config {
	return &Config{
		GroqAPIKey:    os.Getenv("GROQ_API_KEY"),
		SystemPrompt:  os.Getenv("GROQ_SYSTEM_PROMPT"),
		AzureClientID: os.Getenv("AZURE_CLIENT_ID"),
		BotAliases:    GetBotAliases(),
	}
}

// GetBotAliases returns the list of bot aliases from environment or defaults
// Set via GROQ_BOT_ALIASES env var, comma-separated (e.g., "grace,bot")
func GetBotAliases() []string {
	aliasesEnv := os.Getenv("GROQ_BOT_ALIASES")
	if aliasesEnv == "" {
		return []string{} // No aliases by default
	}

	// Split by comma and trim whitespace
	aliases := make([]string, 0)
	for _, alias := range splitAndTrim(aliasesEnv, ",") {
		if alias != "" {
			aliases = append(aliases, alias)
		}
	}
	return aliases
}

func splitAndTrim(s, sep string) []string {
	parts := make([]string, 0)
	for _, part := range splitString(s, sep) {
		trimmed := trimSpace(part)
		if trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	return parts
}

func splitString(s, sep string) []string {
	if s == "" {
		return []string{}
	}
	result := make([]string, 0)
	current := ""
	for _, r := range s {
		if string(r) == sep {
			result = append(result, current)
			current = ""
		} else {
			current += string(r)
		}
	}
	result = append(result, current)
	return result
}

func trimSpace(s string) string {
	start := 0
	end := len(s)

	for start < end && (s[start] == ' ' || s[start] == '\t' || s[start] == '\n' || s[start] == '\r') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\n' || s[end-1] == '\r') {
		end--
	}

	return s[start:end]
}

// GetEngagementConfig returns the engagement configuration
func GetEngagementConfig() *EngagementConfig {
	return &EngagementConfig{
		BaseProbability:       BaseProbability,
		MentionProbability:    MentionProbability,
		QuestionProbability:   QuestionProbability,
		MinMessagesSinceReply: MinMessagesSinceReply,
		MaxSilentMessages:     MaxSilentMessages,
		MinTimeBetweenReplies: MinTimeBetweenReplies,
		ConversationBoost:     ConversationBoost,
	}
}
