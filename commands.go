package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/go-mclib/client/client"
)

// CommandHandler is the function signature for command handlers
type CommandHandler func(c *client.Client, sender string, args []string)

// Command represents a registered command
type Command struct {
	Name        string
	Description string
	Usage       string
	Handler     CommandHandler
}

// CommandRegistry holds all registered commands
type CommandRegistry struct {
	commands map[string]*Command
	mu       sync.RWMutex
}

// Global command registry
var commandRegistry = &CommandRegistry{
	commands: make(map[string]*Command),
}

// RegisterCommand adds a command to the registry
func RegisterCommand(name, description, usage string, handler CommandHandler) {
	commandRegistry.mu.Lock()
	defer commandRegistry.mu.Unlock()

	commandRegistry.commands[strings.ToLower(name)] = &Command{
		Name:        name,
		Description: description,
		Usage:       usage,
		Handler:     handler,
	}
}

// GetCommand retrieves a command by name
func GetCommand(name string) *Command {
	commandRegistry.mu.RLock()
	defer commandRegistry.mu.RUnlock()

	return commandRegistry.commands[strings.ToLower(name)]
}

// GetAllCommands returns all registered commands sorted by name
func GetAllCommands() []*Command {
	commandRegistry.mu.RLock()
	defer commandRegistry.mu.RUnlock()

	commands := make([]*Command, 0, len(commandRegistry.commands))
	for _, cmd := range commandRegistry.commands {
		commands = append(commands, cmd)
	}

	sort.Slice(commands, func(i, j int) bool {
		return commands[i].Name < commands[j].Name
	})

	return commands
}

// HandleCommand processes an incoming command message
// Returns true if a command was handled, false otherwise
func HandleCommand(c *client.Client, sender, message string) bool {
	if !strings.HasPrefix(message, "!") {
		return false
	}

	// Parse command and arguments
	parts := strings.Fields(message[1:]) // Remove the "!" prefix
	if len(parts) == 0 {
		return false
	}

	cmdName := strings.ToLower(parts[0])
	args := parts[1:]

	cmd := GetCommand(cmdName)
	if cmd == nil {
		c.SendChatMessage(fmt.Sprintf("Unknown command: !%s. Use !help for available commands.", cmdName))
		return true
	}

	cmd.Handler(c, sender, args)
	return true
}

// RegisterDefaultCommands registers the built-in commands
func RegisterDefaultCommands() {
	// Help command
	RegisterCommand("help", "Shows available commands", "!help [command]", helpCommand)
}

// helpCommand displays help information
func helpCommand(c *client.Client, sender string, args []string) {
	if len(args) > 0 {
		// Show help for specific command
		cmd := GetCommand(args[0])
		if cmd == nil {
			c.SendChatMessage(fmt.Sprintf("Unknown command: %s", args[0]))
			return
		}
		msg := fmt.Sprintf("!%s - %s", cmd.Name, cmd.Description)
		if cmd.Usage != "" {
			msg += fmt.Sprintf(" | Usage: %s", cmd.Usage)
		}
		c.SendChatMessage(msg)
		return
	}

	// Show all commands in one message
	commands := GetAllCommands()
	var parts []string
	for _, cmd := range commands {
		parts = append(parts, fmt.Sprintf("!%s - %s", cmd.Name, cmd.Description))
	}
	c.SendChatMessage("Commands: " + strings.Join(parts, " | "))
}
