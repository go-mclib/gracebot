package main

import (
	"fmt"

	openrouter "github.com/revrost/go-openrouter"
	"github.com/go-mclib/client/client"
)

// GetAllTools returns all available tools
func GetAllTools() []openrouter.Tool {
	return []openrouter.Tool{
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
