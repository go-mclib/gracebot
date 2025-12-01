package main

import (
	"fmt"

	"github.com/conneroisu/groq-go/pkg/tools"
	"github.com/go-mclib/client/client"
)

// GetAllTools returns all available tools
func GetAllTools() []tools.Tool {
	return []tools.Tool{
		getWeatherTool(),
		getCommandTool(),
	}
}

// ExecuteTool executes a tool call and returns the result
func ExecuteTool(toolName string, arguments string, mcClient *client.Client) (string, error) {
	switch toolName {
	case "get_weather":
		return executeGetWeather(arguments)
	case "send_command":
		return executeSendCommand(arguments, mcClient)
	default:
		return "", fmt.Errorf("unknown tool: %s", toolName)
	}
}
