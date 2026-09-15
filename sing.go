package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type lyricLine struct {
	at   time.Duration
	text string
}

type songState struct {
	State    string `json:"state"`
	Owner    string `json:"owner,omitempty"`
	Track    string `json:"track,omitempty"`
	Artist   string `json:"artist,omitempty"`
	Synced   bool   `json:"synced"`
	Voice    bool   `json:"voice"`
	Fallback string `json:"fallback,omitempty"`
	Error    string `json:"error,omitempty"`
}

type songSession struct {
	state  songState // Protected by activities.mu.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	voice  *voiceChat
}

// A song belongs to the Minecraft connection on which it was requested.
func (a *activities) bindVoice(voice *voiceChat) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.voice != voice && a.song != nil {
		a.song.cancel()
	}
	a.voice = voice
}

func (a *activities) sing(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	query := strings.TrimSpace(args.Query)
	if query == "" || len(query) > 500 {
		return nil, fmt.Errorf("provide a song title and optional artist of at most 500 bytes")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return nil, fmt.Errorf("activities are closed")
	}
	if a.song != nil {
		a.mu.Unlock()
		return nil, fmt.Errorf("a song is already loading or playing; stop it first")
	}
	songCtx, cancel := context.WithCancel(a.ctx)
	session := &songSession{state: songState{State: "loading", Owner: actor}, ctx: songCtx, cancel: cancel, done: make(chan struct{}), voice: a.voice}
	a.song = session
	// Reservation and Add are atomic with close's closed check and Wait.
	a.wg.Add(1)
	a.mu.Unlock()

	// The initiating request cancels the search, but not successful background playback.
	stopRequestCancel := context.AfterFunc(ctx, cancel)
	lyrics, track, artist, synced, duration, err := a.fetchLyrics(songCtx, query)
	stopRequestCancel()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = songCtx.Err()
	}
	if err != nil {
		a.finishSong(session, err)
		return nil, err
	}
	a.mu.Lock()
	if err := songCtx.Err(); err != nil {
		a.mu.Unlock()
		a.finishSong(session, err)
		return nil, err
	}
	session.state.Track = track
	session.state.Artist = artist
	session.state.Synced = synced
	state := session.state
	a.mu.Unlock()
	go func() { a.finishSong(session, a.startSong(session, lyrics, track, artist, duration)) }()
	return state, nil
}

func (a *activities) stopSinging(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	admin := a.store.hasRank(actor, "admin")
	a.mu.Lock()
	session := a.song
	if session == nil {
		a.mu.Unlock()
		return map[string]any{"state": "idle", "stopped": false}, nil
	}
	if !strings.EqualFold(actor, session.state.Owner) && !admin {
		a.mu.Unlock()
		return nil, fmt.Errorf("only the song's owner or an admin may stop singing")
	}
	session.cancel()
	a.mu.Unlock()
	// Wait outside the lock so no old lyrics can leak into a replacement song.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-session.done:
		a.mu.Lock()
		state := session.state
		a.mu.Unlock()
		return state, nil
	}
}

func (a *activities) singingStatus(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.song != nil {
		return a.song.state, nil
	}
	if a.lastSong.State == "" {
		return songState{State: "idle"}, nil
	}
	return a.lastSong, nil
}

func (a *activities) finishSong(session *songSession, err error) {
	a.mu.Lock()
	switch {
	case session.ctx.Err() != nil:
		session.state.State = "stopped"
	case err != nil:
		session.state.State = "failed"
		session.state.Error = err.Error()
	default:
		session.state.State = "finished"
	}
	a.lastSong = session.state
	if a.song == session {
		a.song = nil
	}
	a.mu.Unlock()
	if err != nil && session.ctx.Err() == nil {
		log.Printf("Singing failed: %v", err)
	}
	session.cancel()
	close(session.done)
	a.wg.Done()
}

func (a *activities) startSong(session *songSession, lyrics []lyricLine, track, artist string, duration time.Duration) error {
	var voice *voiceSession
	var audio *songAudio
	var err error
	audioCtx, stopAudio := context.WithCancel(session.ctx)
	defer stopAudio()
	if session.voice == nil {
		err = fmt.Errorf("voice chat is not connected")
	} else {
		voice, err = session.voice.connect(session.ctx)
	}
	if err == nil {
		audio, err = openSongAudio(audioCtx, track, artist, duration)
	}
	if session.ctx.Err() != nil {
		if audio != nil {
			_ = audio.Close()
		}
		return session.ctx.Err()
	}
	a.mu.Lock()
	session.state.State = "playing"
	session.state.Voice = err == nil
	if err != nil {
		session.state.Fallback = err.Error()
	}
	a.mu.Unlock()
	if err != nil {
		log.Printf("Singing %q by %s: chat-only fallback: %v", track, artist, err)
		return a.playSong(session.ctx, lyrics, time.Now())
	}
	defer audio.Close()
	log.Printf("Singing %q by %s over voice chat", track, artist)
	return a.playVoiceSong(session, lyrics, audio.Next, voice.sendAudio, stopAudio)
}

