package main

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-mclib/client"
	chatmodule "github.com/go-mclib/client/modules/chat"
	"github.com/go-mclib/client/modules/playerlist"
	"github.com/go-mclib/client/modules/registry"
	"github.com/go-mclib/data/lang"
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
	word := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
	matches := func(name string) bool {
		name = strings.ToLower(name)
		if name == "" {
			return false
		}
		for offset := 0; offset <= len(message)-len(name); {
			found := strings.Index(message[offset:], name)
			if found < 0 {
				return false
			}
			start := offset + found
			end := start + len(name)
			before, _ := utf8.DecodeLastRuneInString(message[:start])
			after, _ := utf8.DecodeRuneInString(message[end:])
			if (start == 0 || !word(before)) && (end == len(message) || !word(after)) {
				return true
			}
			offset = end
		}
		return false
	}
	if matches(username) {
		return true
	}
	for _, alias := range aliases {
		if matches(alias) {
			return true
		}
	}
	return false
}

var discordChatPattern = regexp.MustCompile(`^\[Discord\][\p{Zs}]+([^»\r\n]+?)[\p{Zs}]+»[\p{Zs}]+((?s:.+))$`)

const discordBridgeActor = "#discord_bridge"

func normalizeDiscordNickname(nickname string) string {
	if !utf8.ValidString(nickname) {
		return ""
	}
	for _, r := range nickname {
		if unicode.IsControl(r) {
			return ""
		}
	}
	nickname = strings.ToLower(strings.Join(strings.Fields(nickname), " "))
	if nickname == "" || len(nickname) > 119 {
		return ""
	}
	return nickname
}

func isDiscordActor(actor string) bool {
	return actor == discordBridgeActor
}

// Only system-chat packets may enter this bridge path. A nickname is an
// unprivileged conversation label, never a Minecraft account or private owner.
func (r *chatRouter) discordChat(ctx context.Context, username, text string, at time.Time) bool {
	if strings.HasPrefix(text, "[Discord]") && r.moderation.blocked(discordBridgeActor, at) {
		return true
	}
	match := discordChatPattern.FindStringSubmatch(text)
	if match == nil {
		return false
	}
	nickname := normalizeDiscordNickname(match[1])
	if nickname == "" || strings.TrimSpace(match[2]) == "" {
		return false
	}
	if ctx.Err() != nil || nickname == normalizeDiscordNickname(username) {
		return true
	}
	display := strings.TrimSpace(match[1])
	if !r.admitChat(ctx, discordBridgeActor, display, match[2], false, at) {
		return true
	}
	id, err := r.journal.appendDiscord(display, match[2], at)
	if err != nil {
		log.Printf("record Discord chat: %v", err)
		return true
	}
	if id != 0 {
		r.assistant.engagements.observe(discordBridgeActor, display, match[2], at)
		if r.routeGame(ctx, discordBridgeActor, display, match[2], username) {
			return true
		}
		r.assistant.engagements.enqueue(chatRequest{sender: discordBridgeActor, username: username, ctx: ctx,
			mentioned: mentioned(match[2], username, r.aliases),
			messages:  []chatFragment{{ID: id, At: at, Text: match[2], DisplayName: display}}})
	}
	return true
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
	moderation   *chatModeration
	games        *gameChat
}

func (r *chatRouter) playerChat(ctx context.Context, username, sender, message string) {
	if ctx.Err() != nil || !validUsername(sender) || strings.EqualFold(sender, username) || strings.TrimSpace(message) == "" {
		return
	}
	at := time.Now()
	if !r.admitChat(ctx, sender, "", message, false, at) {
		return
	}
	eventID, err := r.journal.append("player", sender, message, at)
	if err != nil {
		log.Printf("record player chat: %v", err)
		return
	}
	if eventID != 0 {
		r.assistant.engagements.observe(sender, "", message, at)
		r.enqueue(ctx, username, sender, message, eventID, at, mentioned(message, username, r.aliases))
	}
}

