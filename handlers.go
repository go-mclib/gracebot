package main

import (
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
				if sender != "" && message != "" && !shouldFilterMessage(message) {
					chatter.AddMessage(sender, message, false)
				}
			}
		}

		// handle system chat messages (join/leave/death etc.)
		if pkt.PacketID == packets.S2CSystemChat.PacketID {
			var d packets.S2CSystemChatData
			if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
				content := d.Content.GetText()
				if content != "" && !shouldFilterMessage(content) {
					chatter.AddMessage("Server", content, true)
				}
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
