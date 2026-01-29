package main

import (
	"fmt"
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

// Global client for commands that need it
var mcClient *client.Client

// SetClient sets the global client for all commands
func SetClient(c *client.Client) {
	mcClient = c
}

// GetClient returns the global client
func GetClient() *client.Client {
	return mcClient
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

func init() {
	// Everyone can use these
	RegisterCommand("help", "Shows available commands", "!help [command]", nil, helpCommand)
	RegisterCommand("stop", "Stop any ongoing activity", "!stop", nil, stopCommand)
	RegisterCommand("rank", "Check player ranks", "!rank [player]", nil, rankCommand)
	RegisterCommand("playtime", "Check player playtime", "!playtime [player]", nil, playtimeCommand)
	RegisterCommand("seen", "When was a player last seen", "!seen [player]", nil, seenCommand)
	RegisterCommand("wordcount", "Count word usage", "!wordcount <word> [player]", nil, wordcountCommand)
	RegisterCommand("deaths", "Show death count", "!deaths [player]", nil, deathsCommand)

	// Admin only
	RegisterCommand("addrank", "Add rank to player", "!addrank <player> <rank>", []string{"admin"}, addRankCommand)
	RegisterCommand("delrank", "Remove rank from player", "!delrank <player> <rank>", []string{"admin"}, delRankCommand)
}

// stopCommand stops any ongoing activity (wordle, singing, etc.)
func stopCommand(c *client.Client, sender string, args []string) {
	stopped := false
	denied := false

	// Try to stop wordle
	if ok, permDenied := StopWordle(c, sender); ok {
		stopped = true
	} else if permDenied {
		denied = true
	}

	// Try to stop singing
	if ok, permDenied := StopSinging(sender); ok {
		stopped = true
	} else if permDenied {
		denied = true
	}

	if denied && !stopped {
		c.SendChatMessage("You don't have permission to stop that.")
		return
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

// playtimeCommand shows player playtime
func playtimeCommand(c *client.Client, sender string, args []string) {
	target := sender
	if len(args) > 0 {
		target = args[0]
	}

	if !PlayerExists(target) {
		c.SendChatMessage(fmt.Sprintf("%s has no recorded data.", target))
		return
	}

	playtime := GetPlaytime(target)
	c.SendChatMessage(fmt.Sprintf("%s playtime: %s", target, formatDuration(playtime)))
}

// seenCommand shows when a player was last seen
func seenCommand(c *client.Client, sender string, args []string) {
	target := sender
	if len(args) > 0 {
		target = args[0]
	}

	lastSeen := GetLastSeen(target)
	if lastSeen.IsZero() {
		c.SendChatMessage(fmt.Sprintf("%s has never been seen.", target))
		return
	}

	ago := time.Since(lastSeen)
	c.SendChatMessage(fmt.Sprintf("%s was last seen %s ago", target, formatDuration(ago)))
}

// wordcountCommand shows word usage count
func wordcountCommand(c *client.Client, sender string, args []string) {
	if len(args) < 1 {
		c.SendChatMessage("Usage: !wordcount <word> [player]")
		return
	}

	word := args[0]
	var player string
	if len(args) > 1 {
		player = args[1]
	}

	count := GetWordCount(word, player)

	if player != "" {
		c.SendChatMessage(fmt.Sprintf("%s has said '%s' %d times", player, word, count))
	} else {
		c.SendChatMessage(fmt.Sprintf("'%s' has been said %d times globally", word, count))
	}
}

// deathsCommand shows death count for a player or top deaths
func deathsCommand(c *client.Client, sender string, args []string) {
	if len(args) > 0 {
		// Show deaths for specific player
		player := args[0]
		count := GetDeathCount(player)
		if count == 0 {
			c.SendChatMessage(fmt.Sprintf("%s has no recorded deaths.", player))
			return
		}
		c.SendChatMessage(fmt.Sprintf("%s has died %d times", player, count))
		return
	}

	// Show top deaths
	top := GetTopDeaths(5)
	if len(top) == 0 {
		c.SendChatMessage("No deaths recorded yet!")
		return
	}

	var parts []string
	for _, p := range top {
		parts = append(parts, fmt.Sprintf("%s: %d", p.Player, p.Count))
	}
	c.SendChatMessage("Top deaths: " + strings.Join(parts, ", "))
}

// formatDuration formats a duration in a human readable way
func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)

	days := int(d.Hours() / 24)
	hours := int(d.Hours()) % 24
	minutes := int(d.Minutes()) % 60
	seconds := int(d.Seconds()) % 60

	if days > 0 {
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	}
	if hours > 0 {
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, seconds)
	}
	if minutes > 0 {
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	}
	return fmt.Sprintf("%ds", seconds)
}
