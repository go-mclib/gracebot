package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-mclib/client/client"
)

const (
	defaultWordLength = 5
	maxWordLength     = 10
	minWordLength     = 3
	maxGuesses        = 6
)

// WordleGame represents an active wordle game
type WordleGame struct {
	Word       string
	Length     int
	Guesses    []string
	Revealed   []bool // Which letters have been revealed
	MaxGuesses int
	StartedBy  string
	StartTime  time.Time
}

// WordleManager manages wordle games
type WordleManager struct {
	activeGame *WordleGame
	mu         sync.Mutex
	mcClient   *client.Client
}

// Global wordle manager
var wordleManager *WordleManager

// InitWordle initializes the wordle game system
func InitWordle(c *client.Client) {
	wordleManager = &WordleManager{
		mcClient: c,
	}

	// Register wordle commands (everyone can use)
	RegisterCommand("wordle", "Start a word guessing game", "!wordle [length] (default: 5)", []string{"admin"}, wordleCommand)
	RegisterCommand("wordlepoints", "Show wordle points", "!wordlepoints [player]", nil, wordlePointsCommand)
}

// wordlePointsCommand handles the !wordlepoints command
func wordlePointsCommand(c *client.Client, sender string, args []string) {
	if len(args) > 0 {
		// Show points for specific player
		player := args[0]
		points := GetWordlePoints(player)
		c.SendChatMessage(fmt.Sprintf("%s: %d", player, points))
		return
	}

	// Show top player
	topPlayer, topPoints := GetTopWordlePlayer()
	if topPlayer == "" {
		c.SendChatMessage("No wordle scores yet!")
		return
	}
	c.SendChatMessage(fmt.Sprintf("Top: %s: %d", topPlayer, topPoints))
}

// wordleCommand handles the !wordle command
func wordleCommand(c *client.Client, sender string, args []string) {
	wordleManager.mu.Lock()
	defer wordleManager.mu.Unlock()

	if wordleManager.activeGame != nil {
		c.SendChatMessage(fmt.Sprintf("Game in progress (%d/%d guesses): %s",
			len(wordleManager.activeGame.Guesses), wordleManager.activeGame.MaxGuesses,
			wordleManager.activeGame.GetProgressDisplay()))
		return
	}

	// Parse length argument
	length := defaultWordLength
	if len(args) > 0 {
		var parsedLength int
		if _, err := fmt.Sscanf(args[0], "%d", &parsedLength); err == nil {
			if parsedLength >= minWordLength && parsedLength <= maxWordLength {
				length = parsedLength
			} else {
				c.SendChatMessage(fmt.Sprintf("Word length must be between %d and %d", minWordLength, maxWordLength))
				return
			}
		}
	}

	// Fetch a random word
	word, err := fetchRandomWord(length)
	if err != nil {
		log.Printf("[Wordle] Failed to fetch word: %v", err)
		c.SendChatMessage("Failed to start game. Please try again.")
		return
	}

	// Create new game
	wordleManager.activeGame = &WordleGame{
		Word:       strings.ToUpper(word),
		Length:     length,
		Guesses:    make([]string, 0, maxGuesses),
		Revealed:   make([]bool, length),
		MaxGuesses: maxGuesses,
		StartedBy:  sender,
		StartTime:  time.Now(),
	}

	c.SendChatMessage(fmt.Sprintf("Wordle started by %s! Guess the %d-letter word (%d attempts): %s",
		sender, length, maxGuesses, wordleManager.activeGame.GetProgressDisplay()))
}

// StopWordle stops the current wordle game, returns true if a game was stopped
func StopWordle(c *client.Client) bool {
	if wordleManager == nil {
		return false
	}

	wordleManager.mu.Lock()
	defer wordleManager.mu.Unlock()

	if wordleManager.activeGame == nil {
		return false
	}

	word := wordleManager.activeGame.Word
	wordleManager.activeGame = nil
	c.SendChatMessage(fmt.Sprintf("Wordle stopped. The word was: %s", word))
	return true
}

// ProcessGuess checks if a message is a valid guess and processes it
// Returns true if the message was processed as a guess
func ProcessGuess(c *client.Client, sender, message string) bool {
	if wordleManager == nil {
		return false
	}

	wordleManager.mu.Lock()
	defer wordleManager.mu.Unlock()

	if wordleManager.activeGame == nil {
		return false
	}

	game := wordleManager.activeGame

	// Check if message is the right length and all letters
	message = strings.TrimSpace(message)
	if len(message) != game.Length {
		return false
	}

	// Check if all characters are letters
	for _, r := range message {
		if !unicode.IsLetter(r) {
			return false
		}
	}

	guess := strings.ToUpper(message)

	// Check if already guessed
	for _, g := range game.Guesses {
		if g == guess {
			c.SendChatMessage(fmt.Sprintf("%s: Already guessed!", guess))
			return true
		}
	}

	// Add guess
	game.Guesses = append(game.Guesses, guess)

	// Check for exact match
	if guess == game.Word {
		AddWordlePoint(sender)
		points := GetWordlePoints(sender)
		c.SendChatMessage(fmt.Sprintf("Correct! %s guessed %s in %d guesses! (Total: %d)", sender, game.Word, len(game.Guesses), points))
		wordleManager.activeGame = nil
		return true
	}

	// Update revealed letters and build result
	result := game.ProcessGuessAndReveal(guess)

	// Check for game over
	if len(game.Guesses) >= game.MaxGuesses {
		c.SendChatMessage(fmt.Sprintf("%s: %s | Game over! The word was: %s", sender, result, game.Word))
		wordleManager.activeGame = nil
		return true
	}

	// Show progress
	c.SendChatMessage(fmt.Sprintf("%s: %s (%d/%d) %s", sender, result, len(game.Guesses), game.MaxGuesses, game.GetProgressDisplay()))

	return true
}

