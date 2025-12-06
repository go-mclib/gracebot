package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/go-mclib/client/client"
	"google.golang.org/genai"
)

var (
	//go:embed personality.txt
	personality string

	//go:embed system_prompt.txt
	systemPrompt string
)

// Configuration variables
const (
	// Maximum number of messages to keep in buffer
	maxChatBuffer = 30

	// Interval to process accumulated messages
	chatProcessInterval = 5 * time.Second

	// Word that indicates bot doesn't want to reply
	stopWord = "[silence]"

	geminiModel = "gemini-flash-lite-latest"

	// Typing simulation for human-like responses
	msPerCharacter    = 150  // Milliseconds per character (simulates ~20 chars/second typing speed)
	minMessageDelay   = 1500 // Minimum delay between messages (ms)
	maxMessageDelay   = 5000 // Maximum delay between messages (ms)
)

// ChatMessage represents a chat message
type ChatMessage struct {
	Sender   string
	Message  string
	IsSystem bool
}

// LLMManager manages the LLM interactions
type LLMManager struct {
	client              *genai.Client
	chatBuffer          []ChatMessage
	lastProcessedIndex  int // Index of last processed message
	conversationHistory []*genai.Content // Persistent conversation history
	bufferMutex         sync.Mutex
	mcClient            *client.Client
	ctx                 context.Context
	cancel              context.CancelFunc
	debounceTimer       *time.Timer
	timerMutex          sync.Mutex
}

// NewLLMManager creates a new LLM manager
func NewLLMManager(mcClient *client.Client) (*LLMManager, error) {
	ctx, cancel := context.WithCancel(context.Background())

	// Create Gemini client (reads GEMINI_API_KEY from environment)
	genaiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create Gemini client: %w", err)
	}

	manager := &LLMManager{
		client:              genaiClient,
		chatBuffer:          make([]ChatMessage, 0, maxChatBuffer),
		lastProcessedIndex:  0,
		conversationHistory: make([]*genai.Content, 0),
		mcClient:            mcClient,
		ctx:                 ctx,
		cancel:              cancel,
	}

	// Initialize debounce timer (initially stopped)
	manager.debounceTimer = time.AfterFunc(chatProcessInterval, func() {
		manager.processChat()
	})
	manager.debounceTimer.Stop()

	log.Printf("[LLM] Manager initialized with model: %s", geminiModel)
	return manager, nil
}

// AddMessage adds a message to the chat buffer
func (m *LLMManager) AddMessage(sender, message string, isSystem bool) {
	m.bufferMutex.Lock()

	// Add the new message
	m.chatBuffer = append(m.chatBuffer, ChatMessage{
		Sender:   sender,
		Message:  message,
		IsSystem: isSystem,
	})

	// Trim buffer if it exceeds max size
	if len(m.chatBuffer) > maxChatBuffer {
		trimCount := len(m.chatBuffer) - maxChatBuffer
		m.chatBuffer = m.chatBuffer[trimCount:]
		// CRITICAL: Adjust lastProcessedIndex when trimming
		m.lastProcessedIndex -= trimCount
		if m.lastProcessedIndex < 0 {
			m.lastProcessedIndex = 0
		}
	}

	log.Printf("[LLM] Buffer: %d/%d messages (last processed: %d)", len(m.chatBuffer), maxChatBuffer, m.lastProcessedIndex)
	m.bufferMutex.Unlock()

	// Reset debounce timer
	m.timerMutex.Lock()
	m.debounceTimer.Reset(chatProcessInterval)
	m.timerMutex.Unlock()
}

// startChatProcessor is no longer needed - we use debounce timer now
// Keeping this as a placeholder in case we need background tasks later
func (m *LLMManager) startChatProcessor() {
	// Timer-based debouncing is handled in AddMessage()
	// This goroutine just waits for context cancellation
	<-m.ctx.Done()
}

// processChat sends accumulated messages to the LLM and handles responses
func (m *LLMManager) processChat() {
	m.bufferMutex.Lock()

	// Check if there are new messages to process
	if len(m.chatBuffer) == 0 || m.lastProcessedIndex >= len(m.chatBuffer) {
		m.bufferMutex.Unlock()
		return
	}

	// Get only NEW messages since last processing
	newMessages := m.chatBuffer[m.lastProcessedIndex:]
	messagesToProcess := make([]ChatMessage, len(newMessages))
	copy(messagesToProcess, newMessages)

	// Update the last processed index
	m.lastProcessedIndex = len(m.chatBuffer)

	// Trim buffer if it exceeds max size (keep recent history)
	if len(m.chatBuffer) > maxChatBuffer {
		trimCount := len(m.chatBuffer) - maxChatBuffer
		m.chatBuffer = m.chatBuffer[trimCount:]
		m.lastProcessedIndex -= trimCount
		if m.lastProcessedIndex < 0 {
			m.lastProcessedIndex = 0
		}
	}

	m.bufferMutex.Unlock()

	// Build the new messages string
	var newMessagesText strings.Builder
	for _, msg := range messagesToProcess {
		if msg.IsSystem {
			newMessagesText.WriteString(fmt.Sprintf("[SYSTEM] %s\n", msg.Message))
		} else {
			newMessagesText.WriteString(fmt.Sprintf("<%s> %s\n", msg.Sender, msg.Message))
		}
	}

	log.Printf("[LLM] Processing %d new messages...", len(messagesToProcess))

	// Create the request
	systemInstruction := systemPrompt + "\n\n" + personality
	config := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: systemInstruction}},
		},
		Tools:           defineLLMTools(),
		MaxOutputTokens: 300,
		Temperature:     genai.Ptr(float32(0.9)),
	}

	// Build contents: existing conversation history + new messages
	m.bufferMutex.Lock()
	contents := make([]*genai.Content, len(m.conversationHistory))
	copy(contents, m.conversationHistory)
	m.bufferMutex.Unlock()

	// Add new messages
	contents = append(contents, &genai.Content{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			{Text: newMessagesText.String()},
		},
	})

	// Send the conversation with context
	resp, err := m.client.Models.GenerateContent(m.ctx, geminiModel, contents, config)
	if err != nil {
		log.Printf("[LLM] Error: %v", err)
		return
	}

	// Process the response and update conversation history
	m.handleResponse(resp, contents, config)
}

