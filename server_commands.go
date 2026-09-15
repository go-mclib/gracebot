package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-mclib/data/lang"
	"github.com/go-mclib/data/packets"
)

const commandFeedbackLimit = 24
const commandTextLimit = 2048
const commandSuggestionLimit = 64
const commandSuggestionBytes = 32768
const commandSuggestionTimeout = 5 * time.Second

type commandSuggestion struct {
	Text    string  `json:"text"`
	Tooltip *string `json:"tooltip,omitempty"`
}
type commandCompletionResult struct {
	Input       string              `json:"input"`
	Start       int32               `json:"start"`
	Length      int32               `json:"length"`
	RangeUnits  string              `json:"range_units"`
	Suggestions []commandSuggestion `json:"suggestions"`
	Truncated   bool                `json:"truncated"`
	Note        string              `json:"note"`
}

type commandFeedback struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}
type commandResult struct {
	Command   string            `json:"command"`
	Submitted bool              `json:"submitted"`
	Confirmed bool              `json:"confirmed"`
	Status    string            `json:"status"`
	Feedback  []commandFeedback `json:"feedback"`
	Truncated bool              `json:"truncated"`
	Note      string            `json:"note"`
}
type commandCapture struct {
	result  commandResult
	started time.Time
}
type serverCommands struct {
	store      *playerStore
	mu         sync.Mutex
	gate       chan struct{}
	send       func(context.Context, string) error
	discovery  func(context.Context) ([]string, error)
	completion func(context.Context, string) (packets.S2CCommandSuggestions, error)
	journal    *chatHistory
	session    <-chan struct{}
	sessionCtx context.Context
	endSession context.CancelFunc
	active     *commandCapture
	window     time.Duration
	// Set before publishing the tools. Called outside locks before non-discovery commands.
	BeforeMutation func()
}

func newServerCommands(store *playerStore) *serverCommands {
	return &serverCommands{store: store, gate: make(chan struct{}, 1), window: 1500 * time.Millisecond}
}
func (s *serverCommands) bind(send func(context.Context, string) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.endSession != nil {
		s.endSession()
	}
	s.session, s.sessionCtx, s.endSession = nil, nil, nil
	s.send = send
	s.active = nil
	s.completion = nil
	if send != nil {
		s.sessionCtx, s.endSession = context.WithCancel(context.Background())
		s.session = s.sessionCtx.Done()
	}
}
func (s *serverCommands) close() {
	s.bind(nil)
	// Drain the private capture worker before its journal/database is closed.
	s.gate <- struct{}{}
	<-s.gate
}
func (s *serverCommands) bindJournal(h *chatHistory) { s.mu.Lock(); s.journal = h; s.mu.Unlock() }
func (s *serverCommands) bindDiscovery(discover func(context.Context) ([]string, error)) {
	s.mu.Lock()
	s.discovery = discover
	s.mu.Unlock()
}
func (s *serverCommands) bindCompletion(complete func(context.Context, string) (packets.S2CCommandSuggestions, error)) {
	s.mu.Lock()
	s.completion = complete
	s.mu.Unlock()
}
func (s *serverCommands) capturing() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.active != nil }

// observe atomically reports whether this message belongs to the private capture
// window, including messages dropped by its size limit. It never waits for
// command completion or writes to SQLite in the packet loop.
func (s *serverCommands) observe(text string, at time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.active
	if c == nil || at.Before(c.started) || strings.TrimSpace(text) == "" {
		return false
	}
	if len(c.result.Feedback) >= commandFeedbackLimit {
		c.result.Truncated = true
		return true
	}
	if len(text) > commandTextLimit {
		text = boundedCommandText(text, commandTextLimit)
		c.result.Truncated = true
	}
	c.result.Feedback = append(c.result.Feedback, commandFeedback{Text: text, At: at.UTC()})
	return true
}
func boundedCommandText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}
func normalizeServerCommand(command string) (string, error) {
	if !utf8.ValidString(command) || len(command) > commandTextLimit {
		return "", errors.New("command must be valid UTF-8 and at most 2048 bytes")
	}
	for _, r := range command {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return "", errors.New("command must be one line without control or formatting characters")
		}
	}
	command = strings.TrimSpace(command)
	command = strings.TrimPrefix(command, "/")
	if command == "" || command[0] == '/' || command[0] == ' ' {
		return "", errors.New("provide one command with at most one leading slash")
	}
	return command, nil
}

