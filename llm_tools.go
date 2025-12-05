package main

import (
	"fmt"
	"log"

	"github.com/go-mclib/client/client"
	"google.golang.org/genai"
)

// defineLLMTools returns the tool declarations for Gemini API
func defineLLMTools() []*genai.Tool {
	return []*genai.Tool{
		{
			FunctionDeclarations: []*genai.FunctionDeclaration{
				{
					Name:        "run_command",
					Description: "Execute a Minecraft command on the server. Commands should be sent without the leading slash. For example, to get help, use 'help' not '/help'.",
					Parameters: &genai.Schema{
						Type: genai.TypeObject,
						Properties: map[string]*genai.Schema{
							"command": {
								Type:        genai.TypeString,
								Description: "The command to execute (without leading slash)",
							},
						},
						Required: []string{"command"},
					},
				},
			},
		},
	}
}

// executeTool handles tool execution and returns the result
func executeTool(c *client.Client, toolCall *genai.FunctionCall) string {
	switch toolCall.Name {
	case "run_command":
		command, ok := toolCall.Args["command"].(string)
		if !ok {
			return "Error: command parameter must be a string"
		}

		log.Printf("[Tool] Executing command: /%s", command)
		if err := c.SendCommand(command); err != nil {
			return fmt.Sprintf("Error executing command: %v", err)
		}
		return fmt.Sprintf("Command '/%s' executed successfully", command)

	default:
		return fmt.Sprintf("Unknown tool: %s", toolCall.Name)
	}
}