// handleResponse processes the LLM response and handles tool calls
func (m *LLMManager) handleResponse(resp *genai.GenerateContentResponse, history []*genai.Content, config *genai.GenerateContentConfig) {
	if resp == nil || len(resp.Candidates) == 0 {
		log.Printf("[LLM] No response candidates")
		return
	}

	candidate := resp.Candidates[0]
	if candidate.Content == nil {
		log.Printf("[LLM] No content in response")
		return
	}

	// Add assistant's response to history
	history = append(history, candidate.Content)

	// Check for tool calls and text responses
	var toolCalls []*genai.FunctionCall
	var textResponse string

	for _, part := range candidate.Content.Parts {
		if part.FunctionCall != nil {
			toolCalls = append(toolCalls, part.FunctionCall)
		}
		if part.Text != "" {
			textResponse += part.Text
		}
	}

	// Handle tool calls first
	if len(toolCalls) > 0 {
		// Execute tools and collect responses
		functionResponses := make([]*genai.Part, 0, len(toolCalls))

		for _, funcCall := range toolCalls {
			log.Printf("[LLM] Tool call: %s", funcCall.Name)

			// Execute the tool
			result := executeTool(m.mcClient, funcCall)

			// Create function response
			functionResponses = append(functionResponses, &genai.Part{
				FunctionResponse: &genai.FunctionResponse{
					Name: funcCall.Name,
					Response: map[string]any{
						"result": result,
					},
				},
			})
		}

		// Add function responses to history
		history = append(history, &genai.Content{
			Role:  genai.RoleUser,
			Parts: functionResponses,
		})

		// Send tool results back to continue the conversation
		toolResp, err := m.client.Models.GenerateContent(m.ctx, geminiModel, history, config)
		if err != nil {
			log.Printf("[LLM] Error sending tool response: %v", err)
			return
		}

		// Process the follow-up response
		m.handleResponse(toolResp, history, config)
		return
	}

	// Handle text response
	if textResponse != "" {
		response := strings.TrimSpace(textResponse)
		log.Printf("[LLM] Response: %s", response)

		// Check if the bot wants to stay silent
		if strings.Contains(response, stopWord) {
			log.Printf("[LLM] Bot chose to remain silent")
			// Still update conversation history even when silent
			m.bufferMutex.Lock()
			m.conversationHistory = history
			m.bufferMutex.Unlock()
			return
		}

		// Send the response to chat (split by lines if needed)
		m.sendChatResponse(response)

		// Update conversation history with successful interaction
		m.bufferMutex.Lock()
		m.conversationHistory = history
		m.bufferMutex.Unlock()
	}
}

// sendChatResponse sends the LLM response to Minecraft chat
func (m *LLMManager) sendChatResponse(response string) {
	// Split response into lines (Minecraft chat has character limits)
	for line := range strings.SplitSeq(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// ensure line doesn't exceed Minecraft chat limit (256 chars)
		if len(line) > 255 {
			// split long lines
			for len(line) > 255 {
				chunk := line[:255]
				m.mcClient.SendChatMessage(chunk)
				line = line[255:]

				// human-like delay based on chunk length
				delay := calculateTypingDelay(len(chunk))
				time.Sleep(delay)
			}
		}

		if line != "" {
			log.Printf("[LLM] Sending: %s", line)
			m.mcClient.SendChatMessage(line)

			// human-like delay based on message length
			delay := calculateTypingDelay(len(line))
			time.Sleep(delay)
		}
	}
}

// calculateTypingDelay calculates a human-like delay based on message length
// Simulates typing speed with min/max bounds
func calculateTypingDelay(messageLength int) time.Duration {
	// Calculate delay: length * ms per character
	delayMs := messageLength * msPerCharacter

	// Clamp between min and max
	if delayMs < minMessageDelay {
		delayMs = minMessageDelay
	} else if delayMs > maxMessageDelay {
		delayMs = maxMessageDelay
	}

	return time.Duration(delayMs) * time.Millisecond
}

// Shutdown gracefully shuts down the LLM manager
func (m *LLMManager) Shutdown() {
	log.Printf("[LLM] Shutting down...")

	// Stop the debounce timer
	m.timerMutex.Lock()
	m.debounceTimer.Stop()
	m.timerMutex.Unlock()

	// Process any remaining messages
	m.processChat()

	m.cancel()
}
