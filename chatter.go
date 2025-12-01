package main

import (
	"context"
	"fmt"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/conneroisu/groq-go"
	"github.com/go-mclib/client/client"
)

const (
	debounceInterval = 5 * time.Second
)

var silenceMarkers = []string{"[silence]", "<silence>", "silence"}

type ChatMessage struct {
	Username  string
	Content   string
	Timestamp time.Time
	IsSystem  bool // true for join/leave/death, false for player chat
}

type GraceBot struct {
	groqClient   *groq.Client
	systemPrompt string
	botUsername  string
	botAliases   []string
	maxChatLog   int
	mcClient     *client.Client
	mu           sync.Mutex

	chatLog       []ChatMessage
	context       []groq.ChatCompletionMessage
	debounceTimer *time.Timer
	stopCh        chan struct{}
}

func NewGraceBot(groqClient *groq.Client, systemPrompt, botUsername string, botAliases []string, maxChatLog int) *GraceBot {
	gb := &GraceBot{
		groqClient:   groqClient,
		systemPrompt: systemPrompt,
		botUsername:  botUsername,
		botAliases:   botAliases,
		maxChatLog:   maxChatLog,
		chatLog:      make([]ChatMessage, 0, maxChatLog),
		context:      make([]groq.ChatCompletionMessage, 0),
		stopCh:       make(chan struct{}),
	}

	return gb
}

// SetClient sets the Minecraft client reference
func (gb *GraceBot) SetClient(c *client.Client) {
	gb.mcClient = c
}

// Stop stops the debounce timer
func (gb *GraceBot) Stop() {
	close(gb.stopCh)
	gb.mu.Lock()
	if gb.debounceTimer != nil {
		gb.debounceTimer.Stop()
	}
	gb.mu.Unlock()
}

// callChatCompletion is a generalized method for making chat completion requests
func (gb *GraceBot) callChatCompletion(ctx context.Context, messages []groq.ChatCompletionMessage) (*groq.ChatCompletionResponse, error) {
	tools := GetAllTools()

	response, err := gb.groqClient.ChatCompletion(ctx, groq.ChatCompletionRequest{
		Model:             "llama-3.3-70b-versatile",
		Messages:          messages,
		Temperature:       0.5,
		MaxTokens:         512,
		Tools:             tools,
		ToolChoice:        "auto",
		ParallelToolCalls: true,
	})

	return &response, err
}

// resetDebounceTimer resets or creates the debounce timer
func (gb *GraceBot) resetDebounceTimer() {
	if gb.debounceTimer != nil {
		gb.debounceTimer.Stop()
	}

	gb.debounceTimer = time.AfterFunc(debounceInterval, func() {
		gb.mu.Lock()
		defer gb.mu.Unlock()

		if len(gb.chatLog) > 0 {
			log.Printf("Debounce timer expired - sending %d accumulated messages", len(gb.chatLog))
			gb.respondToChat(false)
		}
	})
}

// AddMessage adds a message to the chat log and triggers response if needed
func (gb *GraceBot) AddMessage(username, content string, isSystem bool) {
	gb.mu.Lock()
	defer gb.mu.Unlock()

	// filter out bot's own messages
	if gb.isBotMessage(username) {
		log.Printf("Filtered bot echo - user: %q", username)
		return
	}

	msg := ChatMessage{
		Username:  username,
		Content:   content,
		Timestamp: time.Now(),
		IsSystem:  isSystem,
	}
	gb.chatLog = append(gb.chatLog, msg)

	// prevent unbounded growth - keep only recent messages if we exceed limit
	if len(gb.chatLog) > gb.maxChatLog {
		gb.chatLog = gb.chatLog[len(gb.chatLog)-gb.maxChatLog:]
	}

	isMentioned := gb.isMentioned(content)
	log.Printf("[%s] %s (chat log: %d, mentioned: %v)", username, content, len(gb.chatLog), isMentioned)

	// if mentioned, respond immediately
	if isMentioned {
		log.Printf("Bot mentioned - responding immediately")
		// stop any pending debounce timer since we're responding now
		if gb.debounceTimer != nil {
			gb.debounceTimer.Stop()
		}
		gb.respondToChat(true)
	} else {
		// not mentioned - reset debounce timer to wait for more messages
		log.Printf("Message added - resetting debounce timer (%v)", debounceInterval)
		gb.resetDebounceTimer()
	}
}

