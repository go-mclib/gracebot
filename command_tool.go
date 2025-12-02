package main

import (
	"encoding/json"
	"fmt"

	openrouter "github.com/revrost/go-openrouter"
	"github.com/go-mclib/client/client"
)

func getCommandTool() openrouter.Tool {
	return openrouter.Tool{
		Type: openrouter.ToolTypeFunction,
		Function: &openrouter.FunctionDefinition{
			Name:        "send_command",
			Description: "Execute a Minecraft command as the bot. Use this to perform server commands like teleporting, giving items, changing game rules, etc. The command should not include the leading '/' character.",
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"command": map[string]interface{}{
						"type":        "string",
						"description": "The Minecraft command to execute (without the leading '/'). Examples: 'tp @s ~ ~10 ~', 'gamemode creative', 'time set day'",
					},
				},
				"required": []string{"command"},
			},
		},
	}
}

func executeSendCommand(arguments string, mcClient *client.Client) (string, error) {
	// parse arguments
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("failed to parse arguments: %w", err)
	}

	if args.Command == "" {
		return "", fmt.Errorf("command is required")
	}

	if mcClient == nil {
		return "", fmt.Errorf("minecraft client not available")
	}

	// send the command using the client's SendCommand method
	if err := mcClient.SendCommand(args.Command); err != nil {
		return "", fmt.Errorf("failed to send command: %w", err)
	}

	return fmt.Sprintf("Successfully executed command: /%s", args.Command), nil
}
