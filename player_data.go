package main

import (
	"encoding/json"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
)

const (
	playerDataFile = "player_data.json"
	adminsFile     = "admins.json"
)

// PlayerData represents stored data for a player
type PlayerData struct {
	WordlePoints int      `json:"wordle_points"`
	Ranks        []string `json:"ranks,omitempty"`
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

// GetWordlePoints gets wordle points for a player
func GetWordlePoints(username string) int {
	if playerDataManager == nil {
		return 0
	}
	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	key := strings.ToLower(username)
	if data, exists := playerDataManager.players[key]; exists {
		return data.WordlePoints
	}
	return 0
}

// AddWordlePoint adds a wordle point for a player
func AddWordlePoint(username string) {
	if playerDataManager == nil {
		return
	}
	data := playerDataManager.getOrCreatePlayer(username)
	playerDataManager.mu.Lock()
	data.WordlePoints++
	playerDataManager.mu.Unlock()
}

// GetTopWordlePlayer returns the top wordle player and their points
func GetTopWordlePlayer() (string, int) {
	if playerDataManager == nil {
		return "", 0
	}

	playerDataManager.mu.RLock()
	defer playerDataManager.mu.RUnlock()

	var topPlayer string
	var topPoints int

	for name, data := range playerDataManager.players {
		if data.WordlePoints > topPoints {
			topPlayer = name
			topPoints = data.WordlePoints
		}
	}

	return topPlayer, topPoints
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
