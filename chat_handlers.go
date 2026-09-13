package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-mclib/client"
	chatmodule "github.com/go-mclib/client/modules/chat"
	"github.com/go-mclib/client/modules/playerlist"
	"github.com/go-mclib/data/packet_ids"
	"github.com/go-mclib/data/packets"
	jp "github.com/go-mclib/protocol/java_protocol"
)

func parseAliases(value string) []string {
	var aliases []string
	seen := make(map[string]bool)
	for _, alias := range strings.Split(value, ",") {
		alias = strings.ToLower(strings.TrimSpace(alias))
		if alias != "" && !seen[alias] {
			aliases = append(aliases, alias)
			seen[alias] = true
		}
	}
	return aliases
}

func mentioned(message, username string, aliases []string) bool {
	message = strings.ToLower(message)
	if username != "" && strings.Contains(message, strings.ToLower(username)) {
		return true
	}
	for _, alias := range aliases {
		if alias != "" && strings.Contains(message, alias) {
			return true
		}
	}
	return false
}

type chatRouter struct {
	assistant *assistant
	store     *playerStore
	aliases   []string
}

func (r *chatRouter) playerChat(ctx context.Context, username, sender, message string) {
	if ctx.Err() != nil || sender == "" || strings.EqualFold(sender, username) || strings.TrimSpace(message) == "" {
		return
	}
	r.store.recordChat(sender, message, time.Now())
	if !mentioned(message, username, r.aliases) {
		return
	}
	request := chatRequest{sender: sender, message: message, username: username, ctx: ctx}
	select {
	case r.assistant.requests <- request:
	case <-ctx.Done():
	default:
		// Bound backpressure so overload cannot indefinitely stall keepalives.
		replyCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		if err := r.assistant.send(replyCtx, sender+": I'm busy; please mention me again shortly."); err != nil {
			log.Printf("chat queue full; busy reply for %s failed: %v", sender, err)
		}
	}
}

func (r *chatRouter) handler(ctx context.Context, players *playerlist.Module) client.Handler {
	return func(c *client.Client, wire *jp.WirePacket) {
		if c.State() != jp.StatePlay {
			return
		}
		switch wire.PacketID {
		case packet_ids.S2CPlayerChatID:
			var packet packets.S2CPlayerChat
			if err := wire.ReadInto(&packet); err != nil {
				log.Printf("decode player chat: %v", err)
				return
			}
			// The signed sender UUID resolves to the profile name. Display names
			// are decorative and must never be used for tool authorization.
			player := players.GetPlayer([16]byte(packet.Sender))
			if player == nil {
				log.Printf("ignoring chat from unknown player UUID %s", packet.Sender)
				return
			}
			if packet.FilterMask.Type != packets.FilterMaskPassThrough {
				return
			}
			r.playerChat(ctx, c.Username, player.Name, string(packet.Body.Content))
		case packet_ids.S2CSystemChatID:
			var packet packets.S2CSystemChat
			if err := wire.ReadInto(&packet); err != nil {
				log.Printf("decode system chat: %v", err)
				return
			}
			if !bool(packet.Overlay) && strings.HasPrefix(packet.Content.Translate, "death.") && len(packet.Content.With) > 0 {
				r.store.recordDeath(packet.Content.With[0].String(), packet.Content.Translate)
			}
			// System, disguised and overlay messages never invoke the AI, even
			// if their rendered text contains a bot name or forged player name.
		}
	}
}

// Upstream chat signing/acknowledgement state is shared by reads and writes.
// Serialize access while the AI and lyrics run outside the packet loop.
type synchronizedChat struct {
	inner *chatmodule.Module
	ctx   context.Context
	mu    sync.Mutex
	ready bool
}

func (m *synchronizedChat) Name() string          { return m.inner.Name() }
func (m *synchronizedChat) Init(c *client.Client) { m.inner.Init(c) }
func (m *synchronizedChat) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ready = false
	m.inner.Reset()
}
func (m *synchronizedChat) HandlePacket(packet *jp.WirePacket) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inner.HandlePacket(packet)
}
func (m *synchronizedChat) SendMessage(message string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready || m.ctx.Err() != nil {
		return fmt.Errorf("Minecraft chat is disconnected")
	}
	return m.inner.SendMessage(message)
}
func (m *synchronizedChat) SendCommand(command string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.ready || m.ctx.Err() != nil {
		return fmt.Errorf("Minecraft chat is disconnected")
	}
	return m.inner.SendCommand(command)
}
func (m *synchronizedChat) SendChatSessionData() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inner.SendChatSessionData()
}

// One output path enforces the client's byte limit, valid UTF-8 and pacing.
type chatOutput struct {
	gate     chan struct{}
	mu       sync.RWMutex
	write    func(string) error
	last     time.Time
	interval time.Duration
}

func newChatOutput() *chatOutput {
	return &chatOutput{gate: make(chan struct{}, 1), interval: time.Second}
}
func (o *chatOutput) bind(write func(string) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.write = write
}
func (o *chatOutput) send(ctx context.Context, text string) error {
	select {
	case o.gate <- struct{}{}:
		defer func() { <-o.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, chunk := range chatChunks(text) {
		if delay := time.Until(o.last.Add(o.interval)); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		o.mu.RLock()
		write := o.write
		o.mu.RUnlock()
		if write == nil {
			return fmt.Errorf("Minecraft chat is disconnected")
		}
		if err := write(chunk); err != nil {
			return err
		}
		o.last = time.Now()
	}
	return nil
}

func chatChunks(text string) []string {
	text = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || r == '§' || r == utf8.RuneError {
			return -1
		}
		return r
	}, text)
	text = strings.TrimSpace(text)
	var chunks []string
	for len(text) > 256 {
		end := 256
		for !utf8.RuneStart(text[end]) {
			end--
		}
		if space := strings.LastIndexByte(text[:end], ' '); space > end/2 {
			end = space
		}
		chunks = append(chunks, text[:end])
		text = strings.TrimSpace(text[end:])
	}
	if text != "" {
		chunks = append(chunks, text)
	}
	return chunks
}
