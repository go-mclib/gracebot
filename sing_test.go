package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"
)

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