// Lyrics follow successfully transmitted audio positions, not media preparation
// time. Chat backpressure cannot stop the 20ms audio sender.
func (a *activities) playVoiceSong(session *songSession, lyrics []lyricLine, next func() ([]byte, error), send func([]byte) error, stopAudio context.CancelFunc) error {
	ctx, cancel := context.WithCancel(session.ctx)
	progress := make(chan time.Duration, 1)
	type result struct {
		at  time.Duration
		err error
	}
	done := make(chan result, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		at, err := streamSongAudio(ctx, next, send, progress)
		done <- result{at, err}
	}()
	// Join the sender before allowing a replacement song to use this session.
	defer func() { cancel(); stopAudio(); <-stopped }()
	for {
		var at time.Duration
		var ended *result
		select {
		case <-ctx.Done():
			return ctx.Err()
		case at = <-progress:
		case result := <-done:
			ended = &result
			at = result.at
		}
		for len(lyrics) > 0 && lyrics[0].at <= at {
			if err := a.send(ctx, lyrics[0].text); err != nil {
				return fmt.Errorf("send lyrics: %w", err)
			}
			lyrics = lyrics[1:]
		}
		if ended != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if ended.err != nil {
				a.mu.Lock()
				session.state.Voice = false
				session.state.Fallback = ended.err.Error()
				a.mu.Unlock()
				log.Printf("Voice playback stopped; continuing lyrics in chat: %v", ended.err)
			}
			// Preserve the current lyric position even if UDP or decoding fails.
			return a.playSong(ctx, lyrics, time.Now().Add(-at))
		}
	}
}

func streamSongAudio(ctx context.Context, next func() ([]byte, error), send func([]byte) error, progress chan time.Duration) (time.Duration, error) {
	const frameTime = 20 * time.Millisecond
	var position time.Duration
	deadline := time.Now()
	defer func() { _ = send(nil) }()
	for {
		if err := ctx.Err(); err != nil {
			return position, err
		}
		frame, err := next()
		if errors.Is(err, io.EOF) {
			return position, waitSongTime(ctx, deadline)
		}
		if err != nil {
			return position, err
		}
		if err := waitSongTime(ctx, deadline); err != nil {
			return position, err
		}
		if err := send(frame); err != nil {
			return position, err
		}
		// Keep the latest audio position if chat is busy sending the previous line.
		select {
		case <-progress:
		default:
		}
		progress <- position
		position += frameTime
		// Never burst overdue packets after a stalled decoder/network read.
		deadline = deadline.Add(frameTime)
		if now := time.Now(); deadline.Before(now) {
			deadline = now.Add(frameTime)
		}
	}
}

func waitSongTime(ctx context.Context, deadline time.Time) error {
	if delay := time.Until(deadline); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (a *activities) playSong(ctx context.Context, lyrics []lyricLine, start time.Time) error {
	for _, line := range lyrics {
		if err := ctx.Err(); err != nil {
			return err
		}
		if delay := time.Until(start.Add(line.at)); delay > 0 {
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
		if err := a.send(ctx, line.text); err != nil {
			return fmt.Errorf("send lyrics: %w", err)
		}
	}
	return nil
}

func (a *activities) fetchLyrics(ctx context.Context, query string) ([]lyricLine, string, string, bool, time.Duration, error) {
	var tracks []struct {
		Track    string  `json:"trackName"`
		Artist   string  `json:"artistName"`
		Synced   string  `json:"syncedLyrics"`
		Plain    string  `json:"plainLyrics"`
		Duration float64 `json:"duration"`
	}
	if err := a.getJSON(ctx, "https://lrclib.net/api/search?q="+url.QueryEscape(query), &tracks); err != nil {
		return nil, "", "", false, 0, err
	}
	for _, track := range tracks {
		if lyrics := parseLRC(track.Synced); len(lyrics) != 0 {
			return lyrics, track.Track, track.Artist, true, time.Duration(track.Duration * float64(time.Second)), nil
		}
	}
	for _, track := range tracks {
		if lyrics := parsePlainLyrics(track.Plain); len(lyrics) != 0 {
			return lyrics, track.Track, track.Artist, false, time.Duration(track.Duration * float64(time.Second)), nil
		}
	}
	return nil, "", "", false, 0, fmt.Errorf("no usable lyrics found for %q", query)
}

// LRC permits multiple timestamps per line and fractional seconds of varying precision.
func parseLRC(lrc string) []lyricLine {
	var lyrics []lyricLine
	var offset time.Duration
	for line := range strings.SplitSeq(lrc, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[offset:") && strings.HasSuffix(line, "]") {
			ms, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(line, "[offset:"), "]"), 10, 32)
			if err == nil {
				offset = time.Duration(ms) * time.Millisecond
			}
			continue
		}
		first := len(lyrics)
		for strings.HasPrefix(line, "[") {
			end := strings.IndexByte(line, ']')
			if end < 0 {
				break
			}
			tag := line[1:end]
			line = strings.TrimSpace(line[end+1:])
			minuteText, secondText, ok := strings.Cut(tag, ":")
			if !ok {
				continue
			}
			minutes, err := strconv.Atoi(minuteText)
			if err != nil || minutes < 0 || minutes > 1440 {
				continue
			}
			seconds, err := time.ParseDuration(secondText + "s")
			if err != nil || seconds < 0 || seconds >= time.Minute {
				continue
			}
			lyrics = append(lyrics, lyricLine{at: time.Duration(minutes)*time.Minute + seconds})
		}
		if line == "" {
			lyrics = lyrics[:first]
			continue
		}
		for i := first; i < len(lyrics); i++ {
			lyrics[i].text = line
		}
	}
	for i := range lyrics {
		// A positive LRC offset means the lyrics should be displayed earlier.
		lyrics[i].at = max(0, lyrics[i].at-offset)
	}
	sort.SliceStable(lyrics, func(i, j int) bool { return lyrics[i].at < lyrics[j].at })
	return lyrics
}

func parsePlainLyrics(plain string) []lyricLine {
	var lyrics []lyricLine
	var at time.Duration
	for line := range strings.SplitSeq(plain, "\n") {
		if text := strings.TrimSpace(line); text != "" {
			lyrics = append(lyrics, lyricLine{at: at, text: text})
			at += 2 * time.Second
		}
	}
	return lyrics
}
