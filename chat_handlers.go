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
	"github.com/go-mclib/client/modules/registry"
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
	assistant    *assistant
	store        *playerStore
	aliases      []string
	journal      *chatHistory
	observations *observations
	commands     *serverCommands
	actions      *actionManager
	consoleChat  bool
}

func (r *chatRouter) playerChat(ctx context.Context, username, sender, message string) {
	if ctx.Err() != nil || !validUsername(sender) || strings.EqualFold(sender, username) || strings.TrimSpace(message) == "" {
		return
	}
	at := time.Now()
	eventID, err := r.journal.append("player", sender, message, at)
	if err != nil {
		log.Printf("record player chat: %v", err)
	}
	r.enqueue(ctx, username, sender, message, eventID, at, mentioned(message, username, r.aliases))
}

func (r *chatRouter) enqueue(ctx context.Context, username, sender, text string, id uint64, at time.Time, mention bool) {
	r.assistant.engagements.enqueue(chatRequest{sender: sender, username: username, ctx: ctx, mentioned: mention,
		messages: []chatFragment{{ID: id, At: at, Text: text}}})
}

func chatTypeName(c *client.Client, bound packets.ChatTypeBound) string {
	if reg := registry.From(c); reg != nil {
		if entry, ok := reg.EntryByIndex("minecraft:chat_type", int(bound.ChatType)); ok {
			return entry.ID
		}
	}
	return ""
}

func privateChatType(name string) bool {
	return name == "minecraft:msg_command_incoming" || name == "minecraft:msg_command_outgoing" ||
		name == "minecraft:team_msg_command_incoming" || name == "minecraft:team_msg_command_outgoing"
}

func publicChatType(name string) bool {
	return name == "minecraft:chat" || name == "minecraft:say_command" || name == "minecraft:emote_command"
}

func (r *chatRouter) captureFeedback(text string, at time.Time) bool {
	return r.commands != nil && r.commands.observe(text, at)
}

func (r *chatRouter) handler(ctx context.Context, players *playerlist.Module) client.Handler {
	return func(c *client.Client, wire *jp.WirePacket) {
		if c.State() != jp.StatePlay {
			return
		}
		switch wire.PacketID {
		case packet_ids.S2CPlayerInfoUpdateID, packet_ids.S2CPlayerInfoRemoveID:
			profiles := players.GetAllPlayers()
			names := make([]string, 0, len(profiles))
			for _, player := range profiles {
				if player.Name != "" {
					names = append(names, player.Name)
				}
			}
			if err := r.store.recordPresence(names, time.Now()); err != nil {
				log.Printf("record presence: %v", err)
			}
		case packet_ids.S2CPlayerChatID:
			var packet packets.S2CPlayerChat
			if err := wire.ReadInto(&packet); err != nil {
				log.Printf("decode player chat: %v", err)
				return
			}
			if packet.FilterMask.Type != packets.FilterMaskPassThrough {
				return
			}
			// The signed sender UUID resolves to the profile name. Display names
			// are decorative and must never be used for tool authorization.
			typeName := chatTypeName(c, packet.ChatType)
			isPrivate := privateChatType(typeName) || packet.ChatType.TargetName.Present
			player := players.GetPlayer([16]byte(packet.Sender))
			if player == nil {
				kind := "disguised"
				if isPrivate || !publicChatType(typeName) {
					kind = "private_disguised"
				}
				r.rememberContext(kind, "", string(packet.Body.Content))
				log.Printf("retained chat from unresolved player UUID %s without granting authority", packet.Sender)
				return
			}
			if isPrivate {
				r.rememberContext("private", player.Name, string(packet.Body.Content))
				return
			}
			// Paper uses paper:raw for renderer-decorated player chat.
			// Accept it only with a resolved player UUID, never as server feedback.
			if !publicChatType(typeName) && typeName != "paper:raw" {
				r.rememberInternal("player", player.Name, string(packet.Body.Content))
				return
			}
			r.playerChat(ctx, c.Username, player.Name, string(packet.Body.Content))
		case packet_ids.S2CSystemChatID:
			// Server text is retained as context but never granted player authority.
			var packet packets.S2CSystemChat
			if err := wire.ReadInto(&packet); err != nil {
				log.Printf("decode system chat: %v", err)
				return
			}
			if bool(packet.Overlay) {
				return
			}
			text, at := packet.Content.String(), time.Now()
			if strings.HasPrefix(packet.Content.Translate, "death.") && len(packet.Content.With) > 0 {
				r.rememberContext("system", "", text)
				if err := r.store.recordDeath(packet.Content.With[0].String(), packet.Content.Translate); err != nil {
					log.Printf("record death: %v", err)
				}
			} else if packet.Content.Translate == "multiplayer.player.joined" || packet.Content.Translate == "multiplayer.player.left" {
				r.rememberContext("system", "", text)
			} else {
				if r.captureFeedback(text, at) {
					return
				}
				// System chat has no recipient metadata. It may be delayed private
				// plugin feedback, so do not expose it as public conversation.
				r.rememberInternal("system", "", text)
			}
		case packet_ids.S2CDisguisedChatID:
			var packet packets.S2CDisguisedChat
			if err := wire.ReadInto(&packet); err != nil {
				log.Printf("decode disguised chat: %v", err)
				return
			}
			typeName := chatTypeName(c, packet.ChatType)
			sender, text, at := packet.ChatType.Name.String(), packet.Message.String(), time.Now()
			if privateChatType(typeName) || packet.ChatType.TargetName.Present {
				r.rememberInternal("private_disguised", sender, text)
				return
			}
			if r.captureFeedback(text, at) {
				return
			}
			if !publicChatType(typeName) {
				r.rememberInternal("disguised", sender, text)
				return
			}
			id, err := r.journal.append("disguised", sender, text, at)
			if err != nil {
				log.Printf("record disguised chat: %v", err)
			}
			if r.consoleChat && typeName == "minecraft:say_command" && sender == "Server" {
				// Plugins can also issue console /say: conversational input only,
				// NEVER authenticated operator authority.
				r.enqueue(ctx, c.Username, "@server", text, id, at, mentioned(text, c.Username, r.aliases))
			}
		}
	}
}