// ProcessGuessAndReveal processes a guess and updates revealed letters
// Returns a simple result string like "H.LL*" where:
// uppercase = correct position, lowercase = wrong position, . = not in word
func (g *WordleGame) ProcessGuessAndReveal(guess string) string {
	var result strings.Builder
	wordRunes := []rune(g.Word)
	guessRunes := []rune(guess)
	wordLetterCount := make(map[rune]int)

	// Count letters in word
	for _, r := range wordRunes {
		wordLetterCount[r]++
	}

	// First pass: mark exact matches
	exactMatch := make([]bool, g.Length)
	for i := 0; i < g.Length; i++ {
		if guessRunes[i] == wordRunes[i] {
			exactMatch[i] = true
			wordLetterCount[guessRunes[i]]--
			g.Revealed[i] = true
		}
	}

	// Second pass: build result
	for i := 0; i < g.Length; i++ {
		letter := guessRunes[i]
		if exactMatch[i] {
			// Exact match: uppercase
			result.WriteRune(unicode.ToUpper(letter))
		} else if wordLetterCount[letter] > 0 {
			// Letter exists elsewhere: lowercase
			wordLetterCount[letter]--
			result.WriteRune(unicode.ToLower(letter))
		} else {
			// Not in word: dot
			result.WriteRune('.')
		}
	}

	return result.String()
}

// GetProgressDisplay returns the current progress display
func (g *WordleGame) GetProgressDisplay() string {
	var result strings.Builder
	result.WriteString("[")

	wordRunes := []rune(g.Word)
	for i := 0; i < g.Length; i++ {
		if g.Revealed[i] {
			result.WriteString(string(wordRunes[i]))
		} else {
			result.WriteString("_")
		}
	}

	result.WriteString("]")
	return result.String()
}

// fetchRandomWord fetches a random word of the specified length
func fetchRandomWord(length int) (string, error) {
	// Try the random word API first
	url := fmt.Sprintf("https://random-word-api.herokuapp.com/word?length=%d", length)

	httpClient := &http.Client{Timeout: 5 * time.Second}
	resp, err := httpClient.Get(url)
	if err == nil && resp.StatusCode == http.StatusOK {
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			var words []string
			if err := json.Unmarshal(body, &words); err == nil && len(words) > 0 {
				return strings.ToLower(words[0]), nil
			}
		}
	}

	// Fallback to built-in word list
	log.Printf("[Wordle] API failed, using fallback word list")
	return getRandomFallbackWord(length)
}

// Fallback word lists by length
var fallbackWords = map[int][]string{
	3:  {"cat", "dog", "sun", "run", "fun", "hat", "bat", "car", "jar", "bar"},
	4:  {"word", "game", "play", "time", "love", "home", "book", "tree", "star", "moon"},
	5:  {"world", "games", "words", "music", "happy", "party", "money", "water", "light", "dream"},
	6:  {"friend", "family", "player", "garden", "castle", "dragon", "knight", "wizard", "forest", "planet"},
	7:  {"another", "morning", "evening", "weather", "journey", "mystery", "fantasy", "kingdom", "diamond", "rainbow"},
	8:  {"computer", "building", "mountain", "treasure", "champion", "surprise", "strength", "football", "birthday", "shopping"},
	9:  {"adventure", "beautiful", "challenge", "dangerous", "excellent", "fantastic", "happiness", "important", "knowledge", "wonderful"},
	10: {"basketball", "absolutely", "background", "celebrated", "determined", "everything", "friendship", "generation", "helicopter", "impossible"},
}

func getRandomFallbackWord(length int) (string, error) {
	words, ok := fallbackWords[length]
	if !ok || len(words) == 0 {
		// Generate a simple word if no fallback exists
		return generateSimpleWord(length), nil
	}
	return words[rand.Intn(len(words))], nil
}

// generateSimpleWord generates a simple pronounceable word
func generateSimpleWord(length int) string {
	vowels := []rune{'a', 'e', 'i', 'o', 'u'}
	consonants := []rune{'b', 'c', 'd', 'f', 'g', 'h', 'j', 'k', 'l', 'm', 'n', 'p', 'r', 's', 't', 'v', 'w'}

	result := make([]rune, length)
	for i := 0; i < length; i++ {
		if i%2 == 0 {
			result[i] = consonants[rand.Intn(len(consonants))]
		} else {
			result[i] = vowels[rand.Intn(len(vowels))]
		}
	}
	return string(result)
}
