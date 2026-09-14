package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

func songTestPage(sequence uint32, flags byte, lacing, body []byte) []byte {
	page := make([]byte, 27+len(lacing)+len(body))
	copy(page, "OggS")
	page[5] = flags
	binary.LittleEndian.PutUint32(page[14:18], 123)
	binary.LittleEndian.PutUint32(page[18:22], sequence)
	page[26] = byte(len(lacing))
	copy(page[27:], lacing)
	copy(page[27+len(lacing):], body)
	binary.LittleEndian.PutUint32(page[22:26], songOggChecksum(page))
	return page
}

func songTestHead() []byte {
	head := make([]byte, 19)
	copy(head, "OpusHead")
	head[8], head[9] = 1, 1
	binary.LittleEndian.PutUint32(head[12:16], 48000)
	return songTestPage(0, 2, []byte{19}, head)
}

func songTestTags(vendorSize int) []byte {
	tags := make([]byte, 16+vendorSize)
	copy(tags, "OpusTags")
	binary.LittleEndian.PutUint32(tags[8:12], uint32(vendorSize))
	return tags
}

func TestSongOggContinuedTagsAndMultipleFrames(t *testing.T) {
	// A packet exactly divisible by 255 still needs its final zero lace, which
	// may be on a continued page together with the next complete packet.
	tags := songTestTags(494)
	stream := songTestHead()
	stream = append(stream, songTestPage(1, 0, []byte{255, 255}, tags)...)
	first, second := []byte{0xf8, 0xff, 0xfe}, []byte{0xf8, 0xff, 0xfd}
	body := append(bytes.Clone(first), second...)
	stream = append(stream, songTestPage(2, 5, []byte{0, 3, 3}, body)...)
	r := &songOggReader{r: bytes.NewReader(stream)}
	got, err := r.next()
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("first packet = %x, %v", got, err)
	}
	gotSecond, err := r.next()
	if err != nil || !bytes.Equal(gotSecond, second) {
		t.Fatalf("second packet = %x, %v", gotSecond, err)
	}
	for range 2 {
		if _, err := r.next(); !errors.Is(err, io.EOF) {
			t.Fatalf("clean finish = %v", err)
		}
	}
}

func TestSongOggRejectsDamagedStreams(t *testing.T) {
	prefix := songTestHead()
	prefix = append(prefix, songTestPage(1, 0, []byte{16}, songTestTags(0))...)
	frame := []byte{0xf8, 0xff, 0xfe}
	goodEnd := songTestPage(2, 4, []byte{3}, frame)
	badChecksum := bytes.Clone(goodEnd)
	badChecksum[len(badChecksum)-1] ^= 1
	cases := map[string][]byte{
		"missing end marker":      songTestPage(2, 0, []byte{3}, frame),
		"truncated page body":     goodEnd[:len(goodEnd)-1],
		"bad checksum":            badChecksum,
		"sequence gap":            songTestPage(3, 4, []byte{3}, frame),
		"unexpected continuation": songTestPage(2, 5, []byte{3}, frame),
		"unfinished packet":       songTestPage(2, 4, []byte{255}, make([]byte, 255)),
		"wrong frame duration":    songTestPage(2, 4, []byte{3}, []byte{0xf0, 0xff, 0xfe}),
		"stereo frame":            songTestPage(2, 4, []byte{3}, []byte{0xfc, 0xff, 0xfe}),
		"trailing stream":         append(bytes.Clone(goodEnd), goodEnd...),
	}
	for name, ending := range cases {
		t.Run(name, func(t *testing.T) {
			stream := append(bytes.Clone(prefix), ending...)
			r := &songOggReader{r: bytes.NewReader(stream)}
			for range 3 {
				_, err := r.next()
				if err != nil {
					if errors.Is(err, io.EOF) {
						t.Fatal("damaged stream reported clean EOF")
					}
					return
				}
			}
			t.Fatal("damaged stream was accepted")
		})
	}
}

func TestSongOggRejectsUnboundedPacket(t *testing.T) {
	stream := songTestHead()
	lacing := bytes.Repeat([]byte{255}, 255)
	stream = append(stream, songTestPage(1, 0, lacing, make([]byte, 65025))...)
	stream = append(stream, songTestPage(2, 5, []byte{255, 255, 2}, make([]byte, 512))...)
	r := &songOggReader{r: bytes.NewReader(stream)}
	if _, err := r.next(); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("oversized packet = %v", err)
	}
}

func TestSongVideoSelectionPrefersMatchingStudioRecording(t *testing.T) {
	duration := 200 * time.Second
	official := songVideo{Title: "Artist - Song (Official Audio)", Channel: "Artist", Duration: 200}
	live := songVideo{Title: "Artist - Song (Live)", Channel: "Artist", Duration: 200}
	video := songVideo{Title: "Artist - Song (Official Video)", Channel: "Artist", Duration: 250}
	for _, alternative := range []songVideo{live, video} {
		if songVideoScore(official, "Song", "Artist", duration) <= songVideoScore(alternative, "Song", "Artist", duration) {
			t.Fatalf("preferred non-studio or mismatched recording: %s", alternative.Title)
		}
	}
}

func TestSoundCloudRequiresMatchingFullRecording(t *testing.T) {
	recording := songVideo{Title: "Never Gonna Give You Up", Uploader: "Rick Astley", Duration: 213,
		WebpageURL: "https://soundcloud.com/rick-astley-official/never-gonna-give-you-up?tracking=discard"}
	want := "https://soundcloud.com/rick-astley-official/never-gonna-give-you-up"
	if got := matchingSoundCloudURL(recording, "Never Gonna Give You Up", "Rick Astley", 213*time.Second); got != want {
		t.Fatalf("matching public recording = %q", got)
	}
	cases := map[string]func(*songVideo){
		"preview":           func(v *songVideo) { v.Duration = 30 },
		"wrong title":       func(v *songVideo) { v.Title = "Together Forever" },
		"uncredited artist": func(v *songVideo) { v.Uploader = "Someone Else" },
		"remix":             func(v *songVideo) { v.Title += " (Remix)" },
		"untrusted host":    func(v *songVideo) { v.WebpageURL = "https://soundcloud.com.example.org/artist/track" },
		"playlist":          func(v *songVideo) { v.WebpageURL = "https://soundcloud.com/artist/sets/album" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := recording
			mutate(&candidate)
			if got := matchingSoundCloudURL(candidate, "Never Gonna Give You Up", "Rick Astley", 213*time.Second); got != "" {
				t.Fatalf("accepted mismatched or unsafe fallback: %q", got)
			}
		})
	}
	if got := matchingSoundCloudURL(recording, "Never Gonna Give You Up", "Rick Astley", 0); got != "" {
		t.Fatal("accepted fallback without reference duration")
	}
}
