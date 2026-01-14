package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/go-mclib/client/client"
	"github.com/sashabaranov/go-openai"
)

// defineLLMTools returns the tool declarations for OpenAI-compatible API (Groq)
func defineLLMTools() []openai.Tool {
	return []openai.Tool{
		{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        "run_command",
				Description: "Execute a Minecraft command on the server. Commands should be sent without the leading slash. For example, to get help, use 'help' not '/help'.",
				Parameters: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"command": map[string]any{
							"type":        "string",
							"description": "The command to execute (without leading slash)",
						},
					},
					"required": []string{"command"},
				},
			},
		},
	}
}

// executeTool handles tool execution and returns the result
func executeTool(c *client.Client, toolCall openai.ToolCall) string {
	switch toolCall.Function.Name {
	case "run_command":
		// Parse the JSON arguments
		var args struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(toolCall.Function.Arguments), &args); err != nil {
			return fmt.Sprintf("Error parsing arguments: %v", err)
		}

		log.Printf("[Tool] Executing command: /%s", args.Command)
		if err := c.SendCommand(args.Command); err != nil {
			return fmt.Sprintf("Error executing command: %v", err)
		}
		return fmt.Sprintf("Command '/%s' executed successfully", args.Command)

	default:
		return fmt.Sprintf("Unknown tool: %s", toolCall.Function.Name)
	}
}