func (r *chatRouter) rememberContext(kind, sender, text string) {
	if _, err := r.journal.append(kind, sender, text, time.Now()); err != nil {
		log.Printf("record %s chat: %v", kind, err)
	}
}

func (r *chatRouter) rememberInternal(kind, sender, text string) {
	if _, err := r.journal.appendOwned(kind, sender, "@unattributed", text, time.Now()); err != nil {
		log.Printf("record internal %s chat: %v", kind, err)
	}
}

func (r *chatRouter) disconnect() {
	if err := r.store.disconnectPresence(time.Now()); err != nil {
		log.Printf("record disconnect: %v", err)
	}
	if r.commands != nil {
		r.commands.bind(nil)
	}
	r.assistant.engagements.reset()
}

// Upstream chat signing/acknowledgement state is shared by reads and writes.
// Serialize access while the AI and lyrics run outside the packet loop.
type synchronizedChat struct {
	inner  *chatmodule.Module
	ctx    context.Context
	gate   chan struct{}
	ready  bool
	client *client.Client
}

func (m *synchronizedChat) Name() string          { return m.inner.Name() }
func (m *synchronizedChat) Init(c *client.Client) { m.inner.Init(c) }
func (m *synchronizedChat) Reset() {
	m.gate <- struct{}{}
	defer func() { <-m.gate }()
	m.ready = false
	m.inner.Reset()
}

// Bound both waiting for signing state and every network write, including
// acknowledgements from HandlePacket. Closing only the captured transport
// cannot abort a replacement connection.
func (m *synchronizedChat) acquire(ctx context.Context) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	select {
	case m.gate <- struct{}{}:
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		<-m.gate
		cancel()
		return nil, err
	}
	conn := m.client.Conn()
	stop := context.AfterFunc(ctx, func() {
		if conn != nil {
			_ = conn.Close()
		}
	})
	return func() { stop(); cancel(); <-m.gate }, nil
}