func (r *chatRouter) admitChat(ctx context.Context, sender, display, text string, private bool, at time.Time) bool {
	if !r.moderation.admit(ctx, sender, text, at) {
		return false
	}
	r.moderation.noteSpeaker(sender, display, at)
	if r.moderation.command(ctx, sender, text, private, at) {
		r.assistant.engagements.forget(sender)
		return false
	}
	return true
}

func (r *chatRouter) enqueue(ctx context.Context, username, sender, text string, id uint64, at time.Time, mention bool) {
	if r.moderation.blocked(sender, at) {
		return
	}
	if r.routeGame(ctx, sender, "", text, username) {
		return
	}
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
					r.moderation.noteSpeaker(player.Name, "", time.Now())
					r.assistant.engagements.observe(player.Name, "", "", time.Now())
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
				// Unresolved input cannot be safely attributed or checked against
				// a block. Do not retain it as anonymous model context.
				return
			}
			if isPrivate {
				if !r.admitChat(ctx, player.Name, "", string(packet.Body.Content), true, time.Now()) {
					return
				}
				r.rememberContext("private", player.Name, string(packet.Body.Content))
				return
			}
			// Paper uses paper:raw for renderer-decorated player chat.
			// Accept it only with a resolved player UUID, never as server feedback.
			if !publicChatType(typeName) && typeName != "paper:raw" {
				if !r.admitChat(ctx, player.Name, "", string(packet.Body.Content), true, time.Now()) {
					return
				}
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
			text, at := packet.Content.Render(lang.Translate), time.Now()
			if bool(packet.Overlay) {
				// Action-bar text is still system history, never a bridge input or
				// an invitation to reply. Unknown recipients remain quarantined.
				if !strings.HasPrefix(text, "[Discord]") || !r.moderation.blocked(discordBridgeActor, at) {
					r.rememberInternal("system", "", text)
				}
				return
			}
			if r.discordChat(ctx, c.Username, text, at) {
				return
			}
			if strings.HasPrefix(packet.Content.Translate, "death.") && len(packet.Content.With) > 0 {
				if r.moderation.blocked(packet.Content.With[0].String(), at) {
					return
				}
				r.rememberContext("system", "", text)
				if err := r.store.recordDeath(packet.Content.With[0].String(), packet.Content.Translate); err != nil {
					log.Printf("record death: %v", err)
				}
			} else if publicSystemAnnouncement(packet.Content.Translate) {
				if len(packet.Content.With) > 0 && r.moderation.blocked(packet.Content.With[0].String(), at) {
					return
				}
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
			if r.moderation.blocked(sender, at) {
				return
			}
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

func publicSystemAnnouncement(translation string) bool {
	switch translation {
	case "multiplayer.player.joined", "multiplayer.player.joined.renamed", "multiplayer.player.left",
		"chat.type.advancement.task", "chat.type.advancement.goal", "chat.type.advancement.challenge":
		return true
	default:
		return false
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
	r.games.reset()
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
	case packet_ids.S2CCommandsID, packet_ids.S2CCommandSuggestionsID, packet_ids.S2CPlayerChatID, packet_ids.S2CSystemChatID, packet_ids.S2CDisguisedChatID:
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
func (m *synchronizedChat) completeCommand(ctx context.Context, input string, expected *jp.Conn) (packets.S2CCommandSuggestions, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	release, err := m.acquire(ctx)
	if err != nil {
		return packets.S2CCommandSuggestions{}, err
	}
	err = m.connected(expected)
	release()
	if err != nil {
		return packets.S2CCommandSuggestions{}, err
	}
	// The SDK owns correlation and session synchronization. In particular, do not
	// hold gate while waiting: HandlePacket needs it to deliver the response.
	// Recheck the captured transport in the SDK's submission critical section.
	return m.inner.SuggestCommandContextForConnection(ctx, input, expected)
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
		if scope := replyScope(ctx); scope != nil && scope.privateReply() {
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
