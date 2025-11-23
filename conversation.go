package main

import (
	"strings"
	"sync"
	"time"
)

// ConversationState tracks an ongoing conversation with a specific player
type ConversationState struct {
	PlayerName      string
	LastMentionTime time.Time
	LastMessageTime time.Time
	MessageCount    int
	IsActive        bool
	Engagement      float64 // 0.0 to 1.0, decays over time
}

// ConversationManager tracks all active conversations
type ConversationManager struct {
	conversations map[string]*ConversationState
	mu            sync.RWMutex
}

func NewConversationManager() *ConversationManager {
	return &ConversationManager{
		conversations: make(map[string]*ConversationState),
	}
}

// DetectMention checks if a message mentions the bot (case-insensitive)
func (cm *ConversationManager) DetectMention(message, botName string) bool {
	msgLower := strings.ToLower(message)
	botLower := strings.ToLower(botName)
	return strings.Contains(msgLower, botLower)
}

// UpdateConversation updates or creates a conversation state
func (cm *ConversationManager) UpdateConversation(playerName, message, botName string) *ConversationState {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	conv, exists := cm.conversations[playerName]
	if !exists {
		conv = &ConversationState{
			PlayerName: playerName,
			IsActive:   false,
			Engagement: 0.0,
		}
		cm.conversations[playerName] = conv
	}

	conv.LastMessageTime = time.Now()
	conv.MessageCount++

	// Check if this message mentions the bot
	if cm.DetectMention(message, botName) {
		conv.LastMentionTime = time.Now()
		conv.IsActive = true
		conv.Engagement = 1.0 // Maximum engagement when mentioned
	}

	return conv
}

// GetConversation retrieves a conversation state
func (cm *ConversationManager) GetConversation(playerName string) *ConversationState {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.conversations[playerName]
}

// DecayEngagement reduces engagement over time for inactive conversations
func (cm *ConversationManager) DecayEngagement(decayRate float64, timeout time.Duration) {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	now := time.Now()
	for _, conv := range cm.conversations {
		timeSinceMessage := now.Sub(conv.LastMessageTime)
		timeSinceMention := now.Sub(conv.LastMentionTime)

		// If conversation has timed out, deactivate it
		if timeSinceMention > timeout {
			conv.IsActive = false
		}

		// Decay engagement over time
		if timeSinceMessage > 5*time.Second {
			conv.Engagement *= decayRate
			if conv.Engagement < 0.01 {
				conv.Engagement = 0.0
			}
		}
	}
}

// GetActiveConversations returns all currently active conversations
func (cm *ConversationManager) GetActiveConversations() []*ConversationState {
	cm.mu.RLock()
	defer cm.mu.RUnlock()

	active := make([]*ConversationState, 0)
	for _, conv := range cm.conversations {
		if conv.IsActive && conv.Engagement > 0.1 {
			active = append(active, conv)
		}
	}
	return active
}

// ShouldTrackMessage determines if a message should extend an existing conversation
func (cm *ConversationManager) ShouldTrackMessage(playerName string, timeSinceLastMsg time.Duration) bool {
	conv := cm.GetConversation(playerName)
	if conv == nil {
		return false
	}

	// Track if conversation is active and message is within conversation window
	return conv.IsActive && timeSinceLastMsg < 30*time.Second
}

// DetectConversationSwitch detects if a player is now talking to someone else
// Returns true if the message appears to be addressing another player
func DetectConversationSwitch(message string, otherPlayers []string) bool {
	msgLower := strings.ToLower(message)

	// Check if message contains another player's name
	for _, player := range otherPlayers {
		if player != "" && strings.Contains(msgLower, strings.ToLower(player)) {
			return true
		}
	}

	// Check for common patterns of addressing someone else
	addressPatterns := []string{
		"hey ", "hi ", "hello ", "@",
	}
	for _, pattern := range addressPatterns {
		if strings.HasPrefix(msgLower, pattern) {
			return true
		}
	}

	return false
}
