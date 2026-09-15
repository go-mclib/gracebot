package main

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/go-mclib/client"
	"github.com/go-mclib/client/modules/playerlist"
	"github.com/go-mclib/client/modules/protocol"
	"github.com/go-mclib/client/modules/registry"
	"github.com/go-mclib/data/packet_ids"
	"github.com/go-mclib/data/packets"
	"github.com/go-mclib/nbt"
	jp "github.com/go-mclib/protocol/java_protocol"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

func TestDiscordSystemChatEngagementAndCaptureIsolation(t *testing.T) {
	store := testPlayerStore(t)
	a := newAssistant(nil, "model", "", newToolRegistry(), nil)
	defer a.engagements.reset()
	commands := newServerCommands(store)
	commands.active = &commandCapture{started: time.Now().Add(-time.Second)}
	r := chatRouter{assistant: a, store: store, journal: store.history(), commands: commands, aliases: parseAliases("grace,helper")}
	c := client.New("unused", "GraceLocal", false)
	c.SetState(jp.StatePlay)
	handler := r.handler(context.Background(), playerlist.New())
	send := func(content ns.TextComponent) {
		t.Helper()
		wire, err := jp.ToWire(&packets.S2CSystemChat{Content: content})
		if err != nil {
			t.Fatal(err)
		}
		handler(c, wire)
	}

	send(ns.NewTextComponent("[Discord] Some 用户 🌍 » hello everyone"))
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("unmentioned Discord speaker started a conversation")
	}
	send(ns.TextComponent{Text: "[Discord] ", Color: "blue", Extra: []ns.TextComponent{
		{Text: "Some 用户 🌍", Color: "gold"},
		{Text: " » ", Color: "gray"},
		{Text: "hey HELPER", Color: "white"},
	}})
	request, _, ready := a.engagements.take(time.Now().Add(3 * time.Second))
	if !ready || request.sender != discordBridgeActor || len(request.messages) != 1 || request.messages[0].Text != "hey HELPER" || request.messages[0].DisplayName != "Some 用户 🌍" || !request.mentioned {
		t.Fatalf("styled Discord mention lost bridge identity/display label: %+v", request)
	}
	a.engagements.finish(request, "continue")
	send(ns.NewTextComponent("[Discord] SOME   用户 🌍 » and what about tomorrow?"))
	send(ns.NewTextComponent("[Discord] Other User » unrelated follow-up"))
	request, _, ready = a.engagements.take(time.Now().Add(3 * time.Second))
	if !ready || request.sender != discordBridgeActor || len(request.messages) != 1 || request.messages[0].Text != "and what about tomorrow?" || request.mentioned {
		t.Fatalf("unrelated Discord display diverted the focused bridge conversation: %+v", request)
	}
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("shared bridge batch was delivered twice")
	}
	if feedback := commands.active.result.Feedback; len(feedback) != 0 {
		t.Fatalf("public bridge chat entered private command feedback: %+v", feedback)
	}
	entries := historyEntries(t, r.journal, "alice", `{"kind":"discord"}`)
	if len(entries) != 4 {
		t.Fatalf("public Discord archive missing messages: %+v", entries)
	}
	for _, entry := range entries {
		if entry.Owner != "" || !isDiscordActor(entry.Sender) {
			t.Fatalf("Discord archive gained an account/private owner: %+v", entry)
		}
	}
	if contextData := r.journal.recentContext(0, request.sender); !strings.Contains(contextData, "hey HELPER") {
		t.Fatalf("Discord conversation missing public recent context: %s", contextData)
	}
}

