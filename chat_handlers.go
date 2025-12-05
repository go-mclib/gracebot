package main

import (
	"log"
	"strings"

	"github.com/go-mclib/client/client"
	packets "github.com/go-mclib/data/go/773/java_packets"
	jp "github.com/go-mclib/protocol/java_protocol"
)

var llmManager *LLMManager

// initChatHandlers initializes the chat handlers with the LLM manager
func initChatHandlers(manager *LLMManager) {
	llmManager = manager
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
				if llmManager != nil {
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

			if content != "" && !shouldFilterMessage(content) {
				if llmManager != nil {
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
