package main

import (
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	playerDataFile = "player_data.json"
	adminsFile     = "admins.json"
)

// PlayerData represents stored data for a player
type PlayerData struct {
	WordlePoints         int            `json:"wordle_points,omitempty"`          // Deprecated: use GamePoints["wordle"]
	GamePoints           map[string]int `json:"game_points,omitempty"`            // Game -> points
	Ranks                []string       `json:"ranks,omitempty"`
	LastSeen             int64          `json:"last_seen,omitempty"`              // Unix timestamp
	TotalPlaytimeSeconds int64          `json:"total_playtime_seconds,omitempty"` // Total playtime in seconds
	WordCounts           map[string]int `json:"word_counts,omitempty"`            // Word -> count
	DeathCounts          map[string]int `json:"death_counts,omitempty"`           // Death type -> count

	// Non-persisted session tracking
	sessionStart int64 `json:"-"` // Unix timestamp of current session start
}

// PlayerDataManager manages player data
type PlayerDataManager struct {
	players map[string]*PlayerData // lowercase username -> data
	admins  map[string]bool        // lowercase admin usernames
	mu      sync.RWMutex
}

// Global player data manager
var playerDataManager *PlayerDataManager

// InitPlayerData initializes the player data system
func InitPlayerData() {
	playerDataManager = &PlayerDataManager{
		players: make(map[string]*PlayerData),
		admins:  make(map[string]bool),
	}

	playerDataManager.loadAdmins()
	playerDataManager.loadPlayerData()

	// Migrate old wordle_points.json if it exists
	playerDataManager.migrateOldPoints()
}

// ShutdownPlayerData saves player data
func ShutdownPlayerData() {
	if playerDataManager != nil {
		playerDataManager.savePlayerData()
	}
}

// loadAdmins loads admin usernames from admins.json
func (m *PlayerDataManager) loadAdmins() {
	data, err := os.ReadFile(adminsFile)
	if err != nil {
		if os.IsNotExist(err) {
			// Create default admins.json
			defaultAdmins := []string{}
			data, _ := json.MarshalIndent(defaultAdmins, "", "  ")
			os.WriteFile(adminsFile, data, 0644)
			log.Printf("[PlayerData] Created empty %s", adminsFile)
		} else {
			log.Printf("[PlayerData] Failed to load admins: %v", err)
		}
		return
	}

	var adminList []string
	if err := json.Unmarshal(data, &adminList); err != nil {
		log.Printf("[PlayerData] Failed to parse admins file: %v", err)
		return
	}

	for _, admin := range adminList {
		m.admins[strings.ToLower(admin)] = true
	}
	log.Printf("[PlayerData] Loaded %d admins", len(m.admins))
}

// loadPlayerData loads player data from file
func (m *PlayerDataManager) loadPlayerData() {
	data, err := os.ReadFile(playerDataFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("[PlayerData] Failed to load player data: %v", err)
		}
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := json.Unmarshal(data, &m.players); err != nil {
		log.Printf("[PlayerData] Failed to parse player data: %v", err)
		return
	}
	log.Printf("[PlayerData] Loaded data for %d players", len(m.players))
}

// savePlayerData saves player data to file
func (m *PlayerDataManager) savePlayerData() {
	m.mu.RLock()
	defer m.mu.RUnlock()

	data, err := json.MarshalIndent(m.players, "", "  ")
	if err != nil {
		log.Printf("[PlayerData] Failed to marshal player data: %v", err)
		return
	}

	if err := os.WriteFile(playerDataFile, data, 0644); err != nil {
		log.Printf("[PlayerData] Failed to save player data: %v", err)
		return
	}
	log.Printf("[PlayerData] Saved data for %d players", len(m.players))
}

