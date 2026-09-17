package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	openai "github.com/sashabaranov/go-openai"
)

func newChatCompletionClient(cfg config) chatCompletionClient {
	base, err := url.Parse(cfg.apiBase)
	if err == nil && strings.EqualFold(base.Hostname(), "api.anthropic.com") {
		return newAnthropicClient(cfg.apiKey, cfg.apiBase)
	}
	apiConfig := openai.DefaultConfig(cfg.apiKey)
	apiConfig.BaseURL = cfg.apiBase
	return &openAIClient{client: openai.NewClientWithConfig(apiConfig)}
}

type openAIClient struct {
	client *openai.Client
}

func (c *openAIClient) CreateChatCompletion(ctx context.Context, req completionRequest) (openai.ChatCompletionResponse, error) {
	return c.client.CreateChatCompletion(ctx, req.ChatCompletionRequest)
}

type anthropicClient struct {
	client anthropic.Client
}

func newAnthropicClient(apiKey, baseURL string) chatCompletionClient {
	// The OpenAI-compatible configuration ends in /v1, but the native SDK
	// appends v1/messages itself. Retain any preceding proxy path.
	if base, err := url.Parse(baseURL); err == nil {
		base.Path = strings.TrimSuffix(strings.TrimRight(base.Path, "/"), "/v1")
		base.RawPath = ""
		baseURL = base.String()
	}
	return &anthropicClient{client: anthropic.NewClient(
		option.WithAPIKey(apiKey), option.WithBaseURL(baseURL), option.WithMaxRetries(0),
	)}
}

func (c *anthropicClient) CreateChatCompletion(ctx context.Context, req completionRequest) (openai.ChatCompletionResponse, error) {
	params, err := anthropicRequest(req)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	response, err := c.client.Messages.New(ctx, params)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	message := openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant}
	var text strings.Builder
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			text.WriteString(block.Text)
		case "tool_use":
			message.ToolCalls = append(message.ToolCalls, openai.ToolCall{
				ID: block.ID, Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{Name: block.Name, Arguments: string(block.Input)},
			})
		}
	}
	message.Content = text.String()
	finish := openai.FinishReasonStop
	switch response.StopReason {
	case anthropic.StopReasonToolUse:
		finish = openai.FinishReasonToolCalls
	case anthropic.StopReasonMaxTokens, anthropic.StopReasonModelContextWindowExceeded:
		finish = openai.FinishReasonLength
	case anthropic.StopReasonRefusal:
		finish = openai.FinishReasonContentFilter
	}
	promptTokens := int(response.Usage.InputTokens + response.Usage.CacheCreationInputTokens + response.Usage.CacheReadInputTokens)
	outputTokens := int(response.Usage.OutputTokens)
	return openai.ChatCompletionResponse{
		ID: response.ID, Object: "chat.completion", Model: string(response.Model),
		Choices: []openai.ChatCompletionChoice{{Index: 0, Message: message, FinishReason: finish}},
		Usage: openai.Usage{
			PromptTokens: promptTokens, CompletionTokens: outputTokens, TotalTokens: promptTokens + outputTokens,
			PromptTokensDetails: &openai.PromptTokensDetails{CachedTokens: int(response.Usage.CacheReadInputTokens)},
		},
	}, nil
}