func (m *synchronizedChat) HandlePacket(packet *jp.WirePacket) {
	if m.client.State() != jp.StatePlay {
		return
	}
	// Match the inner chat module's packet set: chunk/entity traffic must not
	// allocate chat deadlines or contend with the signing state.
	switch packet.PacketID {
	case packet_ids.S2CCommandsID, packet_ids.S2CPlayerChatID, packet_ids.S2CSystemChatID, packet_ids.S2CDisguisedChatID:
	default:
		return
	}
	release, err := m.acquire(m.ctx)
	if err != nil {
		return
	}
	defer release()
	m.inner.HandlePacket(packet)
}
func (m *synchronizedChat) SendMessage(message string) error {
	return m.sendMessage(m.ctx, message, nil)
}
func (m *synchronizedChat) sendMessage(ctx context.Context, message string, expected *jp.Conn) error {
	release, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := m.connected(expected); err != nil {
		return err
	}
	return m.inner.SendMessage(message)
}
func (m *synchronizedChat) SendCommand(command string) error {
	return m.sendCommand(m.ctx, command, nil)
}
func (m *synchronizedChat) sendCommand(ctx context.Context, command string, expected *jp.Conn) error {
	release, err := m.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := m.connected(expected); err != nil {
		return err
	}
	return m.inner.SendCommand(command)
}
func (m *synchronizedChat) connected(expected *jp.Conn) error {
	if !m.ready || m.ctx.Err() != nil {
		return fmt.Errorf("Minecraft chat is disconnected")
	}
	if conn := m.client.Conn(); conn == nil || (expected != nil && conn != expected) {
		return fmt.Errorf("Minecraft connection changed")
	}
	return nil
}
func (m *synchronizedChat) Commands() ([]string, error) {
	return m.commands(m.ctx)
}
func (m *synchronizedChat) commands(ctx context.Context) ([]string, error) {
	release, err := m.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := m.connected(nil); err != nil {
		return nil, err
	}
	return m.inner.Commands()
}
func (m *synchronizedChat) SendChatSessionData() error {
	release, err := m.acquire(m.ctx)
	if err != nil {
		return err
	}
	defer release()
	return m.inner.SendChatSessionData()
}

// One output path enforces the client's byte limit, valid UTF-8 and pacing.
type chatOutput struct {
	gate     chan struct{}
	mu       sync.RWMutex
	write    func(context.Context, string) error
	whisper  func(context.Context, string, string) error
	last     time.Time
	recent   [5]time.Time
	next     int
	interval time.Duration
	journal  *chatHistory
	username string
}

func newChatOutput() *chatOutput {
	return &chatOutput{gate: make(chan struct{}, 1), interval: 1100 * time.Millisecond}
}
func (o *chatOutput) bind(write func(context.Context, string) error, username string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.write = write
	o.username = username
	if write == nil {
		o.whisper = nil
	}
}

func (o *chatOutput) bindWhisper(write func(context.Context, string, string) error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.whisper = write
}
func (o *chatOutput) send(ctx context.Context, text string) error {
	return o.sendChunks(ctx, chatChunks(text))
}

// Structured model replies are validated and sent verbatim: no prefixing,
// sanitizing, splitting, trimming, or MiniMessage interpretation.
func (o *chatOutput) sendMessage(ctx context.Context, text string) error {
	if err := validatePublicMessage(text); err != nil {
		return err
	}
	return o.sendChunks(ctx, []string{text})
}

func (o *chatOutput) identity() string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.username
}

func (o *chatOutput) sendChunks(ctx context.Context, chunks []string) error {
	select {
	case o.gate <- struct{}{}:
		defer func() { <-o.gate }()
	case <-ctx.Done():
		return ctx.Err()
	}
	for _, chunk := range chunks {
		next := o.last.Add(o.interval)
		if oldest := o.recent[o.next]; !oldest.IsZero() && next.Before(oldest.Add(10100*time.Millisecond)) {
			next = oldest.Add(10100 * time.Millisecond)
		}
		if delay := time.Until(next); delay > 0 {
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
		write, whisper, username := o.write, o.whisper, o.username
		o.mu.RUnlock()
		if write == nil {
			return fmt.Errorf("Minecraft chat is disconnected")
		}
		owner := ""
		if scope := replyScope(ctx); scope != nil && scope.private.Load() {
			owner = scope.actor
			if owner == consoleActor || owner == "@server" {
				log.Printf("PRIVATE %s: %s", owner, chunk)
			} else {
				if !validUsername(owner) || whisper == nil {
					return fmt.Errorf("private reply destination unavailable; nothing was broadcast")
				}
				if err := whisper(ctx, owner, chunk); err != nil {
					return err
				}
			}
		} else if err := write(ctx, chunk); err != nil {
			return err
		}
		o.last = time.Now()
		o.recent[o.next] = o.last
		o.next = (o.next + 1) % len(o.recent)
		if _, err := o.journal.appendOwned("bot", username, owner, chunk, o.last); err != nil {
			log.Printf("record bot chat: %v", err)
		}
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
