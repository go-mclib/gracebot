package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

//go:embed system_prompt.txt
var systemPrompt string

const (
	maxToolRounds    = 6
	maxCallsPerTurn  = 16
	maxHistoryTurns  = 8
	maxConversations = 128
)

type chatCompletionClient interface {
	CreateChatCompletion(context.Context, openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error)
}

type chatRequest struct {
	sender, message, username string
	ctx                       context.Context
}

type conversation struct {
	turns [][]openai.ChatCompletionMessage
	used  time.Time
}

type assistant struct {
	client             chatCompletionClient
	model, personality string
	tools              *toolRegistry
	requests           chan chatRequest
	send               func(context.Context, string) error
	history            map[string]*conversation
}

func newAssistant(client chatCompletionClient, model, personality string, tools *toolRegistry, send func(context.Context, string) error) *assistant {
	return &assistant{client: client, model: model, personality: personality, tools: tools,
		requests: make(chan chatRequest, 64), send: send, history: make(map[string]*conversation)}
}

// One worker keeps game mutations and each complete tool exchange ordered.
func (a *assistant) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case request := <-a.requests:
			if ctx.Err() != nil {
				return
			}
			if request.ctx.Err() != nil {
				continue
			}
			turnCtx, cancel := context.WithTimeout(request.ctx, 60*time.Second)
			response, err := a.respond(turnCtx, request)
			cancel()
			if err != nil {
				log.Printf("AI response for %s: %v", request.sender, err)
				response = "I couldn't finish that request. Please try again; any actions already completed are unchanged."
			}
			if request.ctx.Err() == nil {
				if err := a.send(request.ctx, request.sender+": "+response); err != nil {
					log.Printf("chat reply: %v", err)
				}
			}
		}
	}
}

func (a *assistant) respond(ctx context.Context, request chatRequest) (string, error) {
	key := strings.ToLower(request.sender)
	conv := a.history[key]
	if conv == nil {
		if len(a.history) >= maxConversations {
			var oldest string
			for name, entry := range a.history {
				if oldest == "" || entry.used.Before(a.history[oldest].used) {
					oldest = name
				}
			}
			delete(a.history, oldest)
		}
		conv = &conversation{}
		a.history[key] = conv
	}
	conv.used = time.Now()
	identity, _ := json.Marshal(map[string]string{"bot_username": request.username, "requesting_player": request.sender})
	messages := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem,
		Content: systemPrompt + "\n\n" + a.personality + "\n\nRuntime identity: " + string(identity)}}
	for _, turn := range conv.turns {
		messages = append(messages, turn...)
	}
	start := len(messages)
	messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: request.message})
	calls := 0
	for round := range maxToolRounds + 1 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		req := openai.ChatCompletionRequest{Model: a.model, Messages: messages, MaxTokens: 400, Tools: a.tools.definitions}
		if round == maxToolRounds || calls >= maxCallsPerTurn {
			req.ToolChoice = "none"
		}
		resp, err := a.client.CreateChatCompletion(ctx, req)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", fmt.Errorf("AI returned no choices")
		}
		message := resp.Choices[0].Message
		message.Role = openai.ChatMessageRoleAssistant
		if len(message.ToolCalls) == 0 {
			text := strings.TrimSpace(message.Content)
			if text == "" {
				text = "I'm here. What would you like to do?"
			}
			message.Content = text
			messages = append(messages, message)
			conv.turns = append(conv.turns, append([]openai.ChatCompletionMessage(nil), messages[start:]...))
			if len(conv.turns) > maxHistoryTurns {
				conv.turns[0] = nil
				conv.turns = conv.turns[1:]
			}
			return text, nil
		}
		if round == maxToolRounds || calls+len(message.ToolCalls) > maxCallsPerTurn {
			return "", fmt.Errorf("AI exceeded tool-call limit")
		}
		// Validate IDs before performing effects; providers require a result for every call.
		ids := make(map[string]bool, len(message.ToolCalls))
		for _, call := range message.ToolCalls {
			if call.ID == "" || ids[call.ID] {
				return "", fmt.Errorf("AI returned invalid tool-call IDs")
			}
			ids[call.ID] = true
		}
		messages = append(messages, message)
		for _, call := range message.ToolCalls {
			result := a.tools.execute(ctx, request.sender, call)
			messages = append(messages, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: call.ID, Content: result})
			calls++
		}
	}
	return "", fmt.Errorf("AI did not finish its response")
}
