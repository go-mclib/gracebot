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
	MinMessagesSinceReply = 5               // Wait at least 2 messages before responding again
	MaxSilentMessages     = 24              // Forced response after 12 messages of silence
	MinTimeBetweenReplies = 8 * time.Second // Minimum 8s between responses

	// Conversation tracking
	ConversationTimeout = 2 * time.Minute // Conversation inactive after 2 min
	EngagementDecayRate = 0.85            // Engagement decays by 15% per tick
)

type Config struct {
	GroqAPIKey    string
	SystemPrompt  string
	AzureClientID string
}

func LoadConfig() *Config {
	return &Config{
		GroqAPIKey:    os.Getenv("GROQ_API_KEY"),
		SystemPrompt:  os.Getenv("GROQ_SYSTEM_PROMPT"),
		AzureClientID: os.Getenv("AZURE_CLIENT_ID"),
	}
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