// migrateOldPoints migrates from old wordle_points.json format
func (m *PlayerDataManager) migrateOldPoints() {
	oldFile := "wordle_points.json"
	data, err := os.ReadFile(oldFile)
	if err != nil {
		return // No old file to migrate
	}

	var oldPoints map[string]int
	if err := json.Unmarshal(data, &oldPoints); err != nil {
		log.Printf("[PlayerData] Failed to parse old points file: %v", err)
		return
	}

	m.mu.Lock()
	for player, points := range oldPoints {
		key := strings.ToLower(player)
		if _, exists := m.players[key]; !exists {
			m.players[key] = &PlayerData{}
		}
		m.players[key].WordlePoints = points
	}
	m.mu.Unlock()

	// Save migrated data
	m.savePlayerData()

	// Remove old file
	os.Remove(oldFile)
	log.Printf("[PlayerData] Migrated %d players from %s", len(oldPoints), oldFile)
}

// getOrCreatePlayer gets or creates player data
func (m *PlayerDataManager) getOrCreatePlayer(username string) *PlayerData {
	key := strings.ToLower(username)

	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.players[key]; !exists {
		m.players[key] = &PlayerData{}
	}
	return m.players[key]
}

// GetGamePoints gets points for a specific game for a player
func GetGamePoints(username, game string) int {
	if playerDataManager == nil {
		return 0
	}
	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	gameLower := strings.ToLower(game)
	if data, exists := playerDataManager.players[key]; exists {
		if data.GamePoints != nil {
			return data.GamePoints[gameLower]
		}
		// Migration: check old WordlePoints field
		if gameLower == "wordle" && data.WordlePoints > 0 {
			return data.WordlePoints
		}
	}
	return 0
}

// AddGamePoint adds a point for a specific game for a player
func AddGamePoint(username, game string) {
	if playerDataManager == nil {
		return
	}
	data := playerDataManager.getOrCreatePlayer(username)
	playerDataManager.mu.Lock()
	defer playerDataManager.mu.Unlock()

	if data.GamePoints == nil {
		data.GamePoints = make(map[string]int)
	}
	gameLower := strings.ToLower(game)

	// Migration: move old WordlePoints to GamePoints
	if gameLower == "wordle" && data.WordlePoints > 0 && data.GamePoints["wordle"] == 0 {
		data.GamePoints["wordle"] = data.WordlePoints
		data.WordlePoints = 0
	}

	data.GamePoints[gameLower]++
}