func (gb *GraceBot) isMentioned(content string) bool {
	contentLower := strings.ToLower(content)

	if strings.Contains(contentLower, strings.ToLower(gb.botUsername)) {
		return true
	}
	for _, alias := range gb.botAliases {
		if strings.Contains(contentLower, alias) {
			return true
		}
	}

	return false
}

func (gb *GraceBot) isBotMessage(username string) bool {
	return strings.EqualFold(username, gb.botUsername)
}

// respondToChat sends the chat log to the LLM and responds if needed
func (gb *GraceBot) respondToChat(forcedResponse bool) {
	if len(gb.chatLog) == 0 {
		return
	}

	messages := gb.buildAPIMessages(forcedResponse)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	log.Printf("Sending %d chat messages + %d context messages to LLM (forced: %v)", len(gb.chatLog), len(gb.context), forcedResponse)

	response, err := gb.callChatCompletion(ctx, messages)
	if err != nil {
		log.Printf("Groq API error: %v", err)
		// on error, clear chat log anyway to avoid getting stuck
		gb.clearChatLog()
		return
	}

	if len(response.Choices) == 0 {
		log.Printf("No response choices from API")
		gb.clearChatLog()
		return
	}

	choice := response.Choices[0]

	// Check if LLM wants to use a tool
	if len(choice.Message.ToolCalls) > 0 {
		log.Printf("LLM requested %d tool call(s)", len(choice.Message.ToolCalls))
		gb.handleToolCalls(ctx, messages, choice.Message)
		return
	}

	aiResponse := strings.TrimSpace(choice.Message.Content)

	// if LLM decided not to respond (empty or special token), just clear log
	if aiResponse == "" || slices.Contains(silenceMarkers, aiResponse) {
		log.Printf("LLM decided not to respond")
		gb.clearChatLog()
		return
	}

	gb.addChatLogToContext()

	gb.mu.Unlock()
	gb.SendResponseToMinecraft(aiResponse)
	gb.mu.Lock()
}

// handleToolCalls executes tool calls and gets final response from LLM
func (gb *GraceBot) handleToolCalls(ctx context.Context, messages []groq.ChatCompletionMessage, assistantMessage groq.ChatCompletionMessage) {
	messages = append(messages, assistantMessage)

	// exec
	for _, toolCall := range assistantMessage.ToolCalls {
		log.Printf("Executing tool: %s with args: %s", toolCall.Function.Name, toolCall.Function.Arguments)

		result, err := ExecuteTool(toolCall.Function.Name, toolCall.Function.Arguments, gb.mcClient)
		if err != nil {
			log.Printf("Tool execution error: %v", err)
			result = fmt.Sprintf("Error: %v", err)
		} else {
			log.Printf("Tool result: %s", result)
		}

		// add tool result to messages
		messages = append(messages, groq.ChatCompletionMessage{
			Role:       groq.RoleTool,
			Content:    result,
			ToolCallID: toolCall.ID,
		})
	}

	// get final response from LLM with tool results
	response, err := gb.callChatCompletion(ctx, messages)
	if err != nil {
		log.Printf("Groq API error after tool call: %v", err)
		gb.clearChatLog()
		return
	}

	if len(response.Choices) == 0 {
		log.Printf("No response choices from API after tool call")
		gb.clearChatLog()
		return
	}

	aiResponse := strings.TrimSpace(response.Choices[0].Message.Content)

	// if LLM decided not to respond, just clear log
	if aiResponse == "" || slices.Contains(silenceMarkers, aiResponse) {
		log.Printf("LLM decided not to respond after tool call")
		gb.clearChatLog()
		return
	}

	gb.addChatLogToContext()

	gb.mu.Unlock()
	gb.SendResponseToMinecraft(aiResponse)
	gb.mu.Lock()
}

