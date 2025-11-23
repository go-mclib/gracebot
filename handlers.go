package main

import (
	"strings"

	"github.com/go-mclib/client/client"
	packets "github.com/go-mclib/data/go/773/java_packets"
	jp "github.com/go-mclib/protocol/java_protocol"
)

func shouldFilterMessage(message string) bool {
	filterKeywords := []string{"Discord", "Donate", "You received:"}
	for _, keyword := range filterKeywords {
		if strings.Contains(message, keyword) {
			return true
		}
	}
	return false
}

func registerChatHandler(c *client.Client, chatter *GraceBot) {
	c.RegisterHandler(func(c *client.Client, pkt *jp.Packet) {
		if pkt.PacketID == packets.S2CPlayerChat.PacketID {
			chatter.MarkReady()

			var d packets.S2CPlayerChatData
			if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
				sender := d.SenderName.GetText()
				message := string(d.Message)
				if sender != "" && message != "" && !shouldFilterMessage(message) {
					chatter.AddMessage(sender, message)
				}
			}
		}

		if pkt.PacketID == packets.S2CSystemChat.PacketID {
			chatter.MarkReady()

			var d packets.S2CSystemChatData
			if err := jp.BytesToPacketData(pkt.Data, &d); err == nil {
				content := d.Content.GetText()
				if content != "" && !shouldFilterMessage(content) {
					chatter.AddMessage("Server", content)
				}
			}
		}
	})
}

func registerTransferHandler(c *client.Client, chatter *GraceBot) {
	c.RegisterHandler(func(c *client.Client, pkt *jp.Packet) {
		if pkt.PacketID == packets.S2CStartConfiguration.PacketID {
			chatter.Reset()
			chatter.StartWithCancel(c)
		}
	})
}
