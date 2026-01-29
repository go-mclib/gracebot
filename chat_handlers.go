package main

import (
	"log"
	"regexp"
	"strings"

	"github.com/go-mclib/client/client"
	packets "github.com/go-mclib/data/go/773/java_packets"
	jp "github.com/go-mclib/protocol/java_protocol"
)

// deathMessageRegex matches death messages like "death.attack.outOfWorld [HIPPO84]"
var deathMessageRegex = regexp.MustCompile(`^(death\.[a-zA-Z._]+) \[([^\]]+)\]`)

var llmManager *LLMManager
var conversationEnabled bool

// initChatHandlers initializes the chat handlers with the LLM manager
// manager can be nil if conversation mode is disabled
func initChatHandlers(manager *LLMManager) {
	llmManager = manager
	conversationEnabled = manager != nil
}

func graceChatHandler(c *client.Client, pkt *jp.Packet) {
	// handle player chat messages
	if pkt.PacketID == packets.S2CPlayerChat.PacketID {
		var d packets.S2CPlayerChatData
		if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
			sender := d.SenderName.GetText()
			if sender == c.Username {
				return
			}

			message := string(d.Message)
			if sender != "" && message != "" {
				// Track player activity and word counts
				UpdatePlayerActivity(sender)
				CountWords(sender, message)

				// Mode 1: Command handling (always active, ! prefix)
				if HandleCommand(c, sender, message) {
					return
				}

				// Check for wordle guesses (if game is active)
				if ProcessGuess(c, sender, message) {
					return
				}

				// Mode 2: Conversation mode (if enabled)
				if conversationEnabled && llmManager != nil {
					llmManager.AddMessage(sender, message, false)
				}
			}
		}
	}

	// handle system chat messages (join/leave/death etc.)
	if pkt.PacketID == packets.S2CSystemChat.PacketID {
		var d packets.S2CSystemChatData
		if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
			content := d.Content.GetText()
			// check for chat chain broken - triggers reconnect
			if strings.Contains(content, "chat.disabled.chain_broken") {
				log.Printf("Chat chain broken detected - triggering reconnect")
				go func() {
					if err := c.Disconnect(false); err != nil {
						log.Printf("Failed to disconnect for reconnect: %v", err)
					}
				}()
				return
			}

			// Check for death messages
			if matches := deathMessageRegex.FindStringSubmatch(content); matches != nil {
				player := matches[2]
				deathType := matches[1]
				AddDeath(player, deathType)
			}

			if content != "" && !shouldFilterMessage(content) {
				// Only forward system messages to LLM if conversation mode is enabled
				if conversationEnabled && llmManager != nil {
					llmManager.AddMessage("", content, true)
				}
			}
		}
	}
}

// shouldFilterMessage checks if a message should be filtered out
func shouldFilterMessage(message string) bool {
	filterKeywords := []string{
		"You received:",
		"Donate",
		"multiplayer.player.joined [GraceBot]",
	}
	for _, keyword := range filterKeywords {
		if strings.Contains(message, keyword) {
			return true
		}
	}
	return false
}
