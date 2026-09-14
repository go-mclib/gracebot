package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	s.bind(func(context.Context, string) error { return nil })
	defer s.close()
	for _, command := range []string{"help\nstop", "help\rstop", "help\x00", "help\tstop", "help\u2028stop", "//help", "", strings.Repeat("x", 2049), "help\u202estop"} {
		if _, err := s.execute(context.Background(), "alice", command); err == nil {
			t.Errorf("unsafe command accepted: %q", command)
		}
	}
	for _, command := range []string{"stop", "minecraft:help", "help @a", "list uuids", "plugins reload", "help a b", "version --reload"} {
		if _, err := s.execute(context.Background(), "alice", command); err == nil {
			t.Errorf("untrusted command accepted: %q", command)
		}
	}
	for _, command := range []string{"/help", "plugins", "help 2", "help teleport", "version Essentials", "list"} {
		value, err := s.execute(context.Background(), "alice", command)
		if err != nil {
			t.Fatal(err)
		}
		r := value.(commandResult)
		if !r.Submitted || r.Confirmed {
			t.Fatalf("dishonest result: %+v", r)
		}
	}
	stopped := false
	s.BeforeMutation = func() { stopped = true }
	if _, err := s.execute(context.Background(), consoleActor, "time query daytime"); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		t.Fatal("mutating command did not stop physical action")
	}
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
	value, err := s.execute(context.Background(), "alice", "help")
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