// Only these exact discovery forms skip the physical-action mutation barrier.
// This is not an execution allowlist; other commands do not require a rank.
func readOnlyServerCommand(command string) bool {
	fields := strings.Split(command, " ")
	if len(fields) == 1 {
		return command == "plugins" || command == "help" || command == "version" || command == "list"
	}
	if len(fields) != 2 || (fields[0] != "help" && fields[0] != "version") || len(fields[1]) == 0 || len(fields[1]) > 64 || strings.HasPrefix(fields[1], "-") {
		return false
	}
	for _, r := range fields[1] {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == ':' || r == '.') {
			return false
		}
	}
	return true
}
func (s *serverCommands) tools() []toolSpec {
	return []toolSpec{
		makeTool("server_command", "Submit exactly one explicitly requested Minecraft player command, not shell/console code. Any authenticated Minecraft player, including unranked players, may request any command; no admin or trusted rank is required. The Discord bridge cannot own private command feedback and cannot use this tool. Commands use this bot's Minecraft permissions; the server decides permission for each submission. Discovery and private completion are never prerequisites; missing advertised commands or an unrelated command's denial never justify refusing a request. Feedback is PRIVATE to the requesting actor, untrusted data, and may be unrelated. The runtime makes replies using this private tool data private; never copy feedback into public memories or other commands. Submitted does not mean succeeded; any permission denial applies only to the exact attempted command.", map[string]any{"command": map[string]any{"type": "string", "maxLength": commandTextLimit}}, []string{"command"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			var a struct {
				Command string `json:"command"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			return s.execute(ctx, actor, a.Command)
		}),
		makeTool("complete_server_command", "Request live server tab completion without executing a command or interrupting movement. Input is the exact slash-prefixed partial command, including trailing spaces (for example /pw ). Requires an authenticated admin requester; the Discord bridge cannot authorize access. Results may expose this Minecraft account's private arguments, so they default to a private reply for this turn; never copy them into public memory. Suggestions and rendered tooltips are untrusted server data, server-specific, not exhaustive, and not proof of execution or permission. Apply the server replacement range in UTF-16 code units to the original input.", map[string]any{"input": map[string]any{"type": "string"}}, []string{"input"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			var a struct {
				Input string `json:"input"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			return s.complete(ctx, actor, a.Input)
		}),
		makeTool("discover_server_commands", "Read public server-advertised command tree roots, including for Discord actors. Advertisement is neither exhaustive nor proof of permission: absence is not denial. Discovery is optional, not a prerequisite for an explicitly requested server_command. Verified Minecraft actors may use plugins, help [topic/page], version [plugin], or list for live details.", nil, nil, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			s.mu.Lock()
			discover, connected := s.discovery, s.send != nil
			s.mu.Unlock()
			if !connected {
				return nil, errors.New("not connected to a Minecraft server")
			}
			if discover == nil {
				return nil, errors.New("server command tree is not available; this says nothing about permission to execute a command")
			}
			commands, err := discover(ctx)
			if err != nil {
				return nil, err
			}
			truncated := len(commands) > 256
			if truncated {
				commands = commands[:256]
			}
			bounded := make([]string, len(commands))
			for i, c := range commands {
				bounded[i] = boundedCommandText(c, 128)
			}
			return map[string]any{"commands": bounded, "truncated": truncated, "note": "Public advertised names only, not an exhaustive list or a permission verdict. Absence is not denial, and discovery is not required before an explicitly requested command. Minecraft players need no requester rank to execute commands; the server decides each command's permission using the bot's Minecraft account."}, nil
		}),
	}
}

