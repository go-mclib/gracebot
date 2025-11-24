package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/conneroisu/groq-go"
	"github.com/go-mclib/client/client"
)

type GraceBot struct {
	groqClient  *groq.Client
	botUsername string
	botAliases  []string
	ready       chan bool
	cancel      context.CancelFunc
	started     bool
	mu          sync.Mutex

	// New components
	contextManager      *ContextManager
	conversationManager *ConversationManager
	engagementDecider   *EngagementDecider

	// Tracking
	messagesSinceLastReply int
	knownPlayers           map[string]bool
	lastBotSendTime        time.Time
}

func NewGraceBot(groqClient *groq.Client, systemPrompt, botUsername string, botAliases []string) *GraceBot {
	return &GraceBot{
		groqClient:             groqClient,
		botUsername:            botUsername,
		botAliases:             botAliases,
		ready:                  make(chan bool, 1),
		contextManager:         NewContextManager(systemPrompt, MaxContextTokens),
		conversationManager:    NewConversationManager(botAliases),
		engagementDecider:      NewEngagementDecider(GetEngagementConfig()),
		messagesSinceLastReply: 0,
		knownPlayers:           make(map[string]bool),
	}
}

func (gb *GraceBot) Start(ctx context.Context, c *client.Client) {
	<-gb.ready
	log.Printf("GraceBot started - checking for engagement every %v", ChatCheckInterval)
	time.Sleep(InitialDelay)

	ticker := time.NewTicker(ChatCheckInterval)
	defer ticker.Stop()

	decayTicker := time.NewTicker(10 * time.Second)
	defer decayTicker.Stop()

	pruneTicker := time.NewTicker(5 * time.Minute)
	defer pruneTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("GraceBot stopped")
			return
		case <-ticker.C:
			gb.checkAndRespond(c)
		case <-decayTicker.C:
			gb.conversationManager.DecayEngagement(EngagementDecayRate, ConversationTimeout)
		case <-pruneTicker.C:
			gb.contextManager.PruneOldMessages(MaxMessageHistoryAge)
		}
	}
}

func (gb *GraceBot) checkAndRespond(c *client.Client) {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	recentMessages := gb.contextManager.GetRecentMessages(5)
	if len(recentMessages) == 0 {
		return
	}

	latestMessage := recentMessages[len(recentMessages)-1]

	// Skip if this is the bot's own message
	if latestMessage.IsBot {
		return
	}

	// Get or create conversation state
	conversation := gb.conversationManager.GetConversation(latestMessage.Username)

	// Build list of other players for context
	otherPlayers := make([]string, 0)
	for player := range gb.knownPlayers {
		if player != latestMessage.Username && player != gb.botUsername {
			otherPlayers = append(otherPlayers, player)
		}
	}

	// Decide if we should respond
	shouldRespond, reason := gb.engagementDecider.ShouldRespond(
		latestMessage,
		conversation,
		gb.messagesSinceLastReply,
		otherPlayers,
	)

	if !shouldRespond {
		log.Printf("Not responding: %s (msgs since reply: %d)", reason, gb.messagesSinceLastReply)
		return
	}

	log.Printf("Responding: %s (msgs since reply: %d)", reason, gb.messagesSinceLastReply)
	gb.sendResponse(c, conversation, latestMessage)
	gb.messagesSinceLastReply = 0
}

func (gb *GraceBot) sendResponse(c *client.Client, conversation *ConversationState, latestMessage Message) {
	// Get conversation context
	activeConversations := gb.conversationManager.GetActiveConversations()
	contextWindow := gb.contextManager.GetContextWindow(activeConversations)

	// Determine if this is a direct mention
	isDirectMention := false
	if conversation != nil {
		timeSinceMention := time.Since(conversation.LastMentionTime)
		isDirectMention = timeSinceMention < 30*time.Second
	}

	// Generate contextual prompt
	prompt := gb.engagementDecider.GenerateResponsePrompt(
		conversation,
		isDirectMention,
		contextWindow,
	)

	// Build messages for API
	messages := gb.contextManager.BuildChatCompletionMessages(contextWindow, prompt)

	// Call Groq API with higher token limit for multiline responses
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	response, err := gb.groqClient.ChatCompletion(ctx, groq.ChatCompletionRequest{
		Model:       groq.ModelLlama318BInstant,
		Messages:    messages,
		Temperature: 0.85,
		MaxTokens:   MaxResponseTokens,
	})

	if err != nil {
		log.Printf("Groq API error: %v", err)
		return
	}

	if len(response.Choices) == 0 {
		log.Printf("No response choices from API")
		return
	}

	aiResponse := response.Choices[0].Message.Content

	// Split response into lines and send with random delays
	gb.sendMultilineResponse(c, aiResponse)
}

