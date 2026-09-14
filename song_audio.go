package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
)

// songAudio owns both processes and their pipes. Next is serialized; Close may
// interrupt it from another goroutine. Packets remain valid until the next Next call.
type songAudio struct {
	ctx       context.Context
	cancel    context.CancelFunc
	processes []*songAudioProcess
	output    *os.File
	packets   *songOggReader
	stopRead  func() bool
	closeOnce sync.Once
	nextMu    sync.Mutex
	first     []byte
	terminal  error
}

func openSongAudio(ctx context.Context, track, artist string, duration time.Duration) (*songAudio, error) {
	if strings.TrimSpace(track) == "" {
		return nil, errors.New("audio search requires a track title")
	}
	for _, name := range []string{"yt-dlp", "ffmpeg"} {
		if _, err := exec.LookPath(name); err != nil {
			return nil, fmt.Errorf("song audio requires %s on PATH: %w", name, err)
		}
	}
	var failures []error
	for _, source := range []string{"YouTube", "SoundCloud"} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		target, err := findSongRecording(ctx, source, track, artist, duration)
		if err == nil {
			var audio *songAudio
			audio, err = openSongAudioURL(ctx, target)
			if err == nil {
				return audio, nil
			}
		}
		failures = append(failures, fmt.Errorf("%s: %w", source, err))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.Join(failures...)
}

func openSongAudioURL(ctx context.Context, target string) (*songAudio, error) {
	audioCtx, cancel := context.WithCancelCause(ctx)
	a := &songAudio{ctx: audioCtx, cancel: func() { cancel(context.Canceled) }}
	// Bound initial network/encoder stalls without imposing a playback deadline.
	startup := time.AfterFunc(45*time.Second, func() { cancel(context.DeadlineExceeded) })
	defer startup.Stop()
	ok := false
	defer func() {
		if !ok {
			a.Close()
		}
	}()
	input, download, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create audio input pipe: %w", err)
	}
	defer input.Close()
	defer download.Close()
	output, encoded, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create audio output pipe: %w", err)
	}
	a.output = output
	defer encoded.Close()
	a.stopRead = context.AfterFunc(audioCtx, func() { output.Close() })

	encoder := newSongAudioProcess(audioCtx, "ffmpeg", "-hide_banner", "-loglevel", "error", "-nostdin",
		"-i", "pipe:0", "-map", "0:a:0", "-vn", "-map_metadata", "-1", "-ac", "1", "-ar", "48000",
		"-c:a", "libopus", "-application", "audio", "-b:a", "64k", "-vbr", "off", "-frame_duration", "20",
		"-f", "ogg", "-page_duration", "20000", "-flush_packets", "1", "pipe:1")
	encoder.cmd.Stdin, encoder.cmd.Stdout = input, encoded
	if err := encoder.start(); err != nil {
		return nil, fmt.Errorf("start ffmpeg: %w", err)
	}
	a.processes = append(a.processes, encoder)
	input.Close()
	encoded.Close()

	fetch := newSongAudioProcess(audioCtx, "yt-dlp", "--ignore-config", "--no-playlist", "--quiet", "--no-progress",
		"--socket-timeout", "15", "--retries", "2", "--fragment-retries", "2", "--no-part", "--no-cache-dir",
		"-f", "bestaudio/best", "-o", "-", "--", target)
	fetch.cmd.Stdout = download
	if err := fetch.start(); err != nil {
		return nil, fmt.Errorf("start yt-dlp: %w", err)
	}
	a.processes = append(a.processes, fetch)
	download.Close()
	a.packets = &songOggReader{r: output}
	first, err := a.Next()
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = errors.New("encoder produced no audio frames")
		}
		if ctx.Err() == nil && errors.Is(context.Cause(audioCtx), context.DeadlineExceeded) {
			return nil, errors.New("song audio startup timed out after 45 seconds")
		}
		return nil, fmt.Errorf("prepare song audio: %w", err)
	}
	startup.Stop()
	if err := audioCtx.Err(); err != nil {
		return nil, err
	}
	a.first = first
	ok = true
	return a, nil
}

