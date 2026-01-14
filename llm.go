package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-mclib/client/client"
	"github.com/sashabaranov/go-openai"
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

	groqModel   = "llama-3.3-70b-versatile"
	groqBaseURL = "https://api.groq.com/openai/v1"

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
	client              *openai.Client
	chatBuffer          []ChatMessage
	lastProcessedIndex  int                          // Index of last processed message
	conversationHistory []openai.ChatCompletionMessage // Persistent conversation history
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

	// Get API key from environment
	apiKey := os.Getenv("GROQ_API_KEY")
	if apiKey == "" {
		cancel()
		return nil, fmt.Errorf("GROQ_API_KEY environment variable not set")
	}

	// Create Groq client using OpenAI-compatible API
	config := openai.DefaultConfig(apiKey)
	config.BaseURL = groqBaseURL
	groqClient := openai.NewClientWithConfig(config)

	manager := &LLMManager{
		client:              groqClient,
		chatBuffer:          make([]ChatMessage, 0, maxChatBuffer),
		lastProcessedIndex:  0,
		conversationHistory: make([]openai.ChatCompletionMessage, 0),
		mcClient:            mcClient,
		ctx:                 ctx,
		cancel:              cancel,
	}

	// Initialize debounce timer (initially stopped)
	manager.debounceTimer = time.AfterFunc(chatProcessInterval, func() {
		manager.processChat()
	})
	manager.debounceTimer.Stop()

	log.Printf("[LLM] Manager initialized with model: %s", groqModel)
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

	// Build messages: system prompt + existing conversation history + new messages
	systemInstruction := systemPrompt + "\n\n" + personality

	m.bufferMutex.Lock()
	messages := make([]openai.ChatCompletionMessage, 0, len(m.conversationHistory)+2)

	// Add system message
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: systemInstruction,
	})

	// Add conversation history
	messages = append(messages, m.conversationHistory...)
	m.bufferMutex.Unlock()

	// Add new user messages
	messages = append(messages, openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: newMessagesText.String(),
	})

	// Create the request
	req := openai.ChatCompletionRequest{
		Model:       groqModel,
		Messages:    messages,
		Tools:       defineLLMTools(),
		MaxTokens:   300,
		Temperature: 0.9,
	}

	// Send the conversation with context
	resp, err := m.client.CreateChatCompletion(m.ctx, req)
	if err != nil {
		log.Printf("[LLM] Error: %v", err)
		return
	}

	// Process the response and update conversation history
	m.handleResponse(resp, messages)
}

// handleResponse processes the LLM response and handles tool calls
func (m *LLMManager) handleResponse(resp openai.ChatCompletionResponse, history []openai.ChatCompletionMessage) {
	if len(resp.Choices) == 0 {
		log.Printf("[LLM] No response choices")
		return
	}

	choice := resp.Choices[0]
	assistantMessage := choice.Message

	// Add assistant's response to history
	history = append(history, assistantMessage)

	// Check for tool calls
	if len(assistantMessage.ToolCalls) > 0 {
		// Execute tools and collect responses
		for _, toolCall := range assistantMessage.ToolCalls {
			log.Printf("[LLM] Tool call: %s", toolCall.Function.Name)

			// Execute the tool
			result := executeTool(m.mcClient, toolCall)

			// Add tool response to history
			history = append(history, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				Content:    result,
				ToolCallID: toolCall.ID,
			})
		}

		// Send tool results back to continue the conversation
		req := openai.ChatCompletionRequest{
			Model:       groqModel,
			Messages:    history,
			Tools:       defineLLMTools(),
			MaxTokens:   300,
			Temperature: 0.9,
		}

		toolResp, err := m.client.CreateChatCompletion(m.ctx, req)
		if err != nil {
			log.Printf("[LLM] Error sending tool response: %v", err)
			return
		}

		// Process the follow-up response
		m.handleResponse(toolResp, history)
		return
	}

	// Handle text response
	if assistantMessage.Content != "" {
		response := strings.TrimSpace(assistantMessage.Content)
		log.Printf("[LLM] Response: %s", response)

		// Check if the bot wants to stay silent
		if strings.Contains(response, stopWord) {
			log.Printf("[LLM] Bot chose to remain silent")
			// Still update conversation history even when silent
			m.updateConversationHistory(history)
			return
		}

		// Send the response to chat (split by lines if needed)
		m.sendChatResponse(response)

		// Update conversation history with successful interaction
		m.updateConversationHistory(history)
	}
}

// updateConversationHistory updates the conversation history (excluding system message)
func (m *LLMManager) updateConversationHistory(history []openai.ChatCompletionMessage) {
	m.bufferMutex.Lock()
	defer m.bufferMutex.Unlock()

	// Skip the system message (first message) when storing history
	if len(history) > 1 {
		m.conversationHistory = history[1:]
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