func TestDiscordSystemChatWireComponents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content nbt.Tag
	}{
		{
			// Vanilla's heterogeneous-list encoding wraps a string as {"": "..."}.
			// Ignoring that entry loses "[", matching the production internal
			// history's "Discord] Eragon » ..." despite vanilla displaying "[".
			name: "mixed extra list preserves opening bracket",
			content: nbt.Compound{
				"text": nbt.String(""),
				"extra": nbt.List{ElementType: nbt.TagCompound, Elements: []nbt.Tag{
					nbt.Compound{"": nbt.String("[")},
					nbt.Compound{"text": nbt.String("Discord] "), "color": nbt.String("blue")},
					nbt.Compound{"text": nbt.String("Eragon"), "color": nbt.String("gold")},
					nbt.Compound{"text": nbt.String(" » Aaaaaa"), "color": nbt.String("white")},
				}},
			},
		},
		{
			name: "root list with horizontal unicode spacing",
			content: nbt.List{ElementType: nbt.TagCompound, Elements: []nbt.Tag{
				nbt.Compound{"": nbt.String("[")},
				nbt.Compound{"text": nbt.String("Discord]\u00a0Eragon\u2009»\u202fAaaaaa")},
			}},
		},
		{
			name: "translated announcement",
			content: nbt.Compound{
				"translate": nbt.String("chat.type.announcement"),
				"with": nbt.List{ElementType: nbt.TagString, Elements: []nbt.Tag{
					nbt.String("Discord"), nbt.String("Eragon » Aaaaaa"),
				}},
			},
		},
		{
			name: "plugin translation fallback",
			content: nbt.Compound{
				"translate": nbt.String("discord.bridge.chat"),
				"fallback":  nbt.String("[Discord] %s » %s"),
				"with": nbt.List{ElementType: nbt.TagString, Elements: []nbt.Tag{
					nbt.String("Eragon"), nbt.String("Aaaaaa"),
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testPlayerStore(t)
			a := newAssistant(nil, "model", "", newToolRegistry(), nil)
			defer a.engagements.reset()
			commands := newServerCommands(store)
			commands.active = &commandCapture{started: time.Now().Add(-time.Second)}
			r := chatRouter{assistant: a, store: store, journal: store.history(), commands: commands, aliases: parseAliases("grace")}
			c := client.New("unused", "GraceLocal", false)
			c.SetState(jp.StatePlay)
			handler := r.handler(context.Background(), playerlist.New())
			data, err := nbt.EncodeNetwork(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			handler(c, &jp.WirePacket{PacketID: packet_ids.S2CSystemChatID, Data: append(data, 0)})
			entries := historyEntries(t, r.journal, "alice", `{"kind":"discord"}`)
			if len(entries) != 1 || entries[0].Sender != discordBridgeActor || entries[0].DisplayName != "Eragon" || entries[0].Text != "Aaaaaa" || entries[0].Owner != "" {
				t.Fatalf("reported bridge frame lost shared attribution/display label: %+v", entries)
			}
			if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
				t.Fatal("unmentioned reported bridge frame started an engagement")
			}
			// A mention starts the shared bridge actor's conversation; packets
			// from its focused display continue without another mention.
			mention, err := jp.ToWire(&packets.S2CSystemChat{Content: ns.NewTextComponent("[Discord] Eragon » grace hello")})
			if err != nil {
				t.Fatal(err)
			}
			handler(c, mention)
			request, _, ready := a.engagements.take(time.Now().Add(3 * time.Second))
			if !ready || request.sender != discordBridgeActor || !request.mentioned {
				t.Fatalf("bridge mention failed to start shared engagement: %+v", request)
			}
			a.engagements.finish(request, "continue")
			handler(c, &jp.WirePacket{PacketID: packet_ids.S2CSystemChatID, Data: append(data, 0)})
			request, _, ready = a.engagements.take(time.Now().Add(3 * time.Second))
			if !ready || request.sender != discordBridgeActor || len(request.messages) != 1 || request.messages[0].Text != "Aaaaaa" || request.mentioned || store.hasRank(request.sender, "admin") {
				t.Fatalf("structured bridge continuation lost identity or gained authority: %+v", request)
			}
			if feedback := commands.active.result.Feedback; len(feedback) != 0 {
				t.Fatalf("bridge packets leaked into private command feedback: %+v", feedback)
			}
		})
	}
}

func TestDiscordBridgeTrustBoundaryAndSelfEcho(t *testing.T) {
	store := testPlayerStore(t)
	a := newAssistant(nil, "model", "", newToolRegistry(), nil)
	defer a.engagements.reset()
	r := chatRouter{assistant: a, store: store, journal: store.history(), aliases: parseAliases("grace")}
	c := client.New("unused", "GraceLocal", false)
	c.SetState(jp.StatePlay)
	handler := r.handler(context.Background(), playerlist.New())
	send := func(packet jp.Packet) {
		t.Helper()
		wire, err := jp.ToWire(packet)
		if err != nil {
			t.Fatal(err)
		}
		handler(c, wire)
	}
	for _, text := range []string{
		"prefix [Discord] RealAdmin » grace hello",
		"Discord] RealAdmin » grace hello",
		"[System] [CHAT] [Discord] RealAdmin » grace hello",
		"[Discord]\nRealAdmin » grace hello",
		"[Discord] RealAdmin\n» grace hello",
		"[Discord] RealAdmin »\ngrace hello",
		"[Discord] RealAdmin: grace hello",
		"[Discord]  » grace hello",
		"[Discord] RealAdmin »   ",
		"[Discord] " + strings.Repeat("界", 41) + " » grace hello",
		"[Discord] Bad\x00Name » grace hello",
		"[Discord] Real\nAdmin » grace hello",
		"RealAdmin whispers to you: grace secret",
	} {
		send(&packets.S2CSystemChat{Content: ns.NewTextComponent(text)})
	}
	send(&packets.S2CDisguisedChat{Message: ns.NewTextComponent("[Discord] RealAdmin » grace private"), ChatType: packets.ChatTypeBound{TargetName: ns.Some(ns.NewTextComponent("GraceLocal"))}})
	send(&packets.S2CSystemChat{Content: ns.NewTextComponent("[Discord] RealAdmin » grace overlay"), Overlay: true})
	send(&packets.S2CSystemChat{Content: ns.NewTextComponent("[Discord] gRaCeLoCaL » grace echo")})
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("ambiguous/private traffic or canonical self echo reached engagement")
	}
	if entries := historyEntries(t, r.journal, "alice", `{}`); len(entries) != 0 {
		t.Fatalf("quarantined traffic became public: %+v", entries)
	}

	// Aliases and arbitrary nicknames are not bot identity, even when matching
	// an administrator or a reserved internal actor.
	for _, nickname := range []string{"Grace", "RealAdmin", "@console", "@server"} {
		send(&packets.S2CSystemChat{Content: ns.NewTextComponent("[Discord] " + nickname + " » grace hello")})
		request, _, ready := a.engagements.take(time.Now().Add(3 * time.Second))
		if !ready || request.sender != discordBridgeActor || request.messages[0].DisplayName != nickname || store.hasRank(request.sender, "admin") {
			t.Fatalf("nickname suppressed or granted Minecraft authority: %+v", request)
		}
		a.engagements.finish(request, "disengage")
	}

	// A real player's message body is ordinary player input, not bridge metadata.
	r.playerChat(context.Background(), c.Username, "Alice", "[Discord] RealAdmin » grace hello")
	request, _, ready := a.engagements.take(time.Now().Add(3 * time.Second))
	if !ready || request.sender != "Alice" || request.messages[0].Text != "[Discord] RealAdmin » grace hello" {
		t.Fatalf("player body impersonated a Discord actor: %+v", request)
	}
}

