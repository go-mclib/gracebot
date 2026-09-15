package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	spamTimeout           = 10 * time.Minute
	spamBurstWindow       = 10 * time.Second
	spamRepeatWindow      = 30 * time.Second
	spamBurstLimit        = 12
	spamRepeatLimit       = 5
	maxModerationSpeakers = 1024
)

type spamSample struct {
	at     time.Time
	digest [sha256.Size]byte
}

type speakerTraffic struct {
	last    time.Time
	samples []spamSample
}

type moderationReply struct {
	ctx         context.Context
	actor, text string
	private     bool
}

type chatModeration struct {
	store          *playerStore
	mu             sync.Mutex
	traffic        map[string]*speakerTraffic
	discordNames   map[string]time.Time
	minecraftNames map[string]time.Time
	onChange       func(string, string)
	send           func(context.Context, string) error
	replies        chan moderationReply
}

func newChatModeration(store *playerStore, send func(context.Context, string) error) *chatModeration {
	return &chatModeration{store: store, traffic: make(map[string]*speakerTraffic), discordNames: make(map[string]time.Time), minecraftNames: make(map[string]time.Time), send: send, replies: make(chan moderationReply, 16)}
}

func (m *chatModeration) blocked(actor string, at time.Time) bool {
	return m != nil && m.store.actorBlocked(actor, at)
}

func rememberSpeaker(names map[string]time.Time, name string, at time.Time) {
	if _, found := names[name]; !found && len(names) >= maxModerationSpeakers {
		var oldest string
		for candidate, seen := range names {
			if oldest == "" || seen.Before(names[oldest]) {
				oldest = candidate
			}
		}
		delete(names, oldest)
	}
	names[name] = at
}

// Nicknames are hints for resolving moderation targets, never account authority.
func (m *chatModeration) noteSpeaker(actor, display string, at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if isDiscordActor(actor) {
		rememberSpeaker(m.discordNames, normalizeDiscordNickname(display), at)
	} else if validUsername(actor) {
		rememberSpeaker(m.minecraftNames, normalizeStoreName(actor), at)
	}
}

// admit runs before persistence, logging, command dispatch or model input. Only
// digests/timestamps are kept for spam detection, never message contents.
func (m *chatModeration) admit(ctx context.Context, actor, text string, at time.Time) bool {
	if m == nil {
		return true
	}
	actor = normalizeStoreName(actor)
	if m.blocked(actor, at) {
		return false
	}
	if actor == consoleActor {
		return true
	}
	digest := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(text), " "))))
	m.mu.Lock()
	state := m.traffic[actor]
	if state == nil {
		if len(m.traffic) >= maxModerationSpeakers {
			var oldest string
			for name, entry := range m.traffic {
				if oldest == "" || entry.last.Before(m.traffic[oldest].last) {
					oldest = name
				}
			}
			delete(m.traffic, oldest)
		}
		state = &speakerTraffic{}
		m.traffic[actor] = state
	}
	state.last = at
	burst, repeated := 1, 1
	kept := state.samples[:0]
	for _, sample := range state.samples {
		if !sample.at.After(at.Add(-spamRepeatWindow)) {
			continue
		}
		kept = append(kept, sample)
		if sample.at.After(at.Add(-spamBurstWindow)) {
			burst++
		}
		if sample.digest == digest {
			repeated++
		}
	}
	state.samples = append(kept, spamSample{at: at, digest: digest})
	spam := burst >= spamBurstLimit || repeated >= spamRepeatLimit
	if spam {
		delete(m.traffic, actor)
	}
	m.mu.Unlock()
	if !spam {
		return true
	}
	until := at.Add(spamTimeout)
	if _, err := m.store.setActorBlock(ctx, actor, "@automatic", "chat spam", &until, at); err != nil {
		log.Printf("apply spam timeout for %s: %v", actor, err)
		// Do not let the triggering message through if persistence failed.
		return false
	}
	m.changed(actor, "@automatic")
	return false
}

func (m *chatModeration) changed(actor, by string) {
	m.mu.Lock()
	delete(m.traffic, actor)
	m.mu.Unlock()
	if m.onChange != nil {
		m.onChange(actor, by)
	}
}