func (a *songAudio) Next() ([]byte, error) {
	a.nextMu.Lock()
	defer a.nextMu.Unlock()
	if a.terminal != nil {
		return nil, a.terminal
	}
	if err := a.ctx.Err(); err != nil {
		return nil, err
	}
	if a.first != nil {
		packet := a.first
		a.first = nil
		return packet, nil
	}
	packet, err := a.packets.next()
	if err == nil {
		if err := a.ctx.Err(); err != nil {
			return nil, err
		}
		return packet, nil
	}
	// A damaged stream may leave the producer blocked writing: kill first.
	if !errors.Is(err, io.EOF) {
		a.cancel()
	}
	for _, process := range a.processes {
		<-process.done
	}
	// Prefer downloader diagnostics over ffmpeg's resulting invalid-input error.
	for i := len(a.processes) - 1; i >= 0; i-- {
		if failure := a.processes[i].failure(); failure != nil {
			if !errors.Is(err, io.EOF) {
				err = fmt.Errorf("%w; %v", err, failure)
			} else {
				err = failure
			}
			break
		}
	}
	a.terminal = err
	return nil, err
}

func (a *songAudio) Close() error {
	a.closeOnce.Do(func() {
		a.cancel()
		if a.stopRead != nil {
			a.stopRead()
		}
		if a.output != nil {
			a.output.Close()
		}
		for _, process := range a.processes {
			<-process.done
		}
	})
	return nil
}

var songVideoID = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

type songVideo struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Channel    string  `json:"channel"`
	Uploader   string  `json:"uploader"`
	WebpageURL string  `json:"webpage_url"`
	URL        string  `json:"url"`
	Duration   float64 `json:"duration"`
	LiveStatus string  `json:"live_status"`
	IsLive     bool    `json:"is_live"`
}

func findSongRecording(ctx context.Context, source, track, artist string, duration time.Duration) (string, error) {
	if source == "SoundCloud" && (duration <= 0 || strings.TrimSpace(artist) == "") {
		return "", errors.New("safe SoundCloud matching requires lyrics artist and duration metadata")
	}
	searchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Always a search over canonical lyrics metadata, never a user-supplied URL.
	prefix, suffix := "ytsearch10:", " official audio"
	if source == "SoundCloud" {
		prefix, suffix = "scsearch10:", ""
	}
	query := prefix + strings.Join(strings.Fields(artist+" "+track+suffix), " ")
	args := []string{"--ignore-config", "--no-playlist", "--flat-playlist", "--dump-single-json",
		"--quiet", "--no-progress", "--socket-timeout", "15", "--no-cache-dir"}
	// Public searches can include inaccessible/DRM entries; skip, never unlock.
	if source == "SoundCloud" {
		args = append(args, "--ignore-errors")
	}
	args = append(args, "--", query)
	process := newSongAudioProcess(searchCtx, "yt-dlp", args...)
	result := &songLimitedOutput{limit: 1 << 20}
	process.cmd.Stdout = result
	if err := process.start(); err != nil {
		return "", fmt.Errorf("start yt-dlp search: %w", err)
	}
	<-process.done
	if err := searchCtx.Err(); err != nil {
		return "", fmt.Errorf("yt-dlp search: %w", err)
	}
	searchFailure := process.failure()
	if searchFailure != nil && source != "SoundCloud" {
		return "", searchFailure
	}
	if result.exceeded {
		return "", errors.New("yt-dlp search exceeded metadata limit")
	}
	var results struct {
		Entries []songVideo `json:"entries"`
	}
	if err := json.Unmarshal(result.data, &results); err != nil {
		if searchFailure != nil {
			return "", searchFailure
		}
		return "", errors.New("yt-dlp returned invalid search metadata")
	}
	bestURL, bestScore := "", math.Inf(-1)
	for _, video := range results.Entries {
		if video.IsLive || video.LiveStatus == "is_live" || video.LiveStatus == "is_upcoming" {
			continue
		}
		// A differently edited recording cannot share this lyric timeline.
		if duration > 0 && (video.Duration <= 0 || math.Abs(video.Duration-duration.Seconds()) > math.Max(3, duration.Seconds()*0.01)) {
			continue
		}
		var target string
		if source == "YouTube" {
			if !songVideoID.MatchString(video.ID) {
				continue
			}
			target = "https://www.youtube.com/watch?v=" + video.ID
		} else {
			target = matchingSoundCloudURL(video, track, artist, duration)
			if target == "" {
				continue
			}
		}
		score := songVideoScore(video, track, artist, duration)
		if score > bestScore {
			bestURL, bestScore = target, score
		}
	}
	if bestURL == "" {
		return "", errors.Join(fmt.Errorf("yt-dlp found no matching playable %s recordings for the selected song", source), searchFailure)
	}
	return bestURL, nil
}

