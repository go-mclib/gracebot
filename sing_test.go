package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestUnrankedSingingPreservesOwnershipAndAdminStop(t *testing.T) {
	lyrics := make(chan string, 2)
	a := newActivities(commandStore(t), func(ctx context.Context, text string) error {
		select {
		case lyrics <- text:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	defer a.close()
	a.client.Transport = webTestTransport(func(r *http.Request) (*http.Response, error) {
		return webTestResponse(r, http.StatusOK, "application/json", `[{"trackName":"Song","artistName":"Artist","syncedLyrics":"[00:00]first lyric\n[01:00]later lyric","duration":120}]`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	args := json.RawMessage(`{"query":"Song"}`)
	for _, stopper := range []string{"Alice", consoleActor} {
		if _, err := a.sing(ctx, "Alice", args); err != nil {
			t.Fatalf("unranked singing rejected: %v", err)
		}
		select {
		case text := <-lyrics:
			if text != "first lyric" {
				t.Fatalf("unexpected sung lyric: %q", text)
			}
		case <-ctx.Done():
			t.Fatal("accepted song did not begin playback")
		}
		if _, err := a.sing(ctx, "Bob", args); err == nil {
			t.Fatal("another actor replaced the active song")
		}
		if _, err := a.stopSinging(ctx, "Bob", json.RawMessage(`{}`)); err == nil {
			t.Fatal("another actor stopped the active song")
		}
		value, err := a.stopSinging(ctx, stopper, json.RawMessage(`{}`))
		if err != nil {
			t.Fatalf("%s could not stop song: %v", stopper, err)
		}
		if state := value.(songState); state.State != "stopped" || state.Owner != "Alice" {
			t.Fatalf("stop did not retain song ownership: %+v", state)
		}
	}
}

func TestVoiceSongFollowsAudioAndFallsBackWithoutRepeatingLyrics(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session := &songSession{ctx: ctx, cancel: cancel, state: songState{Voice: true}}
	firstFrame := make(chan struct{})
	chatStarted := make(chan struct{})
	voiceFailed := make(chan struct{})
	var messages []string
	a := &activities{send: func(ctx context.Context, text string) error {
		if len(messages) == 0 {
			select {
			case <-firstFrame:
			default:
				t.Error("lyrics preceded the first audio frame")
			}
			close(chatStarted)
			// Simulate Minecraft chat pacing. Audio must still advance and fail
			// while the lyric sender is blocked, rather than stuttering with it.
			select {
			case <-voiceFailed:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		messages = append(messages, text)
		return nil
	}}
	frames := 0
	next := func() ([]byte, error) { return []byte{1}, nil }
	send := func(frame []byte) error {
		if len(frame) == 0 {
			return nil
		}
		frames++
		if frames == 1 {
			close(firstFrame)
		}
		if frames == 3 {
			select {
			case <-chatStarted:
			case <-ctx.Done():
				return ctx.Err()
			}
			close(voiceFailed)
			return errors.New("voice connection lost")
		}
		return nil
	}
	lyrics := []lyricLine{{0, "first"}, {20 * time.Millisecond, "second"}, {80 * time.Millisecond, "third"}}
	if err := a.playVoiceSong(session, lyrics, next, send, func() {}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages, []string{"first", "second", "third"}) {
		t.Fatalf("fallback lost or repeated lyrics: %q", messages)
	}
	if session.state.Voice || session.state.Fallback != "voice connection lost" {
		t.Fatalf("status hid voice failure: %+v", session.state)
	}
	if ctx.Err() != nil {
		t.Fatal("completing playback cancelled the song session")
	}
}

func TestStoppingVoiceSongJoinsBlockedDecoder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &songSession{ctx: ctx, cancel: cancel}
	reading := make(chan struct{})
	decoderStopped := make(chan struct{})
	ended := make(chan struct{})
	var once sync.Once
	next := func() ([]byte, error) {
		close(reading)
		<-decoderStopped
		return nil, io.EOF
	}
	send := func(frame []byte) error {
		if len(frame) == 0 {
			close(ended)
		}
		return nil
	}
	a := &activities{send: func(context.Context, string) error {
		t.Error("cancelled decoder emitted lyrics")
		return nil
	}}
	done := make(chan error, 1)
	go func() {
		done <- a.playVoiceSong(session, []lyricLine{{0, "not played"}}, next, send, func() {
			once.Do(func() { close(decoderStopped) })
		})
	}()
	<-reading
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop returned %v", err)
		}
		select {
		case <-ended:
		default:
			t.Fatal("song returned before the old voice sender ended")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop left the decoder blocked")
	}
}

func TestSongWithoutVoiceKeepsChatFallback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	session := &songSession{ctx: ctx, cancel: cancel}
	var messages []string
	a := &activities{send: func(_ context.Context, text string) error {
		messages = append(messages, text)
		return nil
	}}
	if err := a.startSong(session, []lyricLine{{0, "lyrics only"}}, "song", "artist", 0); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(messages, []string{"lyrics only"}) || session.state.Voice || session.state.Fallback == "" {
		t.Fatalf("missing chat-only fallback: messages=%q state=%+v", messages, session.state)
	}
}