// GetTopGamePlayer returns the top player for a specific game and their points
func GetTopGamePlayer(game string) (string, int) {
	if playerDataManager == nil {
		return "", 0
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	var topPlayer string
	var topPoints int
	gameLower := strings.ToLower(game)

	for name, data := range playerDataManager.players {
		points := 0
		if data.GamePoints != nil {
			points = data.GamePoints[gameLower]
		}
		// Migration: check old WordlePoints field
		if gameLower == "wordle" && data.WordlePoints > points {
			points = data.WordlePoints
		}
		if points > topPoints {
			topPlayer = name
			topPoints = points
		}
	}

	return topPlayer, topPoints
}

// Deprecated: use GetGamePoints(username, "wordle") instead
func GetWordlePoints(username string) int {
	return GetGamePoints(username, "wordle")
}

// Deprecated: use AddGamePoint(username, "wordle") instead
func AddWordlePoint(username string) {
	AddGamePoint(username, "wordle")
}

// Deprecated: use GetTopGamePlayer("wordle") instead
func GetTopWordlePlayer() (string, int) {
	return GetTopGamePlayer("wordle")
}

// AddDeath records a death for a player with the death type
func AddDeath(username, deathType string) {
	if playerDataManager == nil {
		return
	}
	data := playerDataManager.getOrCreatePlayer(username)
	playerDataManager.mu.Lock()
	defer playerDataManager.mu.Unlock()

	if data.DeathCounts == nil {
		data.DeathCounts = make(map[string]int)
	}
	data.DeathCounts[deathType]++
}

// GetDeathCounts returns all death type counts for a player
func GetDeathCounts(username string) map[string]int {
	if playerDataManager == nil {
		return nil
	}
	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists {
		return data.DeathCounts
	}
	return nil
}

// GetDeathCount returns the total death count for a player
func GetDeathCount(username string) int {
	if playerDataManager == nil {
		return 0
	}
	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists && data.DeathCounts != nil {
		total := 0
		for _, count := range data.DeathCounts {
			total += count
		}
		return total
	}
	return 0
}

// GetDeathTypeCount returns the count of a specific death type for a player
func GetDeathTypeCount(username, deathType string) int {
	if playerDataManager == nil {
		return 0
	}
	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists && data.DeathCounts != nil {
		return data.DeathCounts[deathType]
	}
	return 0
}

// GetTopDeathCauses returns the top N death causes for a player, sorted by count
func GetTopDeathCauses(username string, n int) []struct {
	Type  string
	Count int
} {
	if playerDataManager == nil {
		return nil
	}
	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	data, exists := playerDataManager.players[key]
	if !exists || data.DeathCounts == nil {
		return nil
	}

	type deathCause struct {
		Type  string
		Count int
	}

	var causes []deathCause
	for dtype, count := range data.DeathCounts {
		causes = append(causes, deathCause{Type: dtype, Count: count})
	}

	// Sort by count descending
	for i := range len(causes) {
		for j := i + 1; j < len(causes); j++ {
			if causes[j].Count > causes[i].Count {
				causes[i], causes[j] = causes[j], causes[i]
			}
		}
	}

	if n > len(causes) {
		n = len(causes)
	}

	result := make([]struct {
		Type  string
		Count int
	}, n)
	for i := range n {
		result[i].Type = causes[i].Type
		result[i].Count = causes[i].Count
	}
	return result
}

// GetPlayerWithMostDeaths returns the player with the most total deaths
func GetPlayerWithMostDeaths() (string, int) {
	if playerDataManager == nil {
		return "", 0
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	var topPlayer string
	var topCount int

	for name, data := range playerDataManager.players {
		if data.DeathCounts == nil {
			continue
		}
		total := 0
		for _, count := range data.DeathCounts {
			total += count
		}
		if total > topCount {
			topPlayer = name
			topCount = total
		}
	}

	return topPlayer, topCount
}

// IsAdmin checks if a player is an admin
func IsAdmin(username string) bool {
	if playerDataManager == nil {
		return false
	}
	return playerDataManager.admins[strings.ToLower(username)]
}

// HasRank checks if a player has a specific rank
func HasRank(username, rank string) bool {
	if playerDataManager == nil {
		return rank == "default"
	}

	// Everyone has "default" rank
	if rank == "default" {
		return true
	}

	// Check admin rank
	if rank == "admin" && IsAdmin(username) {
		return true
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	data, exists := playerDataManager.players[key]
	if !exists {
		return false
	}

	rankLower := strings.ToLower(rank)
	for _, r := range data.Ranks {
		if strings.ToLower(r) == rankLower {
			return true
		}
	}
	return false
}

// GetRanks gets all ranks for a player
func GetRanks(username string) []string {
	ranks := []string{"default"}

	if IsAdmin(username) {
		ranks = append(ranks, "admin")
	}

	if playerDataManager == nil {
		return ranks
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists {
		ranks = append(ranks, data.Ranks...)
	}

	return ranks
}

// validRankRegex matches alphanumeric ranks
var validRankRegex = regexp.MustCompile(`^[a-zA-Z0-9]+$`)

// AddRank adds a rank to a player
func AddRank(username, rank string) error {
	if !validRankRegex.MatchString(rank) {
		return &RankError{"rank must be alphanumeric with no spaces"}
	}

	if rank == "default" || rank == "admin" {
		return &RankError{"cannot add reserved rank"}
	}

	if playerDataManager == nil {
		return &RankError{"player data not initialized"}
	}

	data := playerDataManager.getOrCreatePlayer(username)

	playerDataManager.mu.Lock()
	defer playerDataManager.mu.Unlock()

	// Check if already has rank
	rankLower := strings.ToLower(rank)
	for _, r := range data.Ranks {
		if strings.ToLower(r) == rankLower {
			return &RankError{"player already has this rank"}
		}
	}

	data.Ranks = append(data.Ranks, rank)
	return nil
}

// RemoveRank removes a rank from a player
func RemoveRank(username, rank string) error {
	if rank == "default" || rank == "admin" {
		return &RankError{"cannot remove reserved rank"}
	}

	if playerDataManager == nil {
		return &RankError{"player data not initialized"}
	}

	playerDataManager.mu.Lock()
	defer playerDataManager.mu.Unlock()

	key := strings.ToLower(username)
	data, exists := playerDataManager.players[key]
	if !exists {
		return &RankError{"player not found"}
	}

	rankLower := strings.ToLower(rank)
	newRanks := make([]string, 0, len(data.Ranks))
	found := false
	for _, r := range data.Ranks {
		if strings.ToLower(r) == rankLower {
			found = true
		} else {
			newRanks = append(newRanks, r)
		}
	}

	if !found {
		return &RankError{"player does not have this rank"}
	}

	data.Ranks = newRanks
	return nil
}

// RankError represents a rank-related error
type RankError struct {
	Message string
}

func (e *RankError) Error() string {
	return e.Message
}

// UpdatePlayerActivity updates the player's last seen time and manages session tracking
func UpdatePlayerActivity(username string) {
	if playerDataManager == nil {
		return
	}

	now := time.Now().Unix()
	data := playerDataManager.getOrCreatePlayer(username)

	playerDataManager.mu.Lock()
	defer playerDataManager.mu.Unlock()

	// If no active session or session is stale (>5 minutes since last activity), start new session
	if data.sessionStart == 0 || (data.LastSeen > 0 && now-data.LastSeen > 300) {
		data.sessionStart = now
	} else if data.LastSeen > 0 {
		// Add time since last message to playtime
		data.TotalPlaytimeSeconds += now - data.LastSeen
	}

	data.LastSeen = now
}

// GetLastSeen returns when a player was last seen
func GetLastSeen(username string) time.Time {
	if playerDataManager == nil {
		return time.Time{}
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists && data.LastSeen > 0 {
		return time.Unix(data.LastSeen, 0)
	}
	return time.Time{}
}

// GetPlaytime returns the total tracked playtime for a player
func GetPlaytime(username string) time.Duration {
	if playerDataManager == nil {
		return 0
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists {
		return time.Duration(data.TotalPlaytimeSeconds) * time.Second
	}
	return 0
}

// CountWords counts words in a message for a player (ignores messages with !)
func CountWords(username, message string) {
	if playerDataManager == nil {
		return
	}

	// Ignore messages containing !
	if strings.Contains(message, "!") {
		return
	}

	data := playerDataManager.getOrCreatePlayer(username)

	playerDataManager.mu.Lock()
	defer playerDataManager.mu.Unlock()

	if data.WordCounts == nil {
		data.WordCounts = make(map[string]int)
	}

	words := strings.Fields(strings.ToLower(message))
	for _, word := range words {
		// Clean up word - keep only alphanumeric
		cleaned := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, word)
		if cleaned != "" {
			data.WordCounts[cleaned]++
		}
	}
}

// GetWordCount returns the count of a specific word for a player, or globally if username is empty
func GetWordCount(word, username string) int {
	if playerDataManager == nil {
		return 0
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	wordLower := strings.ToLower(word)

	if username != "" {
		// Count for specific player
		key := strings.ToLower(username)
		if data, exists := playerDataManager.players[key]; exists && data.WordCounts != nil {
			return data.WordCounts[wordLower]
		}
		return 0
	}

	// Global count across all players
	total := 0
	for _, data := range playerDataManager.players {
		if data.WordCounts != nil {
			total += data.WordCounts[wordLower]
		}
	}
	return total
}

// PlayerExists checks if a player has any data stored
func PlayerExists(username string) bool {
	if playerDataManager == nil {
		return false
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	_, exists := playerDataManager.players[key]
	return exists
}
