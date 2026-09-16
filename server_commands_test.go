package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-mclib/data/packets"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

func commandStore(t *testing.T) *playerStore {
	t.Helper()
	s, err := openPlayerStore(filepath.Join(t.TempDir(), "state.sqlite"), filepath.Join(t.TempDir(), "admins.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.close() })
	return s
}
func TestServerCommandSafetyAndAuthority(t *testing.T) {
	s := newServerCommands(commandStore(t))
	s.window = time.Millisecond
	var sent []string
	s.bind(func(_ context.Context, command string) error {
		sent = append(sent, command)
		return nil
	})
	defer s.close()
	for _, command := range []string{"help\nstop", "help\rstop", "help\x00", "help\tstop", "help\u2028stop", "//help", "", strings.Repeat("x", 2049), "help\u202estop"} {
		if _, err := s.execute(context.Background(), "Alice", command); err == nil {
			t.Errorf("unsafe command accepted: %q", command)
		}
	}
	for _, actor := range []string{"", "@server", "@discord:alice", "not a username"} {
		if _, err := s.execute(context.Background(), actor, "help"); err == nil {
			t.Errorf("unauthenticated actor accepted: %q", actor)
		}
	}
	if len(sent) != 0 {
		t.Fatalf("rejected input reached Minecraft: %q", sent)
	}
	for _, command := range []string{"/help", "/minecraft:execute as @a run say hello"} {
		before := len(sent)
		value, err := s.execute(context.Background(), "Alice", command)
		if err != nil {
			t.Fatal(err)
		}
		result := value.(commandResult)
		if !result.Submitted || result.Confirmed || len(sent) != before+1 || sent[before] != strings.TrimPrefix(command, "/") {
			t.Fatalf("unranked request %q was not submitted exactly once: %+v; sent=%q", command, result, sent)
		}
	}
}

func TestServerCommandUnreleasedItemPreventsMutation(t *testing.T) {
	s := newServerCommands(commandStore(t))
	s.window = time.Millisecond
	var sent []string
	s.bind(func(_ context.Context, command string) error { sent = append(sent, command); return nil })
	defer s.close()
	s.BeforeMutation = func(context.Context) error { return errors.New("item release has not reached transport") }
	value, err := s.execute(context.Background(), "Alice", "spawn")
	if err != nil {
		t.Fatal(err)
	}
	if value.(commandResult).Submitted || len(sent) != 0 {
		t.Fatalf("command executed before held use could be released: %+v; sent=%q", value, sent)
	}
	value, err = s.execute(context.Background(), "Alice", "help")
	if err != nil {
		t.Fatal(err)
	}
	if !value.(commandResult).Submitted || len(sent) != 1 || sent[0] != "help" {
		t.Fatalf("read-only discovery was blocked by unrelated held use: %+v; sent=%q", value, sent)
	}
}

func TestServerCompletionWithholdsReplyAfterAdminRevocation(t *testing.T) {
	store := commandStore(t)
	if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Alice","rank":"admin","action":"add"}`)); err != nil {
		t.Fatal(err)
	}
	s := newServerCommands(store)
	s.bind(func(context.Context, string) error { return nil })
	defer s.close()
	s.bindCompletion(func(context.Context, string) (packets.S2CCommandSuggestions, error) {
		_, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Alice","rank":"admin","action":"remove"}`))
		return packets.S2CCommandSuggestions{Matches: []packets.CommandSuggestion{{Text: "private-warp"}}}, err
	})
	if value, err := s.complete(context.Background(), "Alice", "/pw "); err == nil || value != nil {
		t.Fatalf("revoked admin received private completion: %+v, %v", value, err)
	}
}
func TestDiscordCommandsCannotCaptureButCanDiscover(t *testing.T) {
	s := newServerCommands(commandStore(t))
	s.window = time.Millisecond
	s.bind(func(context.Context, string) error {
		t.Error("Discord command reached the Minecraft transport")
		return nil
	})
	defer s.close()
	s.bindDiscovery(func(context.Context) ([]string, error) {
		return []string{"help", "time"}, nil
	})
	for _, command := range []string{"help", "time query daytime"} {
		if _, err := s.execute(context.Background(), discordBridgeActor, command); err == nil {
			t.Fatalf("Discord command was accepted: %q", command)
		}
		if s.observe("public system chat after rejected request", time.Now()) {
			t.Fatal("rejected Discord command captured public system chat")
		}
	}
	for _, spec := range s.tools() {
		if spec.name != "discover_server_commands" {
			continue
		}
		value, err := spec.run(context.Background(), discordBridgeActor, json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		commands := value.(map[string]any)["commands"].([]string)
		if strings.Join(commands, ",") != "help,time" {
			t.Fatalf("Discord discovery did not return the advertised commands: %v", commands)
		}
		return
	}
	t.Fatal("public command discovery tool is unavailable")
}
func TestServerCommandPrivateFeedbackAndBounds(t *testing.T) {
	store := commandStore(t)
	s := newServerCommands(store)
	s.window = 5 * time.Millisecond
	s.bindJournal(store.history())
	s.bind(func(_ context.Context, command string) error {
		if !s.capturing() {
			t.Error("capture was not installed before submission")
		}
		if s.observe("old public message", time.Now().Add(-time.Hour)) {
			t.Error("captured feedback preceding the command")
		}
		for range commandFeedbackLimit + 2 {
			if !s.observe("private balance "+strings.Repeat("€", 1000), time.Now()) {
				t.Error("private capture was not reported, including overflow")
			}
		}
		return nil
	})
	defer s.close()
	value, err := s.execute(context.Background(), "alice", "balance")
	if err != nil {
		t.Fatal(err)
	}
	r := value.(commandResult)
	if s.observe("outside capture", time.Now()) {
		t.Fatal("completed command still captured feedback")
	}
	if len(r.Feedback) != commandFeedbackLimit || !r.Truncated || r.Confirmed {
		t.Fatalf("feedback bounds: %+v", r)
	}
	for _, f := range r.Feedback {
		if len(f.Text) > commandTextLimit {
			t.Fatal("unbounded feedback")
		}
	}
	for _, actor := range []string{"alice", "bob", consoleActor} {
		result, err := store.history().search(context.Background(), actor, json.RawMessage(`{"query":"private balance","limit":50}`))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(result)
		present := strings.Contains(string(data), "private balance")
		if present != (actor == "alice") {
			t.Fatalf("private ownership wrong for %s: %s", actor, data)
		}
	}
}
func TestServerCommandCancellationRetainsCaptureAndSerializes(t *testing.T) {
	s := newServerCommands(nil)
	s.window = 40 * time.Millisecond
	sent := make(chan string, 2)
	s.bind(func(_ context.Context, command string) error { sent <- command; return nil })
	defer s.close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan any, 1)
	go func() { value, _ := s.execute(ctx, "alice", "help"); done <- value }()
	<-sent
	cancel()
	<-done
	if !s.capturing() {
		t.Fatal("cancelled request released private capture too early")
	}
	s.observe("late private response", time.Now())
	queued, cancelQueued := context.WithCancel(context.Background())
	cancelQueued()
	if _, err := s.execute(queued, "bob", "plugins"); !errors.Is(err, context.Canceled) {
		t.Fatalf("queue cancellation: %v", err)
	}
	select {
	case c := <-sent:
		t.Fatalf("cancelled command submitted: %s", c)
	default:
	}
	next := make(chan any, 1)
	go func() { value, _ := s.execute(context.Background(), "bob", "plugins"); next <- value }()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("serialized command never submitted")
	}
	result := (<-next).(commandResult)
	if len(result.Feedback) != 0 {
		t.Fatal("previous actor feedback crossed request boundary")
	}
}
func TestServerCommandDisconnectAndSubmissionFailure(t *testing.T) {
	s := newServerCommands(nil)
	if _, err := s.execute(context.Background(), "alice", "help"); err == nil {
		t.Fatal("disconnected command accepted")
	}
	s.bind(func(context.Context, string) error { return errors.New("connection lost") })
	value, err := s.execute(context.Background(), "alice", "help")
	if err != nil {
		t.Fatal(err)
	}
	r := value.(commandResult)
	if r.Submitted || r.Confirmed || !strings.Contains(r.Status, "submission_error") {
		t.Fatalf("failed write reported as success: %+v", r)
	}
	sent := make(chan struct{})
	s.bind(func(context.Context, string) error { close(sent); return nil })
	done := make(chan any, 1)
	go func() { value, _ := s.execute(context.Background(), "alice", "help"); done <- value }()
	<-sent
	s.close()
	select {
	case value := <-done:
		r := value.(commandResult)
		if r.Confirmed || r.Status != "disconnected_after_submission" {
			t.Fatalf("disconnect status: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("disconnect did not release command")
	}
}

func TestCommandCancelledWhileWaitingForChatTransport(t *testing.T) {
	chat := &synchronizedChat{ctx: context.Background(), gate: make(chan struct{}, 1)}
	// Another packet operation owns the signing state. A cancelled command must
	// return without acquiring it or touching the deliberately absent signer.
	chat.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- chat.sendCommand(ctx, "help", nil) }()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled queued command: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled command remained stuck behind a chat operation")
	}
}

