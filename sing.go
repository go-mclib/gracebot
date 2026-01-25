package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-mclib/client/client"
)

// LyricLine represents a single line of lyrics with timestamp
type LyricLine struct {
	Time time.Duration
	Text string
}

// SingSession represents an active singing session
type SingSession struct {
	Track    string
	Artist   string
	Lyrics   []LyricLine
	StartAt  time.Time
	StopChan chan struct{}
}

// SingManager manages singing sessions
type SingManager struct {
	activeSession *SingSession
	mu            sync.Mutex
	mcClient      *client.Client
}

// Global sing manager
var singManager *SingManager

// InitSing initializes the singing system
func InitSing(c *client.Client) {
	singManager = &SingManager{
		mcClient: c,
	}

	RegisterCommand("sing", "Sing lyrics of a song", "!sing <song name>", []string{"admin"}, singCommand)
}

// lrcLibResponse represents the response from lrclib.net API
type lrcLibResponse struct {
	ID           int     `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	SyncedLyrics string  `json:"syncedLyrics"`
	PlainLyrics  string  `json:"plainLyrics"`
}

// singCommand handles the !sing command
func singCommand(c *client.Client, sender string, args []string) {
	if len(args) == 0 {
		c.SendChatMessage("Usage: !sing <song name>")
		return
	}

	query := strings.Join(args, " ")

	singManager.mu.Lock()
	if singManager.activeSession != nil {
		singManager.mu.Unlock()
		c.SendChatMessage("Already singing! Use !stop to stop.")
		return
	}
	singManager.mu.Unlock()

	// Fetch lyrics
	c.SendChatMessage(fmt.Sprintf("Searching for: %s...", query))

	lyrics, track, artist, err := fetchLyrics(query)
	if err != nil {
		log.Printf("[Sing] Failed to fetch lyrics: %v", err)
		c.SendChatMessage("Could not find lyrics for that song.")
		return
	}

	if len(lyrics) == 0 {
		c.SendChatMessage("No synced lyrics available for this song.")
		return
	}

	singManager.mu.Lock()
	singManager.activeSession = &SingSession{
		Track:    track,
		Artist:   artist,
		Lyrics:   lyrics,
		StartAt:  time.Now(),
		StopChan: make(chan struct{}),
	}
	session := singManager.activeSession
	singManager.mu.Unlock()

	c.SendChatMessage(fmt.Sprintf("Now singing: %s - %s", artist, track))

	// Start singing in background
	go singManager.playSong(c, session)
}

// playSong plays the lyrics synchronized
func (m *SingManager) playSong(c *client.Client, session *SingSession) {
	startTime := session.StartAt

	for _, line := range session.Lyrics {
		// Calculate when this line should be sent
		targetTime := startTime.Add(line.Time)
		waitDuration := time.Until(targetTime)

		if waitDuration > 0 {
			select {
			case <-session.StopChan:
				c.SendChatMessage("Stopped singing.")
				m.mu.Lock()
				m.activeSession = nil
				m.mu.Unlock()
				return
			case <-time.After(waitDuration):
			}
		}

		// Check if stopped
		select {
		case <-session.StopChan:
			c.SendChatMessage("Stopped singing.")
			m.mu.Lock()
			m.activeSession = nil
			m.mu.Unlock()
			return
		default:
		}

		// Send the lyric line
		if line.Text != "" {
			c.SendChatMessage(line.Text)
		}
	}

	// Song finished
	m.mu.Lock()
	m.activeSession = nil
	m.mu.Unlock()
}

// StopSinging stops the current singing session
func StopSinging() bool {
	if singManager == nil {
		return false
	}

	singManager.mu.Lock()
	defer singManager.mu.Unlock()

	if singManager.activeSession == nil {
		return false
	}

	close(singManager.activeSession.StopChan)
	singManager.activeSession = nil
	return true
}

// IsSinging returns true if currently singing
func IsSinging() bool {
	if singManager == nil {
		return false
	}

	singManager.mu.Lock()
	defer singManager.mu.Unlock()
	return singManager.activeSession != nil
}

// fetchLyrics fetches synced lyrics from lrclib.net
func fetchLyrics(query string) ([]LyricLine, string, string, error) {
	// Search for the song
	searchURL := fmt.Sprintf("https://lrclib.net/api/search?q=%s", url.QueryEscape(query))

	httpClient := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		return nil, "", "", err
	}
	req.Header.Set("User-Agent", "GraceBot/1.0")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, "", "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", "", fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", "", err
	}

	var results []lrcLibResponse
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, "", "", err
	}

	// Find first result with synced lyrics
	for _, result := range results {
		if result.SyncedLyrics != "" {
			lyrics := parseLRC(result.SyncedLyrics)
			return lyrics, result.TrackName, result.ArtistName, nil
		}
	}

	// Fall back to plain lyrics if no synced available
	if len(results) > 0 && results[0].PlainLyrics != "" {
		lyrics := parsePlainLyrics(results[0].PlainLyrics)
		return lyrics, results[0].TrackName, results[0].ArtistName, nil
	}

	return nil, "", "", fmt.Errorf("no lyrics found")
}

// parseLRC parses LRC format lyrics with timestamps
// Format: [mm:ss.xx] lyrics text
func parseLRC(lrc string) []LyricLine {
	var lyrics []LyricLine
	// Match [mm:ss.xx] or [mm:ss]
	re := regexp.MustCompile(`\[(\d+):(\d+)(?:\.(\d+))?\](.*)`)

	for _, line := range strings.Split(lrc, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		matches := re.FindStringSubmatch(line)
		if matches == nil {
			continue
		}

		minutes, _ := strconv.Atoi(matches[1])
		seconds, _ := strconv.Atoi(matches[2])
		milliseconds := 0
		if matches[3] != "" {
			ms, _ := strconv.Atoi(matches[3])
			// Handle different precision (2 or 3 digits)
			if len(matches[3]) == 2 {
				milliseconds = ms * 10
			} else {
				milliseconds = ms
			}
		}

		text := strings.TrimSpace(matches[4])

		timestamp := time.Duration(minutes)*time.Minute +
			time.Duration(seconds)*time.Second +
			time.Duration(milliseconds)*time.Millisecond

		lyrics = append(lyrics, LyricLine{
			Time: timestamp,
			Text: text,
		})
	}

	// Sort by timestamp
	sort.Slice(lyrics, func(i, j int) bool {
		return lyrics[i].Time < lyrics[j].Time
	})

	return lyrics
}

// parsePlainLyrics parses plain lyrics without timestamps
// Sends one line per second
func parsePlainLyrics(plain string) []LyricLine {
	var lyrics []LyricLine
	timestamp := time.Duration(0)

	for _, line := range strings.Split(plain, "\n") {
		line = strings.TrimSpace(line)
		lyrics = append(lyrics, LyricLine{
			Time: timestamp,
			Text: line,
		})
		// Add 2 seconds between lines for plain lyrics
		timestamp += 2 * time.Second
	}

	return lyrics
}