func (m *chatModeration) resolveTarget(target, source string, at time.Time) (string, error) {
	target = strings.TrimSpace(target)
	source = strings.ToLower(strings.TrimSpace(source))
	if source != "" && source != "minecraft" && source != "discord" {
		return "", errors.New("source must be minecraft or discord")
	}
	if source == "discord" || strings.EqualFold(target, discordBridgeActor) || strings.HasPrefix(strings.ToLower(target), "discord:") {
		if source == "minecraft" {
			return "", errors.New("Discord cannot be targeted as a Minecraft account")
		}
		return discordBridgeActor, nil
	}
	if strings.HasPrefix(strings.ToLower(target), "minecraft:") {
		target = target[len("minecraft:"):]
		source = "minecraft"
	}
	if source == "" {
		m.mu.Lock()
		discordAt, discordSeen := m.discordNames[normalizeDiscordNickname(target)]
		_, minecraftSeen := m.minecraftNames[normalizeStoreName(target)]
		m.mu.Unlock()
		if discordSeen && at.Sub(discordAt) <= 10*time.Minute {
			if minecraftSeen || m.store.hasRank(target, "admin") {
				return "", errors.New("ambiguous name: specify minecraft:Name or discord:Nickname; Discord blocks affect the entire bridge")
			}
			return discordBridgeActor, nil
		}
	}
	return storePlayerName(target)
}

var blockDurationPart = regexp.MustCompile(`(?i)\s*([0-9]+)\s*(seconds?|secs?|s|minutes?|mins?|m|hours?|hrs?|h|days?|d|weeks?|w)`)

// A duration is a bounded sum of whole units, e.g. "1 hour 30 minutes", "2d"
// or "10m". Empty/forever/permanent means no expiry; invalid input never does.
func parseBlockDuration(raw string, at time.Time) (*time.Time, error) {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" || raw == "forever" || raw == "permanent" {
		return nil, nil
	}
	var total time.Duration
	for raw != "" {
		match := blockDurationPart.FindStringSubmatchIndex(raw)
		if match == nil || match[0] != 0 {
			return nil, errors.New("duration must use positive seconds, minutes, hours, days or weeks (for example 1 hour 30 minutes)")
		}
		count, err := strconv.ParseUint(raw[match[2]:match[3]], 10, 64)
		if err != nil {
			return nil, errors.New("duration is too large")
		}
		unit := raw[match[4]:match[5]]
		scale := time.Second
		switch unit[0] {
		case 'm':
			scale = time.Minute
		case 'h':
			scale = time.Hour
		case 'd':
			scale = 24 * time.Hour
		case 'w':
			scale = 7 * 24 * time.Hour
		}
		const max = 3650 * 24 * time.Hour
		if count > uint64((max-total)/scale) {
			return nil, errors.New("duration must not exceed 10 years")
		}
		total += time.Duration(count) * scale
		raw = strings.TrimSpace(raw[match[1]:])
	}
	if total <= 0 {
		return nil, errors.New("duration must be positive; use unblock to remove a block")
	}
	until := at.Add(total)
	return &until, nil
}

func (m *chatModeration) block(ctx context.Context, actor, target, source, duration string, at time.Time) (actorBlock, error) {
	if !m.store.hasRank(actor, "admin") {
		return actorBlock{}, errors.New("administrator authority required")
	}
	target, err := m.resolveTarget(target, source, at)
	if err != nil {
		return actorBlock{}, err
	}
	if target == normalizeStoreName(actor) {
		return actorBlock{}, errors.New("cannot block your own administrator identity")
	}
	until, err := parseBlockDuration(duration, at)
	if err != nil {
		return actorBlock{}, err
	}
	result, err := m.store.setActorBlock(ctx, target, normalizeStoreName(actor), "administrator block", until, at)
	if err == nil {
		m.changed(target, actor)
	}
	return result, err
}