func matchingSoundCloudURL(video songVideo, track, artist string, duration time.Duration) string {
	if duration <= 0 || video.Duration <= 0 || math.Abs(video.Duration-duration.Seconds()) > math.Max(3, duration.Seconds()*0.01) {
		return ""
	}
	normalize := func(value string) string {
		return strings.Join(strings.FieldsFunc(strings.ToLower(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }), " ")
	}
	title, wantedTrack, wantedArtist := normalize(video.Title), normalize(track), normalize(artist)
	if wantedTrack == "" || wantedArtist == "" || !strings.Contains(" "+title+" ", " "+wantedTrack+" ") {
		return ""
	}
	credit := normalize(video.Title + " " + video.Channel + " " + video.Uploader)
	if !strings.Contains(" "+credit+" ", " "+wantedArtist+" ") {
		return ""
	}
	// Matching length alone is insufficient: covers and remixes often reuse it.
	for _, variant := range []string{"live", "cover", "karaoke", "remix", "slowed", "sped up", "nightcore", "instrumental", "reaction", "extended"} {
		if strings.Contains(" "+title+" ", " "+variant+" ") && !strings.Contains(" "+wantedTrack+" ", " "+variant+" ") {
			return ""
		}
	}
	target := video.WebpageURL
	if target == "" {
		target = video.URL
	}
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || (parsed.Host != "soundcloud.com" && parsed.Host != "www.soundcloud.com") {
		return ""
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "sets" || parts[0] == "discover" || parts[1] == "sets" {
		return ""
	}
	// Drop tracking parameters and never forward extractor-provided credentials.
	return "https://soundcloud.com/" + url.PathEscape(parts[0]) + "/" + url.PathEscape(parts[1])
}

func songVideoScore(video songVideo, track, artist string, duration time.Duration) float64 {
	title := strings.ToLower(video.Title)
	score := 0.0
	if strings.Contains(title, strings.ToLower(track)) {
		score += 12
	}
	if artist != "" && strings.Contains(strings.ToLower(video.Title+" "+video.Channel), strings.ToLower(artist)) {
		score += 8
	}
	if strings.Contains(title, "official audio") {
		score += 12
	}
	if strings.HasSuffix(strings.ToLower(video.Channel), " - topic") {
		score += 12
	}
	if strings.Contains(title, "studio") {
		score += 5
	}
	if strings.Contains(title, "official video") || strings.Contains(title, "music video") {
		score -= 3
	}
	for _, variant := range []string{"live", "cover", "karaoke", "remix", "slowed", "sped up", "nightcore", "instrumental", "reaction", "extended"} {
		if strings.Contains(title, variant) && !strings.Contains(strings.ToLower(track), variant) {
			score -= 25
		}
	}
	if duration > 0 {
		if video.Duration > 0 {
			difference := math.Abs(video.Duration - duration.Seconds())
			score -= math.Min(difference, 300)
			if difference <= 3 {
				score += 20
			}
		} else {
			score -= 20
		}
	}
	return score
}

// Fixed-size diagnostics are classified rather than echoed: yt-dlp errors can
// contain signed media URLs, cookie values, or tokens.
type songAudioLog struct {
	mu   sync.Mutex
	data []byte
}

func (l *songAudioLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := len(p)
	if len(p) >= 8192 {
		l.data = append(l.data[:0], p[len(p)-8192:]...)
	} else {
		if excess := len(l.data) + len(p) - 8192; excess > 0 {
			l.data = append(l.data[:0], l.data[excess:]...)
		}
		l.data = append(l.data, p...)
	}
	return n, nil
}

func (l *songAudioLog) detail() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	text := strings.ToLower(string(l.data))
	for _, reason := range []struct{ marker, detail string }{
		{"drm", "some recordings are DRM-protected and cannot be used; only public playable recordings are supported"},
		{"sign in to confirm", "YouTube requires sign-in or has blocked this host as a bot"},
		{"page needs to be reloaded", "YouTube rejected playback; check the yt-dlp version and JavaScript runtime"},
		{"javascript runtime", "yt-dlp needs a supported JavaScript runtime (for example Deno) on PATH"},
		{"requested format is not available", "YouTube did not offer a usable audio format; update yt-dlp and check its JavaScript runtime"},
		{"http error 403", "YouTube denied the media request (HTTP 403)"},
		{"http error 429", "YouTube rate-limited this host (HTTP 429)"},
		{"video unavailable", "YouTube reports that the selected recording is unavailable"},
		{"not available in your country", "the selected recording is region restricted"},
		{"timed out", "the media request timed out"},
		{"unable to download", "yt-dlp could not download the selected media; check network and YouTube access"},
		{"unknown encoder", "ffmpeg lacks the required libopus encoder"},
		{"invalid data found", "ffmpeg received invalid or incomplete media"},
		{"unrecognized option", "the installed media binary does not support a required option; update it"},
	} {
		if strings.Contains(text, reason.marker) {
			return reason.detail
		}
	}
	return "check installed yt-dlp/ffmpeg versions, a supported JavaScript runtime, and host access to YouTube"
}

