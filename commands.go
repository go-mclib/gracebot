package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-mclib/client/client"
)

// CommandHandler is the function signature for command handlers
type CommandHandler func(c *client.Client, sender string, args []string)

// Command represents a registered command
type Command struct {
	Name         string
	Description  string
	Usage        string
	AllowedRanks []string // Empty or contains "default" = everyone can use
	Handler      CommandHandler
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
// ranks can be nil/empty for commands anyone can use
func RegisterCommand(name, description, usage string, ranks []string, handler CommandHandler) {
	commandRegistry.mu.Lock()
	defer commandRegistry.mu.Unlock()

	// If no ranks specified, default to everyone
	if len(ranks) == 0 {
		ranks = []string{"default"}
	}

	commandRegistry.commands[strings.ToLower(name)] = &Command{
		Name:         name,
		Description:  description,
		Usage:        usage,
		AllowedRanks: ranks,
		Handler:      handler,
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

// CanUseCommand checks if a player can use a command based on ranks
func CanUseCommand(sender string, cmd *Command) bool {
	for _, rank := range cmd.AllowedRanks {
		if HasRank(sender, rank) {
			return true
		}
	}
	return false
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

	// Check permissions
	if !CanUseCommand(sender, cmd) {
		c.SendChatMessage(fmt.Sprintf("You don't have permission to use !%s", cmd.Name))
		return true
	}

	cmd.Handler(c, sender, args)
	return true
}

// RegisterDefaultCommands registers the built-in commands
func RegisterDefaultCommands() {
	// Everyone can use these
	RegisterCommand("help", "Shows available commands", "!help [command]", nil, helpCommand)
	RegisterCommand("stop", "Stop any ongoing activity", "!stop", nil, stopCommand)
	RegisterCommand("rank", "Check player ranks", "!rank [player]", nil, rankCommand)
	RegisterCommand("define", "Define a word", "!define <word>", nil, defineCommand)

	// Admin only
	RegisterCommand("addrank", "Add rank to player", "!addrank <player> <rank>", []string{"admin"}, addRankCommand)
	RegisterCommand("delrank", "Remove rank from player", "!delrank <player> <rank>", []string{"admin"}, delRankCommand)
}

// stopCommand stops any ongoing activity (wordle, singing, etc.)
func stopCommand(c *client.Client, sender string, args []string) {
	stopped := false

	// Try to stop wordle
	if StopWordle(c) {
		stopped = true
	}

	// Try to stop singing
	if StopSinging() {
		stopped = true
	}

	if !stopped {
		c.SendChatMessage("Nothing to stop.")
	}
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

	// Show all commands the user can access in one message
	commands := GetAllCommands()
	var parts []string
	for _, cmd := range commands {
		if CanUseCommand(sender, cmd) {
			parts = append(parts, fmt.Sprintf("!%s", cmd.Name))
		}
	}
	c.SendChatMessage("Commands: " + strings.Join(parts, ", "))
}

// rankCommand shows player ranks
func rankCommand(c *client.Client, sender string, args []string) {
	target := sender
	if len(args) > 0 {
		target = args[0]
	}

	ranks := GetRanks(target)
	c.SendChatMessage(fmt.Sprintf("%s ranks: %s", target, strings.Join(ranks, ", ")))
}

// addRankCommand adds a rank to a player (admin only)
func addRankCommand(c *client.Client, sender string, args []string) {
	if len(args) < 2 {
		c.SendChatMessage("Usage: !addrank <player> <rank>")
		return
	}

	player := args[0]
	rank := args[1]

	if err := AddRank(player, rank); err != nil {
		c.SendChatMessage(fmt.Sprintf("Error: %s", err.Error()))
		return
	}

	c.SendChatMessage(fmt.Sprintf("Added rank '%s' to %s", rank, player))
}

// delRankCommand removes a rank from a player (admin only)
func delRankCommand(c *client.Client, sender string, args []string) {
	if len(args) < 2 {
		c.SendChatMessage("Usage: !delrank <player> <rank>")
		return
	}

	player := args[0]
	rank := args[1]

	if err := RemoveRank(player, rank); err != nil {
		c.SendChatMessage(fmt.Sprintf("Error: %s", err.Error()))
		return
	}

	c.SendChatMessage(fmt.Sprintf("Removed rank '%s' from %s", rank, player))
}

// dictionaryResponse represents the response from dictionary API
type dictionaryResponse struct {
	Word     string `json:"word"`
	Meanings []struct {
		PartOfSpeech string `json:"partOfSpeech"`
		Definitions  []struct {
			Definition string `json:"definition"`
		} `json:"definitions"`
	} `json:"meanings"`
}

// defineCommand looks up a word definition
func defineCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Usage: !define <word>")
		return
	}

	word := strings.ToLower(args[0])
	apiURL := fmt.Sprintf("https://api.dictionaryapi.dev/api/v2/entries/en/%s", url.QueryEscape(word))

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(apiURL)
	if err != nil {
		c.SendChatMessage("Failed to look up definition.")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		c.SendChatMessage(fmt.Sprintf("No definition found for '%s'", word))
		return
	}

	if resp.StatusCode != http.StatusOK {
		c.SendChatMessage("Failed to look up definition.")
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		c.SendChatMessage("Failed to read definition.")
		return
	}

	var results []dictionaryResponse
	if err := json.Unmarshal(body, &results); err != nil || len(results) == 0 {
		c.SendChatMessage("Failed to parse definition.")
		return
	}

	// Get first definition
	result := results[0]
	if len(result.Meanings) == 0 || len(result.Meanings[0].Definitions) == 0 {
		c.SendChatMessage(fmt.Sprintf("No definition found for '%s'", word))
		return
	}

	partOfSpeech := result.Meanings[0].PartOfSpeech
	definition := result.Meanings[0].Definitions[0].Definition

	// Truncate if too long
	if len(definition) > 200 {
		definition = definition[:197] + "..."
	}

	c.SendChatMessage(fmt.Sprintf("%s (%s): %s", word, partOfSpeech, definition))
}
