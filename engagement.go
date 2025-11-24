package main

import (
	"math/rand"
	"strings"
	"time"
)

// EngagementDecider determines when the bot should respond
type EngagementDecider struct {
	lastResponseTime    time.Time
	minResponseInterval time.Duration
	recentMessages      []Message
	config              *EngagementConfig
	rng                 *rand.Rand
}

type EngagementConfig struct {
	BaseProbability       float64 // Base probability of responding to general chat (0.0-1.0)
	MentionProbability    float64 // Probability when mentioned directly
	QuestionProbability   float64 // Probability when a question is detected
	MinMessagesSinceReply int     // Minimum messages before considering another reply
	MaxSilentMessages     int     // Maximum messages to stay silent before forced engagement
	MinTimeBetweenReplies time.Duration
	ConversationBoost     float64 // Boost to probability during active conversation
}

func NewEngagementDecider(config *EngagementConfig) *EngagementDecider {
	return &EngagementDecider{
		config:              config,
		minResponseInterval: config.MinTimeBetweenReplies,
		recentMessages:      make([]Message, 0),
		rng:                 rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// ShouldRespond determines if the bot should respond to the current context
func (ed *EngagementDecider) ShouldRespond(
	latestMessage Message,
	conversation *ConversationState,
	messagesSinceLastReply int,
	otherPlayers []string,
) (bool, string) {
	now := time.Now()

	// Check if this is a recent direct mention (within 30 seconds)
	isRecentMention := false
	if conversation != nil && conversation.IsActive {
		timeSinceMention := now.Sub(conversation.LastMentionTime)
		isRecentMention = timeSinceMention < 30*time.Second
	}

	// Don't respond too frequently (UNLESS directly mentioned)
	if !isRecentMention && now.Sub(ed.lastResponseTime) < ed.minResponseInterval {
		return false, "too soon since last response"
	}

	// Check if we've been silent too long (forced engagement)
	if messagesSinceLastReply >= ed.config.MaxSilentMessages {
		ed.lastResponseTime = now
		return true, "forced engagement - too many silent messages"
	}

	// Don't respond too quickly (UNLESS directly mentioned)
	if !isRecentMention && messagesSinceLastReply < ed.config.MinMessagesSinceReply {
		return false, "not enough messages since last reply"
	}

	// Direct mentions should always respond immediately
	if isRecentMention {
		ed.lastResponseTime = now
		return true, "direct mention - responding immediately"
	}

	// Detect if player is talking to someone else
	if DetectConversationSwitch(latestMessage.Content, otherPlayers) {
		// Reduce conversation engagement when switching
		if conversation != nil {
			conversation.Engagement *= 0.5
		}
		return false, "player talking to someone else"
	}

	// Calculate response probability
	probability := ed.config.BaseProbability

	// Boost for active conversations (mentions already handled above)
	if conversation != nil && conversation.IsActive {
		// Active conversation boost
		probability += ed.config.ConversationBoost * conversation.Engagement
	}

	// Boost for questions
	if ed.isQuestion(latestMessage.Content) {
		probability = max(probability, ed.config.QuestionProbability)
	}

	// Additional context: if there's been rapid back-and-forth, increase engagement
	if ed.detectRapidConversation() {
		probability += 0.15
	}

	// Cap probability at 1.0
	if probability > 1.0 {
		probability = 1.0
	}

	// Roll the dice
	roll := ed.rng.Float64()
	shouldRespond := roll < probability

	if shouldRespond {
		ed.lastResponseTime = now
		return true, "engagement probability met"
	}

	return false, "engagement probability not met"
}

// isQuestion detects if a message is a question
func (ed *EngagementDecider) isQuestion(message string) bool {
	msg := strings.TrimSpace(message)

	// Check for question mark
	if strings.HasSuffix(msg, "?") {
		return true
	}

	// Check for question words at the start
	questionWords := []string{
		"who", "what", "when", "where", "why", "how",
		"is", "are", "can", "could", "would", "should",
		"do", "does", "did", "will", "has", "have",
	}

	msgLower := strings.ToLower(msg)
	for _, qw := range questionWords {
		if strings.HasPrefix(msgLower, qw+" ") {
			return true
		}
	}

	return false
}

// detectRapidConversation checks if there's been rapid back-and-forth
func (ed *EngagementDecider) detectRapidConversation() bool {
	if len(ed.recentMessages) < 3 {
		return false
	}

	// Check last 3 messages
	now := time.Now()
	count := 0
	for i := len(ed.recentMessages) - 1; i >= 0 && count < 3; i-- {
		msg := ed.recentMessages[i]
		if now.Sub(msg.Timestamp) < 10*time.Second {
			count++
		}
	}

	return count >= 3
}

// UpdateRecentMessages tracks recent messages for context
func (ed *EngagementDecider) UpdateRecentMessages(msg Message) {
	ed.recentMessages = append(ed.recentMessages, msg)

	// Keep only last 10 messages
	if len(ed.recentMessages) > 10 {
		ed.recentMessages = ed.recentMessages[1:]
	}
}

// GenerateResponsePrompt creates a contextual prompt for the LLM
func (ed *EngagementDecider) GenerateResponsePrompt(
	conversation *ConversationState,
	isDirectMention bool,
	recentMessages []Message,
) string {
	baseInstruction := "\n\nYou can use multiple lines if needed (separate with newlines). Keep responses natural and conversational."

	if isDirectMention {
		return "You were directly mentioned. Respond naturally to the conversation." + baseInstruction
	}

	if conversation != nil && conversation.IsActive && conversation.Engagement > 0.5 {
		return "You're in an active conversation. Respond naturally and appropriately to continue the discussion." + baseInstruction
	}

	// Check if last few messages contain questions
	hasQuestion := false
	for i := len(recentMessages) - 1; i >= 0 && i >= len(recentMessages)-3; i-- {
		if ed.isQuestion(recentMessages[i].Content) {
			hasQuestion = true
			break
		}
	}

	if hasQuestion {
		return "There's a question in the recent conversation. You can respond if it's relevant, or just add to the conversation naturally." + baseInstruction
	}

	return "Join the conversation naturally. Keep it casual and human-like." + baseInstruction
}

// Reset clears the engagement state
func (ed *EngagementDecider) Reset() {
	ed.lastResponseTime = time.Time{}
	ed.recentMessages = make([]Message, 0)
}

func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
