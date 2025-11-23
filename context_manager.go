package main

import (
	"sync"
	"time"

	"github.com/conneroisu/groq-go"
)

// Message represents a chat message with metadata
type Message struct {
	Username  string
	Content   string
	Timestamp time.Time
	IsBot     bool
}

// ContextManager manages the chat history and keeps it within token limits
type ContextManager struct {
	messages     []Message
	maxTokens    int
	systemPrompt string
	mu           sync.RWMutex
}

func NewContextManager(systemPrompt string, maxTokens int) *ContextManager {
	return &ContextManager{
		messages:     make([]Message, 0),
		maxTokens:    maxTokens,
		systemPrompt: systemPrompt,
	}
}

// AddMessage adds a message to the history
func (cm *ContextManager) AddMessage(username, content string, isBot bool) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	msg := Message{
		Username:  username,
		Content:   content,
		Timestamp: time.Now(),
		IsBot:     isBot,
	}

	cm.messages = append(cm.messages, msg)
}

// EstimateTokens provides a rough estimate of tokens in a string
// Using ~4 characters per token as a rough approximation
func EstimateTokens(text string) int {
	return len(text) / 4
}

// GetContextWindow returns messages that fit within the token limit
// Prioritizes recent messages and messages from active conversations
func (cm *ContextManager) GetContextWindow(activeConversations []*ConversationState) []Message {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if len(cm.messages) == 0 {
		return []Message{}
	}

	// Start with system prompt tokens
	estimatedTokens := EstimateTokens(cm.systemPrompt)

	// Reserve tokens for the response (MaxTokens in the API call)
	reservedTokens := 50
	availableTokens := cm.maxTokens - estimatedTokens - reservedTokens

	// Build context window from most recent messages going backward
	result := make([]Message, 0)

	// Always try to include at least the last 10 messages if they fit
	startIdx := len(cm.messages) - 1
	minMessages := 10
	if len(cm.messages) < minMessages {
		minMessages = len(cm.messages)
	}

	// First pass: add recent messages
	for i := startIdx; i >= 0 && len(result) < minMessages; i-- {
		msg := cm.messages[i]
		msgTokens := EstimateTokens(msg.Username + ": " + msg.Content)

		if estimatedTokens+msgTokens > availableTokens && len(result) >= 5 {
			// If we have at least 5 messages, stop
			break
		}

		result = append([]Message{msg}, result...)
		estimatedTokens += msgTokens
	}

	// Second pass: if we have room, add more context from active conversations
	activePlayerNames := make(map[string]bool)
	for _, conv := range activeConversations {
		activePlayerNames[conv.PlayerName] = true
	}

	// Go further back to find messages from active conversation participants
	furtherBackIdx := len(cm.messages) - len(result) - 1
	addedCount := 0
	maxAdditional := 20 // Don't add too many old messages

	for i := furtherBackIdx; i >= 0 && addedCount < maxAdditional; i-- {
		msg := cm.messages[i]

		// Only add if from active conversation participant
		if !activePlayerNames[msg.Username] && !msg.IsBot {
			continue
		}

		msgTokens := EstimateTokens(msg.Username + ": " + msg.Content)
		if estimatedTokens+msgTokens > availableTokens {
			break
		}

		result = append([]Message{msg}, result...)
		estimatedTokens += msgTokens
		addedCount++
	}

	return result
}

// BuildChatCompletionMessages converts messages to Groq API format
func (cm *ContextManager) BuildChatCompletionMessages(contextWindow []Message, prompt string) []groq.ChatCompletionMessage {
	messages := []groq.ChatCompletionMessage{
		{
			Role:    "system",
			Content: cm.systemPrompt,
		},
	}

	// Build chat history context
	chatHistory := "Recent chat history:\n"
	for _, msg := range contextWindow {
		chatHistory += msg.Username + ": " + msg.Content + "\n"
	}

	chatHistory += "\n" + prompt

	messages = append(messages, groq.ChatCompletionMessage{
		Role:    "user",
		Content: chatHistory,
	})

	return messages
}

// GetRecentMessages returns the N most recent messages
func (cm *ContextManager) GetRecentMessages(n int) []Message {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	if len(cm.messages) == 0 {
		return []Message{}
	}

	start := 0
	if len(cm.messages) > n {
		start = len(cm.messages) - n
	}

	result := make([]Message, len(cm.messages)-start)
	copy(result, cm.messages[start:])
	return result
}

// Clear removes all messages from history
func (cm *ContextManager) Clear() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.messages = make([]Message, 0)
}

// PruneOldMessages removes messages older than the specified duration
func (cm *ContextManager) PruneOldMessages(maxAge time.Duration) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	cutoff := time.Now().Add(-maxAge)
	newMessages := make([]Message, 0)

	for _, msg := range cm.messages {
		if msg.Timestamp.After(cutoff) {
			newMessages = append(newMessages, msg)
		}
	}

	cm.messages = newMessages
}