func TestPlayerHeadChatRoutesAuthenticatedBody(t *testing.T) {
	// Vanilla 26.2 encoded unsigned content: [Member] [Alice head] Alice » hey grace.
	// The real packet sender below is Bob; decoration must not supply identity/body.
	decoration, err := base64.StdEncoding.DecodeString("CgkABWV4dHJhCgAAAAQIAAAACVtNZW1iZXJdIAAIAAAAASAACgAGcGxheWVyCAAEbmFtZQAFQWxpY2UAAAgAAAATIEFsaWNlIMK7IGhleSBncmFjZQAIAAR0ZXh0AAAA")
	if err != nil {
		t.Fatal(err)
	}
	store := testPlayerStore(t)
	a := newAssistant(nil, "model", "", newToolRegistry(), nil)
	defer a.engagements.reset()
	r := chatRouter{assistant: a, store: store, journal: store.history(), aliases: parseAliases("grace")}
	c := client.New("unused", "GraceLocal", false)
	proto, registries, players := protocol.New(), registry.New(), playerlist.New()
	c.Register(proto)
	c.Register(registries)
	c.Register(players)
	if err := registries.Wire(); err != nil {
		t.Fatal(err)
	}
	c.SetState(jp.StateConfiguration)
	wire, err := jp.ToWire(&packets.S2CRegistryData{
		RegistryId: "minecraft:chat_type",
		Entries:    []packets.RegistryEntry{{EntryId: "minecraft:chat"}, {EntryId: "minecraft:msg_command_incoming"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	proto.HandlePacket(wire)
	c.SetState(jp.StatePlay)
	sender := ns.UUID{1}
	info := ns.NewWriter()
	_ = info.WriteByte(1) // ADD_PLAYER
	_ = info.WriteVarInt(1)
	_ = info.WriteUUID(sender)
	_ = info.WriteString("Bob")
	_ = info.WriteVarInt(0) // Profile properties
	players.HandlePacket(&jp.WirePacket{PacketID: packet_ids.S2CPlayerInfoUpdateID, Data: info.Bytes()})
	handler := r.handler(context.Background(), players)
	send := func(uuid ns.UUID, body string, chatType ns.VarInt) {
		t.Helper()
		buf := ns.NewWriter()
		_ = buf.WriteVarInt(0) // Global message index
		_ = buf.WriteUUID(uuid)
		_ = buf.WriteVarInt(0)   // Sender's message index
		_ = buf.WriteBool(false) // No signature
		_ = buf.WriteString(ns.String(body))
		_ = buf.WriteInt64(0)   // Timestamp
		_ = buf.WriteInt64(0)   // Salt
		_ = buf.WriteVarInt(0)  // Last-seen signatures
		_ = buf.WriteBool(true) // Unsigned content
		_, _ = buf.Write(decoration)
		_ = buf.WriteVarInt(0) // Pass-through filter
		_ = buf.WriteVarInt(chatType + 1)
		_ = buf.WriteTextComponent(ns.NewTextComponent("Alice"))
		_ = buf.WriteBool(false) // No target name
		handler(c, &jp.WirePacket{PacketID: packet_ids.S2CPlayerChatID, Data: buf.Bytes()})
	}
	send(sender, "ordinary public conversation", 0)
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("mention in unsigned decoration started engagement")
	}
	send(sender, "grace, this is the real body", 0)
	request, _, ready := a.engagements.take(time.Now().Add(3 * time.Second))
	if !ready || request.sender != "Bob" || len(request.messages) != 1 || request.messages[0].Text != "grace, this is the real body" {
		t.Fatalf("decorated player packet lost authenticated identity/body: %+v", request)
	}
	a.engagements.finish(request, "disengage")
	send(sender, "grace, private body", 1)
	send(ns.UUID{2}, "grace, unknown sender", 0)
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("private or unknown player packet reached public engagement")
	}
}

func TestDiscordPublicReplyUsesMinecraftChat(t *testing.T) {
	store := testPlayerStore(t)
	output := newChatOutput()
	output.journal = store.history()
	var public []string
	output.bind(func(_ context.Context, text string) error {
		public = append(public, text)
		return nil
	}, "GraceLocal")
	output.bindWhisper(func(context.Context, string, string) error {
		t.Fatal("Discord public reply tried to whisper to a Minecraft account")
		return nil
	})
	ctx, _ := scopedTurn(context.Background(), discordBridgeActor)
	if err := output.send(ctx, "Hello from Minecraft"); err != nil {
		t.Fatal(err)
	}
	if len(public) != 1 || public[0] != "Hello from Minecraft" {
		t.Fatalf("Discord response did not use public Minecraft chat: %+v", public)
	}
	if entries := historyEntries(t, store.history(), "alice", `{"kind":"bot"}`); len(entries) != 1 || entries[0].Text != public[0] || entries[0].Owner != "" {
		t.Fatalf("Discord response was not archived publicly: %+v", entries)
	}
}

func TestSystemMessagesRetainAnnouncementsOverlaysAndSeparators(t *testing.T) {
	store := testPlayerStore(t)
	a := newAssistant(nil, "model", "", newToolRegistry(), nil)
	defer a.engagements.reset()
	r := &chatRouter{assistant: a, store: store, journal: store.history()}
	c := client.New("unused", "GraceLocal", false)
	c.SetState(jp.StatePlay)
	handler := r.handler(context.Background(), playerlist.New())
	send := func(content ns.TextComponent, overlay bool) {
		t.Helper()
		wire, err := jp.ToWire(&packets.S2CSystemChat{Content: content, Overlay: ns.Boolean(overlay)})
		if err != nil {
			t.Fatal(err)
		}
		handler(c, wire)
	}
	send(ns.TextComponent{Translate: "chat.type.advancement.task", With: []ns.TextComponent{
		ns.NewTextComponent("GraceLocal"), ns.NewTextComponent("[STONE_ADVANCEMENT]"),
	}}, false)
	send(ns.TextComponent{Translate: "chat.type.advancement.challenge", With: []ns.TextComponent{
		ns.NewTextComponent("Alice"), ns.NewTextComponent("[CHALLENGE_ADVANCEMENT]"),
	}}, false)
	send(ns.NewTextComponent("PRIVATE_ACTION_BAR"), true)
	send(ns.NewTextComponent("LATE_PLUGIN_FEEDBACK"), false)
	send(ns.NewTextComponent(""), false)

	rows, err := store.db.Query("SELECT text, owner FROM events WHERE kind='system' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	saved := make(map[string]string)
	for rows.Next() {
		var text, owner string
		if err := rows.Scan(&text, &owner); err != nil {
			t.Fatal(err)
		}
		saved[text] = owner
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(saved) != 5 {
		t.Fatalf("system messages were dropped: %v", saved)
	}
	for _, text := range []string{"PRIVATE_ACTION_BAR", "LATE_PLUGIN_FEEDBACK", ""} {
		if saved[text] != "@unattributed" {
			t.Fatalf("unclassified system text lost quarantine: %q owner=%q", text, saved[text])
		}
	}
	public := r.journal.recentContext(0, "Bob")
	if !strings.Contains(public, "STONE_ADVANCEMENT") || !strings.Contains(public, "CHALLENGE_ADVANCEMENT") {
		t.Fatalf("public advancements were not available as history: %s", public)
	}
	if strings.Contains(public, "PRIVATE_ACTION_BAR") || strings.Contains(public, "LATE_PLUGIN_FEEDBACK") {
		t.Fatalf("unclassified system data entered public context: %s", public)
	}
	if _, _, ready := a.engagements.take(time.Now().Add(3 * time.Second)); ready {
		t.Fatal("system message scheduled a model call")
	}
}

func TestMentionUsesWholeNameBoundaries(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"that was graceful", false},
		{"megagrace", false},
		{"grace_2", false},
		{"graceα", false},
		{"你好grace", false},
		{"hey, @GRACE!", true},
		{"graceful then grace help", true},
	} {
		if got := mentioned(tc.text, "GraceLocal", []string{"grace"}); got != tc.want {
			t.Errorf("mentioned(%q)=%v, want %v", tc.text, got, tc.want)
		}
	}
}