// buildAPIMessages builds the messages array for the Groq API
func (gb *GraceBot) buildAPIMessages(forcedResponse bool) []groq.ChatCompletionMessage {
	messages := []groq.ChatCompletionMessage{
		{
			Role:    groq.RoleSystem,
			Content: gb.buildSystemPrompt(forcedResponse),
		},
	}

	messages = append(messages, gb.context...)

	chatLogText := gb.formatChatLog()
	messages = append(messages, groq.ChatCompletionMessage{
		Role:    groq.RoleUser,
		Content: chatLogText,
	})

	return messages
}

func (gb *GraceBot) buildSystemPrompt(forcedResponse bool) string {
	prompt := gb.systemPrompt
	if forcedResponse {
		prompt += "\n\nYou were directly mentioned in the latest message. You MUST respond."
	} else {
		prompt += "\n\nHere are recent chat messages. If you have nothing to say, output: `" + strings.Join(silenceMarkers, "` or `") + "` (no quotes)."
	}

	return prompt
}

func (gb *GraceBot) formatChatLog() string {
	var lines []string
	for _, msg := range gb.chatLog {
		if msg.IsSystem {
			lines = append(lines, fmt.Sprintf("[SYSTEM] %s", msg.Content))
		} else {
			lines = append(lines, fmt.Sprintf("[%s] %s", msg.Username, msg.Content))
		}
	}
	return strings.Join(lines, "\n")
}

func (gb *GraceBot) addChatLogToContext() {
	chatLogText := gb.formatChatLog()
	gb.context = append(gb.context, groq.ChatCompletionMessage{
		Role:    groq.RoleUser,
		Content: chatLogText,
	})

	// keep context limited to last 20 messages (10 exchanges)
	if len(gb.context) > 20 {
		gb.context = gb.context[len(gb.context)-20:]
	}
}

func (gb *GraceBot) clearChatLog() {
	gb.chatLog = make([]ChatMessage, 0, gb.maxChatLog)
}

// SendResponseToMinecraft sends multi-line response with typing delays
func (gb *GraceBot) SendResponseToMinecraft(aiResponse string) {
	if gb.mcClient == nil {
		log.Printf("Minecraft client not set, cannot send response")
		return
	}

	lines := strings.Split(aiResponse, "\n")
	validLines := make([]string, 0)
	for _, line := range lines {
		sanitized := sanitizeChatMessage(line)
		if sanitized != "" {
			validLines = append(validLines, sanitized)
		}
	}

	if len(validLines) == 0 {
		log.Printf("No valid lines to send after sanitization")
		gb.clearChatLog()
		return
	}

	log.Printf("Sending %d-line response", len(validLines))

	// send each line with delay proportional to message length
	for i, line := range validLines {
		if err := gb.mcClient.SendChatMessage(line); err != nil {
			log.Printf("Failed to send chat message (line %d): %v", i+1, err)
			continue
		}

		gb.context = append(gb.context, groq.ChatCompletionMessage{
			Role:    groq.RoleAssistant,
			Content: line,
		})

		log.Printf("Bot responded (line %d/%d): %s", i+1, len(validLines), line)

		// wait
		if i < len(validLines)-1 {
			// base delay: 10ms per character, min 500ms, max 3000ms
			delayMs := min(max(len(line)*10, 500), 3000)
			delay := time.Duration(delayMs) * time.Millisecond
			log.Printf("Waiting %v before next line...", delay)
			time.Sleep(delay)
		}
	}

	gb.clearChatLog()
}

func sanitizeChatMessage(message string) string {
	// trim whitespace
	message = strings.TrimSpace(message)

	// limit to 255 characters (Minecraft limit)
	if len(message) > 255 {
		message = message[:255]
	}

	return message
}