func TestServerCompletionAuthorityExactInputAndPrivacy(t *testing.T) {
	store := commandStore(t)
	if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Bob","rank":"trusted","action":"add"}`)); err != nil {
		t.Fatal(err)
	}
	s := newServerCommands(store)
	s.bind(func(context.Context, string) error {
		t.Error("completion executed a command")
		return nil
	})
	defer s.close()
	s.BeforeMutation = func(context.Context) error { t.Error("completion interrupted movement"); return nil }
	expected := "/pw "
	calls := 0
	s.bindCompletion(func(ctx context.Context, input string) (packets.S2CCommandSuggestions, error) {
		calls++
		if input != expected {
			t.Errorf("completion input changed: %q, want %q", input, expected)
		}
		if s.capturing() || s.observe("ordinary public system message", time.Now()) {
			t.Error("completion installed command feedback capture")
		}
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > commandSuggestionTimeout {
			t.Error("completion callback is not time bounded")
		}
		length := ns.VarInt(0)
		if input == "/pw 🌍a" {
			length = 3
		}
		return packets.S2CCommandSuggestions{Start: 4, Length: length, Matches: []packets.CommandSuggestion{{
			Text: "private-warp ",
			Tooltip: ns.PrefixedOptional[ns.TextComponent]{Present: true, Value: ns.TextComponent{
				Text: "Private", Extra: []ns.TextComponent{{Text: " warp"}},
			}},
		}}}, nil
	})
	var completion toolSpec
	for _, spec := range s.tools() {
		if spec.name == "complete_server_command" {
			completion = spec
		}
	}
	if completion.run == nil {
		t.Fatal("completion tool is not registered")
	}
	for _, actor := range []string{"Alice", "Bob", discordBridgeActor, "@discord:alice", "@discord:console", "@server", ""} {
		if _, err := completion.run(context.Background(), actor, json.RawMessage(`{"input":"/pw "}`)); err == nil {
			t.Errorf("unauthorized actor accepted: %q", actor)
		}
	}
	if calls != 0 {
		t.Fatal("unauthorized completion reached transport")
	}
	if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Alice","rank":"admin","action":"add"}`)); err != nil {
		t.Fatal(err)
	}
	ctx, _ := scopedTurn(context.Background(), "Alice")
	value, err := completion.run(ctx, "Alice", json.RawMessage(`{"input":"/pw "}`))
	if err != nil {
		t.Fatal(err)
	}
	r := value.(commandCompletionResult)
	if calls != 1 || r.Input != expected || r.Start != 4 || r.Length != 0 || r.RangeUnits != "UTF-16 code units" || r.Truncated {
		t.Fatalf("server completion correlation/input/range changed: %+v; calls=%d", r, calls)
	}
	if len(r.Suggestions) != 1 || r.Suggestions[0].Text != "private-warp " || r.Suggestions[0].Tooltip == nil || *r.Suggestions[0].Tooltip != "Private warp" {
		t.Fatalf("typed server suggestion or rendered tooltip changed: %+v", r.Suggestions)
	}
	out := newChatOutput()
	out.interval, out.journal = 0, store.history()
	var public, private []string
	out.bind(func(_ context.Context, text string) error { public = append(public, text); return nil }, "GraceLocal")
	out.bindWhisper(func(_ context.Context, actor, text string) error {
		if actor != "Alice" {
			t.Errorf("private suggestion sent to %q", actor)
		}
		private = append(private, text)
		return nil
	})
	if err := out.sendMessage(ctx, r.Suggestions[0].Text); err != nil {
		t.Fatal(err)
	}
	next, _ := scopedTurn(context.Background(), "Alice")
	if err := out.sendMessage(next, "hello"); err != nil {
		t.Fatal(err)
	}
	if len(private) != 1 || private[0] != "private-warp " || len(public) != 1 || public[0] != "hello" {
		t.Fatalf("completion escaped private turn or tainted next turn: private=%q public=%q", private, public)
	}
	history, err := store.history().search(context.Background(), "Bob", json.RawMessage(`{"query":"private-warp"}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(history)
	if err != nil || strings.Contains(string(data), "private-warp") {
		t.Fatalf("completion leaked through another player's journal: %s, %v", data, err)
	}
	expected = "/pw 🌍a"
	value, err = completion.run(context.Background(), consoleActor, json.RawMessage(`{"input":"/pw 🌍a"}`))
	if err != nil {
		t.Fatal(err)
	}
	r = value.(commandCompletionResult)
	if r.Input != expected || r.Start != 4 || r.Length != 3 {
		t.Fatalf("UTF-16 range converted to byte/rune offsets: %+v", r)
	}
}

func TestServerCompletionBoundsNeverShortenSuggestions(t *testing.T) {
	s := newServerCommands(commandStore(t))
	s.bind(func(context.Context, string) error { t.Fatal("completion executed a command"); return nil })
	defer s.close()
	for _, tc := range []struct {
		name    string
		matches []packets.CommandSuggestion
		want    int
		first   string
	}{
		{"count", make([]packets.CommandSuggestion, commandSuggestionLimit+1), commandSuggestionLimit, ""},
		{"oversized text", []packets.CommandSuggestion{{Text: ns.String(strings.Repeat("x", commandTextLimit+1))}, {Text: "intact "}}, 1, "intact "},
		{"oversized tooltip", []packets.CommandSuggestion{{Text: "omit", Tooltip: ns.PrefixedOptional[ns.TextComponent]{Present: true, Value: ns.TextComponent{Text: strings.Repeat("x", commandTextLimit+1)}}}, {Text: "intact "}}, 1, "intact "},
		{"aggregate bytes", func() []packets.CommandSuggestion {
			matches := make([]packets.CommandSuggestion, commandSuggestionBytes/commandTextLimit+1)
			for i := range matches {
				matches[i].Text = ns.String(strings.Repeat("x", commandTextLimit))
			}
			return matches
		}(), commandSuggestionBytes / commandTextLimit, strings.Repeat("x", commandTextLimit)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.bindCompletion(func(context.Context, string) (packets.S2CCommandSuggestions, error) {
				return packets.S2CCommandSuggestions{Start: 4, Matches: tc.matches}, nil
			})
			value, err := s.complete(context.Background(), consoleActor, "/pw ")
			if err != nil {
				t.Fatal(err)
			}
			r := value.(commandCompletionResult)
			if !r.Truncated || len(r.Suggestions) != tc.want || r.Suggestions[0].Text != tc.first {
				t.Fatalf("completion bounds changed entries or lost truncation: %+v", r)
			}
		})
	}
}

func TestServerCompletionCancellationAndRebind(t *testing.T) {
	for _, rebind := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "connection replacement"}[rebind], func(t *testing.T) {
			s := newServerCommands(commandStore(t))
			s.bind(func(context.Context, string) error { return nil })
			defer s.close()
			started := make(chan context.Context, 1)
			s.bindCompletion(func(ctx context.Context, _ string) (packets.S2CCommandSuggestions, error) {
				started <- ctx
				<-ctx.Done()
				return packets.S2CCommandSuggestions{}, ctx.Err()
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := s.complete(ctx, consoleActor, "/pw "); done <- err }()
			submissionCtx := <-started
			replacementCalls := 0
			if rebind {
				s.bind(func(context.Context, string) error { return nil })
				if submissionCtx.Err() != context.Canceled {
					t.Fatal("replacement did not synchronously invalidate old submission context")
				}
				s.bindCompletion(func(context.Context, string) (packets.S2CCommandSuggestions, error) {
					replacementCalls++
					return packets.S2CCommandSuggestions{}, nil
				})
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("completion cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("completion cancellation did not clean up callback")
			}
			if replacementCalls != 0 {
				t.Fatal("old request submitted on replacement connection")
			}
			if rebind {
				if _, err := s.complete(context.Background(), consoleActor, "/pw "); err != nil || replacementCalls != 1 {
					t.Fatalf("fresh request did not use replacement: %v, calls=%d", err, replacementCalls)
				}
				s.bind(nil)
				if _, err := s.complete(context.Background(), consoleActor, "/pw "); err == nil || replacementCalls != 1 {
					t.Fatal("disconnected completion reached old callback")
				}
			}
		})
	}
}

func TestCompletionCancelledWhileWaitingForChatTransport(t *testing.T) {
	chat := &synchronizedChat{ctx: context.Background(), gate: make(chan struct{}, 1)}
	chat.gate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := chat.completeCommand(ctx, "/pw ", nil); done <- err }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled queued completion: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("completion remained stuck behind packet handling")
	}
}
