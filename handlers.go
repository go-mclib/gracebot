package main

import (
	"log"
	"strings"

	"github.com/go-mclib/client/client"
	packets "github.com/go-mclib/data/go/773/java_packets"
	jp "github.com/go-mclib/protocol/java_protocol"
)

func registerChatHandler(c *client.Client, chatter *GraceBot) {
	c.RegisterHandler(func(c *client.Client, pkt *jp.Packet) {
		// handle player chat messages
		if pkt.PacketID == packets.S2CPlayerChat.PacketID {
			var d packets.S2CPlayerChatData
			if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
				sender := d.SenderName.GetText()
				message := string(d.Message)
				log.Printf("DEBUG: PlayerChat packet - sender=%q, message=%q", sender, message)
				if sender != "" && message != "" && !shouldFilterMessage(message) {
					chatter.AddMessage(sender, message, false)
				} else {
					log.Printf("DEBUG: PlayerChat filtered - empty=%v, filtered=%v", sender == "" || message == "", shouldFilterMessage(message))
				}
			} else {
				log.Printf("DEBUG: PlayerChat parse error: %v", err)
			}
		}

		// handle system chat messages (join/leave/death etc.)
		if pkt.PacketID == packets.S2CSystemChat.PacketID {
			var d packets.S2CSystemChatData
			if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
				content := d.Content.GetText()
				log.Printf("DEBUG: SystemChat packet - content=%q", content)

				// check for chat chain broken - triggers reconnect
				if strings.Contains(content, "chat.disabled.chain_broken") {
					log.Printf("Chat chain broken detected - triggering reconnect (preserving context)")
					go func() {
						if err := c.Disconnect(false); err != nil {
							log.Printf("Failed to disconnect for reconnect: %v", err)
						}
					}()
					return
				}

				if content != "" && !shouldFilterMessage(content) {
					chatter.AddMessage("Server", content, true)
				} else {
					log.Printf("DEBUG: SystemChat filtered - empty=%v, filtered=%v", content == "", shouldFilterMessage(content))
				}
			} else {
				log.Printf("DEBUG: SystemChat parse error: %v", err)
			}
		}
	})
}

// shouldFilterMessage checks if a message should be filtered out
func shouldFilterMessage(message string) bool {
	filterKeywords := []string{
		"You have received:",
		"Donate",
	}
	for _, keyword := range filterKeywords {
		if strings.Contains(message, keyword) {
			return true
		}
	}
	return false
}