type songLimitedOutput struct {
	data     []byte
	limit    int
	exceeded bool
}

func (w *songLimitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if n > w.limit-len(w.data) {
		w.exceeded = true
		p = p[:w.limit-len(w.data)]
	}
	w.data = append(w.data, p...)
	return n, nil
}

type songAudioProcess struct {
	cmd  *exec.Cmd
	log  songAudioLog
	done chan struct{}
	err  error
}

func newSongAudioProcess(ctx context.Context, name string, args ...string) *songAudioProcess {
	p := &songAudioProcess{cmd: exec.CommandContext(ctx, name, args...), done: make(chan struct{})}
	p.cmd.Stderr = &p.log
	// yt-dlp may launch a JS runtime or downloader. Cancel the whole group, not
	// only the Python parent, so inherited pipe descriptors cannot keep reads alive.
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	p.cmd.Cancel = func() error {
		err := syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	p.cmd.WaitDelay = 2 * time.Second
	return p
}

func (p *songAudioProcess) start() error {
	if err := p.cmd.Start(); err != nil {
		return err
	}
	go func() {
		p.err = p.cmd.Wait()
		// Reap has completed; remove any helper left in this process group.
		_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
		close(p.done)
	}()
	return nil
}

func (p *songAudioProcess) failure() error {
	if p.err == nil {
		return nil
	}
	return fmt.Errorf("%s failed (%v): %s", p.cmd.Args[0], p.err, p.log.detail())
}

// songOggReader accepts a single, checksummed Ogg Opus stream. Pages and packets
// have hard limits; continued packets never turn into unbounded allocations.
type songOggReader struct {
	r         io.Reader
	page      [65307]byte
	packet    [65536]byte
	packetLen int
	segments  []byte
	body      []byte
	serial    uint32
	sequence  uint32
	started   bool
	ended     bool
	headers   int
}

func (r *songOggReader) next() ([]byte, error) {
	for {
		packet, err := r.nextPacket()
		if err != nil {
			if errors.Is(err, io.EOF) && r.headers != 2 {
				return nil, errors.New("Ogg stream is missing Opus headers")
			}
			return nil, err
		}
		switch r.headers {
		case 0:
			if len(packet) != 19 || !bytes.Equal(packet[:8], []byte("OpusHead")) || packet[8] != 1 || packet[9] != 1 || packet[18] != 0 {
				return nil, errors.New("expected a mono Opus identification header")
			}
			r.headers++
		case 1:
			if err := checkSongOpusTags(packet); err != nil {
				return nil, err
			}
			r.headers++
		default:
			if !songOpus20ms(packet) {
				return nil, errors.New("encoder produced an invalid or non-20ms mono Opus packet")
			}
			return packet, nil
		}
	}
}

func (r *songOggReader) nextPacket() ([]byte, error) {
	// The previous packet's slice is valid only until the next call.
	if r.packetLen < 0 {
		r.packetLen = 0
	}
	for {
		if len(r.segments) == 0 {
			if err := r.readPage(); err != nil {
				return nil, err
			}
			continue
		}
		n := int(r.segments[0])
		r.segments = r.segments[1:]
		if n > len(r.packet)-r.packetLen {
			return nil, errors.New("Ogg packet exceeds 64 KiB")
		}
		copy(r.packet[r.packetLen:], r.body[:n])
		r.packetLen += n
		r.body = r.body[n:]
		if n < 255 {
			packet := r.packet[:r.packetLen]
			r.packetLen = -1
			return packet, nil
		}
	}
}

func (r *songOggReader) readPage() error {
	if r.ended {
		if r.packetLen > 0 {
			return errors.New("Ogg ends with an incomplete packet")
		}
		var trailing [1]byte
		_, err := io.ReadFull(r.r, trailing[:])
		if errors.Is(err, io.EOF) {
			return io.EOF
		}
		if err != nil {
			return err
		}
		return errors.New("unexpected data after Ogg end-of-stream")
	}
	if _, err := io.ReadFull(r.r, r.page[:27]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("truncated Ogg stream before end-of-stream")
		}
		return err
	}
	if string(r.page[:4]) != "OggS" || r.page[4] != 0 || r.page[5]&^byte(7) != 0 {
		return errors.New("invalid Ogg page header")
	}
	flags := r.page[5]
	serial, sequence := binary.LittleEndian.Uint32(r.page[14:18]), binary.LittleEndian.Uint32(r.page[18:22])
	if !r.started {
		if flags&2 == 0 || flags&1 != 0 || sequence != 0 {
			return errors.New("invalid first Ogg page")
		}
		r.serial, r.started = serial, true
	} else if serial != r.serial || sequence != r.sequence || flags&2 != 0 {
		return errors.New("Ogg stream serial or page sequence changed")
	}
	if (flags&1 != 0) != (r.packetLen > 0) {
		return errors.New("invalid Ogg packet continuation")
	}
	r.sequence = sequence + 1
	count := int(r.page[26])
	if _, err := io.ReadFull(r.r, r.page[27:27+count]); err != nil {
		return fmt.Errorf("truncated Ogg lacing table: %w", err)
	}
	size := 0
	for _, n := range r.page[27 : 27+count] {
		size += int(n)
	}
	end := 27 + count + size
	if _, err := io.ReadFull(r.r, r.page[27+count:end]); err != nil {
		return fmt.Errorf("truncated Ogg page body: %w", err)
	}
	checksum := binary.LittleEndian.Uint32(r.page[22:26])
	clear(r.page[22:26])
	if songOggChecksum(r.page[:end]) != checksum {
		return errors.New("Ogg page checksum mismatch")
	}
	r.segments, r.body = r.page[27:27+count], r.page[27+count:end]
	r.ended = flags&4 != 0
	return nil
}