func (m *chatModeration) unblock(ctx context.Context, actor, target, source string, at time.Time) (any, error) {
	if !m.store.hasRank(actor, "admin") {
		return nil, errors.New("administrator authority required")
	}
	target, err := m.resolveTarget(target, source, at)
	if err != nil {
		return nil, err
	}
	changed, err := m.store.clearActorBlock(ctx, target, actor)
	if err != nil {
		return nil, err
	}
	m.changed(target, actor)
	return map[string]any{"actor": target, "blocked": false, "changed": changed}, nil
}

func (m *chatModeration) tools() []toolSpec {
	target := map[string]any{"type": "string", "description": "Minecraft account name, #discord_bridge, or Discord nickname with source=discord. Blocking any Discord speaker blocks the entire bridge."}
	source := map[string]any{"type": "string", "enum": []string{"minecraft", "discord"}, "description": "Specify the source for names that could collide. Discord nicknames never authorize Minecraft actions."}
	return []toolSpec{
		makeTool("block_chat", "Admin-only: make a speaker invisible to chat/context/history. Default permanent; duration accepts e.g. 10 minutes, 2 hours, 7 days. Targeting Discord blocks the entire bridge. Use only when an authenticated admin requests it; automatic spam timeouts are enforced separately.", map[string]any{"target": target, "source": source, "duration": map[string]any{"type": "string"}}, []string{"target"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			var args struct {
				Target   string `json:"target"`
				Source   string `json:"source"`
				Duration string `json:"duration"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return m.block(ctx, actor, args.Target, args.Source, args.Duration, time.Now())
		}),
		makeTool("unblock_chat", "Admin-only: remove a permanent or temporary chat block. Unblocking a Discord nickname unblocks the entire #discord_bridge. Previously discarded messages cannot be recovered.", map[string]any{"target": target, "source": source}, []string{"target"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			var args struct {
				Target string `json:"target"`
				Source string `json:"source"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			return m.unblock(ctx, actor, args.Target, args.Source, time.Now())
		}),
	}
}

func (m *chatModeration) command(ctx context.Context, actor, text string, private bool, at time.Time) bool {
	if m == nil {
		return false
	}
	verb, rest, _ := strings.Cut(strings.TrimSpace(text), " ")
	verb = strings.ToLower(verb)
	if verb != "!block" && verb != "!unblock" {
		return false
	}
	var err error
	var reply string
	if strings.TrimSpace(rest) == "" {
		err = errors.New("usage: !block <name|#discord_bridge|discord:nickname> [for 10 minutes], or !unblock <target>")
	} else if verb == "!block" {
		target, duration := strings.TrimSpace(rest), ""
		if index := strings.LastIndex(strings.ToLower(target), " for "); index >= 0 {
			target, duration = target[:index], strings.TrimSpace(target[index+5:])
			if duration == "" {
				err = errors.New("a duration is required after for")
			}
		}
		if err == nil {
			var result actorBlock
			result, err = m.block(ctx, actor, target, "", duration, at)
			if err == nil {
				reply = "blocked " + result.Actor + " forever"
				if result.ExpiresAt != nil {
					reply = "blocked " + result.Actor + " until " + result.ExpiresAt.UTC().Format(time.RFC3339)
				}
			}
		}
	} else {
		var result any
		result, err = m.unblock(ctx, actor, strings.TrimSpace(rest), "", at)
		if err == nil {
			reply = "unblocked " + result.(map[string]any)["actor"].(string)
		}
	}
	if err != nil {
		reply = err.Error()
	}
	select {
	case m.replies <- moderationReply{ctx: ctx, actor: actor, text: reply, private: private}:
	default:
		log.Print("moderation reply queue full")
	}
	return true
}

func (m *chatModeration) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case reply := <-m.replies:
			if reply.ctx.Err() != nil || m.blocked(reply.actor, time.Now()) {
				continue
			}
			turnCtx, scope := scopedTurn(reply.ctx, reply.actor)
			if reply.private {
				if err := scope.setAudience("private"); err != nil {
					continue
				}
			}
			if m.send != nil {
				if err := m.send(turnCtx, reply.text); err != nil && ctx.Err() == nil {
					log.Printf("moderation reply: %v", err)
				}
			}
		}
	}
}