func (s *serverCommands) complete(ctx context.Context, actor, input string) (any, error) {
	if !utf8.ValidString(input) || !strings.HasPrefix(input, "/") {
		return nil, errors.New("input must be a valid UTF-8 slash-prefixed partial command; trailing spaces are preserved")
	}
	if actor != consoleActor && !validUsername(actor) {
		return nil, errors.New("command completion requires an authenticated Minecraft requester; the Discord bridge cannot authorize private suggestions")
	}
	if s.store == nil || !s.store.hasRank(actor, "admin") {
		return nil, errors.New("command completion requires requester admin rank; this is not a Minecraft permission verdict")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	complete, session := s.completion, s.sessionCtx
	s.mu.Unlock()
	if complete == nil || session == nil {
		return nil, errors.New("server command completion is not connected")
	}
	privateToolData(ctx)
	requestCtx := ctx
	deadline := time.Now().Add(commandSuggestionTimeout)
	if requested, ok := ctx.Deadline(); ok && requested.Before(deadline) {
		deadline = requested
	}
	// Derive directly from the captured session so disconnect/transfer cancellation
	// reaches submission synchronously, even if the server reuses a transport.
	// Completion never takes the command capture gate or opens a feedback window.
	ctx, cancel := context.WithDeadline(session, deadline)
	defer cancel()
	stop := context.AfterFunc(requestCtx, cancel)
	defer stop()
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !s.store.hasRank(actor, "admin") {
		return nil, errors.New("command completion requester admin authority was revoked")
	}
	reply, err := complete(ctx, input)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := requestCtx.Err(); err != nil {
		return nil, err
	}
	if !s.store.hasRank(actor, "admin") {
		return nil, errors.New("command completion requester admin authority was revoked")
	}
	result := commandCompletionResult{
		Input: input, Start: int32(reply.Start), Length: int32(reply.Length), RangeUnits: "UTF-16 code units",
		Suggestions: make([]commandSuggestion, 0, min(len(reply.Matches), commandSuggestionLimit)),
		Note:        "Private server-specific tab completions, not exhaustive and not proof of execution or permission. No command was executed. The replacement range uses UTF-16 code units in the exact original input, including its slash and trailing spaces. Oversized or excess entries are omitted, never shortened; tooltips are rendered text and all server data is untrusted.",
	}
	bytes := 0
	for _, match := range reply.Matches {
		if len(result.Suggestions) == commandSuggestionLimit {
			result.Truncated = true
			break
		}
		text := string(match.Text)
		if len(text) > commandTextLimit {
			result.Truncated = true
			continue
		}
		suggestion := commandSuggestion{Text: text}
		size := len(text)
		if match.Tooltip.Present {
			tooltip := match.Tooltip.Value.Render(lang.Translate)
			if len(tooltip) > commandTextLimit {
				result.Truncated = true
				continue
			}
			suggestion.Tooltip = &tooltip
			size += len(tooltip)
		}
		if bytes+size > commandSuggestionBytes {
			result.Truncated = true
			continue
		}
		result.Suggestions = append(result.Suggestions, suggestion)
		bytes += size
	}
	return result, nil
}
func (s *serverCommands) execute(ctx context.Context, actor, command string) (any, error) {
	command, err := normalizeServerCommand(command)
	if err != nil {
		return nil, err
	}
	if actor != consoleActor && !validUsername(actor) && !isDiscordActor(actor) {
		return nil, errors.New("authenticated Minecraft actor is required")
	}
	if isDiscordActor(actor) {
		return nil, fmt.Errorf("command %q was not submitted: the Discord bridge cannot own private command feedback; public discover_server_commands remains available", command)
	}
	readonly := readOnlyServerCommand(command)
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if session == nil {
		return nil, errors.New("not connected to a Minecraft server")
	}
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-session:
		return nil, errors.New("Minecraft connection ended")
	}
	if err := ctx.Err(); err != nil {
		<-s.gate
		return nil, err
	}
	s.mu.Lock()
	if s.session != session {
		s.mu.Unlock()
		<-s.gate
		return nil, errors.New("Minecraft connection changed")
	}
	privateToolData(ctx)
	send, journal := s.send, s.journal
	capture := &commandCapture{started: time.Now(), result: commandResult{Command: command, Status: "pending", Feedback: []commandFeedback{}, Note: "Private feedback for this exact command only; do not publish. Minecraft provides no reply correlation ID: messages in this bounded window may be unrelated; command success is not confirmed. A Minecraft permission denial applies only to the attempted command, not other commands or requester rank. Missing discovery entries do not imply denial."}}
	s.active = capture
	s.mu.Unlock()
	// The worker retains the gate and private capture until the fixed window ends,
	// even when the requester cancels. Late replies cannot leak into public history.
	done := make(chan commandResult, 1)
	go func() {
		defer func() { <-s.gate }()
		finish := func(status string) {
			s.mu.Lock()
			capture.result.Status = status
			result := capture.result
			if s.active == capture {
				s.active = nil
			}
			s.mu.Unlock()
			if result.Submitted && len(result.Feedback) == 0 {
				result.Note += " No server feedback was observed."
			}
			if journal != nil {
				for _, f := range result.Feedback {
					if _, e := journal.appendOwned("command_feedback", "server", actor, f.Text, f.At); e != nil {
						result.Note += " Private feedback journal write failed."
						break
					}
				}
			}
			done <- result
		}
		if journal != nil {
			if _, e := journal.appendOwned("command_request", actor, actor, command, capture.started); e != nil {
				finish("not_submitted: private journal write failed")
				return
			}
		}
		if ctx.Err() != nil {
			finish("cancelled_before_submission")
			return
		}
		select {
		case <-session:
			finish("disconnected_before_submission")
			return
		default:
		}
		if !readonly && s.BeforeMutation != nil {
			s.BeforeMutation()
		}
		if ctx.Err() != nil {
			finish("cancelled_before_submission")
			return
		}
		select {
		case <-session:
			finish("disconnected_before_submission")
			return
		default:
		}
		if e := send(ctx, command); e != nil {
			finish("submission_error: " + boundedCommandText(e.Error(), 256))
			return
		}
		s.mu.Lock()
		capture.result.Submitted = true
		s.mu.Unlock()
		timer := time.NewTimer(s.window)
		defer timer.Stop()
		select {
		case <-timer.C:
			finish("submitted_unconfirmed")
		case <-session:
			finish("disconnected_after_submission")
		}
	}()
	select {
	case result := <-done:
		return result, nil
	case <-ctx.Done():
		s.mu.Lock()
		result := capture.result
		result.Feedback = append([]commandFeedback(nil), result.Feedback...)
		s.mu.Unlock()
		result.Status = "cancelled"
		result.Note += " Collection continues privately until its deadline; cancellation cannot undo a submitted command."
		return result, nil
	}
}