func anthropicRequest(req completionRequest) (anthropic.MessageNewParams, error) {
	cache := anthropic.NewCacheControlEphemeralParam()
	params := anthropic.MessageNewParams{
		Model: anthropic.Model(req.Model), MaxTokens: int64(req.MaxTokens),
		CacheControl: cache,
		Messages:     make([]anthropic.MessageParam, 0, len(req.Messages)),
		Tools:        make([]anthropic.ToolUnionParam, 0, len(req.Tools)),
	}
	for _, tool := range req.Tools {
		if tool.Type != openai.ToolTypeFunction || tool.Function == nil {
			return params, fmt.Errorf("unsupported Anthropic tool type %q", tool.Type)
		}
		function := tool.Function
		definition := anthropic.ToolParam{
			Name: function.Name, Description: anthropic.String(function.Description),
			InputSchema: param.Override[anthropic.ToolInputSchemaParam](function.Parameters),
		}
		params.Tools = append(params.Tools, anthropic.ToolUnionParam{OfTool: &definition})
	}
	if len(params.Tools) > 0 {
		params.Tools[len(params.Tools)-1].OfTool.CacheControl = cache
	}
	choice, err := anthropicToolChoice(req.ToolChoice)
	if err != nil {
		return params, err
	}
	params.ToolChoice = choice

	prefix := true
	for i, message := range req.Messages {
		if prefix && message.Role == openai.ChatMessageRoleSystem {
			if message.Content != "" {
				block := anthropic.TextBlockParam{Text: message.Content}
				// The first nonempty system block is the static instructions.
				// A history boundary here may also cache the stable identity.
				if len(params.System) == 0 || i+1 == req.historyEnd {
					block.CacheControl = cache
				}
				params.System = append(params.System, block)
			}
			continue
		}
		prefix = false
		role := anthropic.MessageParamRoleUser
		blocks := make([]anthropic.ContentBlockParamUnion, 0, 1+len(message.ToolCalls))
		switch message.Role {
		case openai.ChatMessageRoleTool:
			result := anthropic.ToolResultBlockParam{ToolUseID: message.ToolCallID}
			// An empty result still acknowledges its call, but empty text
			// blocks are invalid. Native tool_result content is optional.
			if message.Content != "" {
				result.Content = []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: message.Content}}}
			}
			blocks = append(blocks, anthropic.ContentBlockParamUnion{OfToolResult: &result})
		case openai.ChatMessageRoleAssistant, openai.ChatMessageRoleUser, openai.ChatMessageRoleSystem:
			if message.Role == openai.ChatMessageRoleAssistant {
				role = anthropic.MessageParamRoleAssistant
			}
			// Late system messages are runtime metadata (including moderation
			// outcomes). Keep them in chronological order rather than hoisting
			// them ahead of cached history; native messages have no system role.
			if message.Content != "" {
				blocks = append(blocks, anthropic.NewTextBlock(message.Content))
			}
			for _, call := range message.ToolCalls {
				blocks = append(blocks, anthropic.NewToolUseBlock(call.ID, json.RawMessage(call.Function.Arguments), call.Function.Name))
			}
		default:
			return params, fmt.Errorf("unsupported Anthropic message role %q", message.Role)
		}
		if len(blocks) == 0 {
			continue
		}
		if i+1 == req.historyEnd {
			*blocks[len(blocks)-1].GetCacheControl() = cache
		}
		// In particular, all results from a parallel tool batch must be in
		// the same user message immediately following the assistant calls.
		if n := len(params.Messages); n > 0 && params.Messages[n-1].Role == role {
			params.Messages[n-1].Content = append(params.Messages[n-1].Content, blocks...)
		} else {
			params.Messages = append(params.Messages, anthropic.MessageParam{Role: role, Content: blocks})
		}
	}
	return params, nil
}

func anthropicToolChoice(choice any) (anthropic.ToolChoiceUnionParam, error) {
	switch choice {
	case nil:
		return anthropic.ToolChoiceUnionParam{}, nil
	case "required":
		return anthropic.ToolChoiceUnionParam{OfAny: &anthropic.ToolChoiceAnyParam{}}, nil
	case "auto":
		return anthropic.ToolChoiceUnionParam{OfAuto: &anthropic.ToolChoiceAutoParam{}}, nil
	case "none":
		return anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}, nil
	}
	// GraceBot's forced choice is an OpenAI function object. Decoding the
	// shared JSON shape also accepts the SDK's typed ToolChoice value.
	raw, err := json.Marshal(choice)
	if err != nil {
		return anthropic.ToolChoiceUnionParam{}, fmt.Errorf("encode Anthropic tool choice: %w", err)
	}
	var forced openai.ToolChoice
	if err := json.Unmarshal(raw, &forced); err != nil {
		return anthropic.ToolChoiceUnionParam{}, fmt.Errorf("decode Anthropic tool choice: %w", err)
	}
	if forced.Type != openai.ToolTypeFunction || forced.Function.Name == "" {
		return anthropic.ToolChoiceUnionParam{}, fmt.Errorf("unsupported Anthropic tool choice")
	}
	return anthropic.ToolChoiceUnionParam{OfTool: &anthropic.ToolChoiceToolParam{Name: forced.Function.Name}}, nil
}
