package main

import (
	"net"
	"strconv"
	"strings"
)

func sanitizeChatMessage(msg string) string {
	var result strings.Builder
	for _, r := range msg {
		if r >= 32 && r != 127 && r != '\n' && r != '\r' && r != '\t' {
			result.WriteRune(r)
		} else if r == '\n' || r == '\r' || r == '\t' {
			result.WriteRune(' ')
		}
	}

	cleaned := strings.TrimSpace(result.String())
	cleaned = strings.Join(strings.Fields(cleaned), " ")

	if len(cleaned) > MaxMessageLen {
		cleaned = cleaned[:MaxMessageLen-3] + "..."
	}

	return cleaned
}

func parseAddr(addr string) (string, uint16) {
	host := addr
	port := uint16(25565)
	if h, p, err := net.SplitHostPort(addr); err == nil {
		host = h
		if convP, err := strconv.ParseUint(p, 10, 16); err == nil {
			port = uint16(convP)
		}
	}
	return host, port
}