var songOggCRCTable = func() [256]uint32 {
	var table [256]uint32
	for i := range table {
		crc := uint32(i) << 24
		for range 8 {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
	return table
}()

func songOggChecksum(data []byte) uint32 {
	var crc uint32
	for _, b := range data {
		crc = crc<<8 ^ songOggCRCTable[byte(crc>>24)^b]
	}
	return crc
}

func checkSongOpusTags(packet []byte) error {
	invalid := errors.New("invalid Opus comment header")
	if len(packet) < 16 || string(packet[:8]) != "OpusTags" {
		return invalid
	}
	vendor := uint64(binary.LittleEndian.Uint32(packet[8:12]))
	if vendor > uint64(len(packet)-16) {
		return invalid
	}
	pos := 12 + int(vendor)
	count := binary.LittleEndian.Uint32(packet[pos : pos+4])
	pos += 4
	for ; count > 0; count-- {
		if len(packet)-pos < 4 {
			return invalid
		}
		size := uint64(binary.LittleEndian.Uint32(packet[pos : pos+4]))
		pos += 4
		if size > uint64(len(packet)-pos) {
			return invalid
		}
		pos += int(size)
	}
	return nil
}

func songOpus20ms(packet []byte) bool {
	if len(packet) < 2 || packet[0]&4 != 0 {
		return false
	}
	config := packet[0] >> 3
	var samples int
	switch {
	case config >= 16:
		samples = 120 << (config & 3)
	case config >= 12:
		samples = 480 << (config & 1)
	case config&3 == 3:
		samples = 2880
	default:
		samples = 480 << (config & 3)
	}
	count := 1
	switch packet[0] & 3 {
	case 1, 2:
		count = 2
	case 3:
		count = int(packet[1] & 63)
	}
	return count > 0 && samples*count == 960
}