// sendMultilineResponse splits a message into lines and sends each with a random delay
func (gb *GraceBot) sendMultilineResponse(c *client.Client, message string) {
	// Split by newlines
	lines := strings.Split(message, "\n")

	// Filter out empty lines and sanitize each line
	validLines := make([]string, 0)
	for _, line := range lines {
		sanitized := sanitizeChatMessage(line)
		if sanitized != "" {
			validLines = append(validLines, sanitized)
		}
	}

	if len(validLines) == 0 {
		log.Printf("No valid lines to send after sanitization")
		return
	}

	log.Printf("Sending %d-line response", len(validLines))

	// Send each line with random delay
	for i, line := range validLines {
		// Send the message
		if err := c.SendChatMessage(line); err != nil {
			log.Printf("Failed to send chat message (line %d): %v", i+1, err)
			continue
		}

		// Track bot's own message in history
		gb.contextManager.AddMessage(gb.botUsername, line, true)
		gb.lastBotSendTime = time.Now()

		log.Printf("Bot responded (line %d/%d): %s", i+1, len(validLines), line)

		// Wait random delay before next line (except after last line)
		if i < len(validLines)-1 {
			// Random delay between MultilineDelayMin and MultilineDelayMax
			minMs := int(MultilineDelayMin.Milliseconds())
			maxMs := int(MultilineDelayMax.Milliseconds())
			delayMs := minMs + gb.engagementDecider.rng.Intn(maxMs-minMs+1)
			delay := time.Duration(delayMs) * time.Millisecond
			log.Printf("Waiting %v before next line...", delay)
			time.Sleep(delay)
		}
	}
}

func (gb *GraceBot) AddMessage(username, content string) {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	// Filter out bot's own messages from the handler
	if gb.isBotMessage(username, content) {
		log.Printf("Filtered echo - user: %q", username)
		return
	}

	// Track known players
	gb.knownPlayers[username] = true

	// Add to context manager
	gb.contextManager.AddMessage(username, content, false)

	// Update conversation tracking
	gb.conversationManager.UpdateConversation(username, content, gb.botUsername)

	// Update engagement decider
	msg := Message{
		Username:  username,
		Content:   content,
		Timestamp: time.Now(),
		IsBot:     false,
	}
	gb.engagementDecider.UpdateRecentMessages(msg)

	// Increment message counter
	gb.messagesSinceLastReply++

	log.Printf("[%s] %s (msgs since reply: %d)", username, content, gb.messagesSinceLastReply)
}

func (gb *GraceBot) isBotMessage(username, content string) bool {
	// Check if username matches bot username (case-insensitive)
	if strings.EqualFold(username, gb.botUsername) {
		return true
	}

	// Check if it was sent very recently (within 1 second of bot sending)
	timeSinceLastSent := time.Since(gb.lastBotSendTime)
	if timeSinceLastSent < 1*time.Second {
		log.Printf("Message too close to bot send time (%v), likely echo", timeSinceLastSent)
		return true
	}

	return false
}

func (gb *GraceBot) Reset() {
	if gb.cancel != nil {
		log.Println("Server transfer detected, resetting GraceBot")
		gb.cancel()
	}

	gb.mu.Lock()
	gb.started = false
	gb.ready = make(chan bool, 1)
	gb.contextManager.Clear()
	gb.conversationManager = NewConversationManager(gb.botAliases)
	gb.engagementDecider.Reset()
	gb.messagesSinceLastReply = 0
	gb.knownPlayers = make(map[string]bool)
	gb.mu.Unlock()
}

func (gb *GraceBot) MarkReady() {
	if !gb.started {
		select {
		case gb.ready <- true:
			log.Println("First chat message received, GraceBot ready")
			gb.started = true
		default:
		}
	}
}

func (gb *GraceBot) StartWithCancel(c *client.Client) {
	ctx, cancel := context.WithCancel(context.Background())
	gb.cancel = cancel
	go gb.Start(ctx, c)
}
