package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/go-mclib/client/client"
)

// GameType represents the type of word guessing game
type GameType int

const (
	GameTypeWordle GameType = iota
	GameTypeHangman
)

const (
	defaultWordLength = 5
	maxWordLength     = 15
	minWordLength     = 3
)

// WordGuessingGame represents an active word guessing game
type WordGuessingGame struct {
	Type         GameType
	Word         string
	Length       int
	Guesses      []string // For wordle: full word guesses; for hangman: single letters
	Revealed     []bool   // Which letters have been revealed
	WrongGuesses []string // For hangman: letters guessed that aren't in the word
	MaxGuesses   int      // 0 = infinite
	StartedBy    string
	StartTime    time.Time
	CustomWord   bool // True if word was provided by user
}

// WordGuessingManager manages word guessing games
type WordGuessingManager struct {
	activeGame *WordGuessingGame
	mu         sync.Mutex
	mcClient   *client.Client
}

// Global word guessing manager
var wordGuessingManager *WordGuessingManager

// InitWordGuessing initializes the word guessing game system
func InitWordGuessing(c *client.Client) {
	wordGuessingManager = &WordGuessingManager{
		mcClient: c,
	}

	// Register wordle commands
	RegisterCommand("wordle", "Start a word guessing game", "!wordle [word|length] [attempts]", []string{"admin"}, wordleCommand)
	RegisterCommand("wordlepoints", "Show wordle points", "!wordlepoints [player]", nil, wordlePointsCommand)

	// Register hangman commands
	RegisterCommand("hangman", "Start a hangman game", "!hangman [word|length] [attempts]", []string{"admin"}, hangmanCommand)
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

// isNumeric checks if a string is a number
func isNumeric(s string) bool {
	_, err := strconv.Atoi(s)
	return err == nil
}

// isValidWord checks if a string contains only letters
func isValidWord(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return len(s) > 0
}

// startGame is the shared logic for starting a word guessing game
func startGame(c *client.Client, sender string, args []string, gameType GameType) {
	wordGuessingManager.mu.Lock()
	defer wordGuessingManager.mu.Unlock()

	gameName := "Wordle"
	if gameType == GameTypeHangman {
		gameName = "Hangman"
	}

	if wordGuessingManager.activeGame != nil {
		activeGameName := "Wordle"
		if wordGuessingManager.activeGame.Type == GameTypeHangman {
			activeGameName = "Hangman"
		}
		guessInfo := fmt.Sprintf("%d", len(wordGuessingManager.activeGame.Guesses))
		if wordGuessingManager.activeGame.MaxGuesses > 0 {
			guessInfo = fmt.Sprintf("%d/%d", len(wordGuessingManager.activeGame.Guesses), wordGuessingManager.activeGame.MaxGuesses)
		}
		c.SendChatMessage(fmt.Sprintf("%s in progress (%s guesses): %s",
			activeGameName, guessInfo, wordGuessingManager.activeGame.GetProgressDisplay()))
		return
	}

	var word string
	var maxAttempts int = 0 // 0 = infinite
	var customWord bool = false

	if len(args) == 0 {
		// No args: random 5-letter word, infinite attempts
		var err error
		word, err = fetchRandomWord(defaultWordLength)
		if err != nil {
			log.Printf("[%s] Failed to fetch word: %v", gameName, err)
			c.SendChatMessage("Failed to start game. Please try again.")
			return
		}
	} else if isNumeric(args[0]) {
		// First arg is a number: it's the length
		length, _ := strconv.Atoi(args[0])
		if length < minWordLength || length > maxWordLength {
			c.SendChatMessage(fmt.Sprintf("Word length must be between %d and %d", minWordLength, maxWordLength))
			return
		}
		var err error
		word, err = fetchRandomWord(length)
		if err != nil {
			log.Printf("[%s] Failed to fetch word: %v", gameName, err)
			c.SendChatMessage("Failed to start game. Please try again.")
			return
		}
		// Check for second arg (attempts)
		if len(args) > 1 && isNumeric(args[1]) {
			maxAttempts, _ = strconv.Atoi(args[1])
		}
	} else if isValidWord(args[0]) {
		// First arg is a word: use it as the custom word
		word = args[0]
		customWord = true
		// Check for second arg (attempts)
		if len(args) > 1 && isNumeric(args[1]) {
			maxAttempts, _ = strconv.Atoi(args[1])
		}
	} else {
		c.SendChatMessage(fmt.Sprintf("Usage: !%s [word|length] [attempts]", strings.ToLower(gameName)))
		return
	}

	// Use rune length to properly handle Unicode characters
	length := len([]rune(word))
	if length < minWordLength || length > maxWordLength {
		c.SendChatMessage(fmt.Sprintf("Word must be between %d and %d letters", minWordLength, maxWordLength))
		return
	}

	// Create new game
	wordGuessingManager.activeGame = &WordGuessingGame{
		Type:         gameType,
		Word:         strings.ToUpper(word),
		Length:       length,
		Guesses:      make([]string, 0),
		Revealed:     make([]bool, length),
		WrongGuesses: make([]string, 0),
		MaxGuesses:   maxAttempts,
		StartedBy:    sender,
		StartTime:    time.Now(),
		CustomWord:   customWord,
	}

	attemptsStr := "infinite"
	if maxAttempts > 0 {
		attemptsStr = fmt.Sprintf("%d", maxAttempts)
	}

	c.SendChatMessage(fmt.Sprintf("%s started by %s! Guess the %d-letter word (%s attempts): %s",
		gameName, sender, length, attemptsStr, wordGuessingManager.activeGame.GetProgressDisplay()))
}

// wordleCommand handles the !wordle command
func wordleCommand(c *client.Client, sender string, args []string) {
	startGame(c, sender, args, GameTypeWordle)
}

// hangmanCommand handles the !hangman command
func hangmanCommand(c *client.Client, sender string, args []string) {
	startGame(c, sender, args, GameTypeHangman)
}

// StopWordle stops the current word guessing game, returns true if a game was stopped
func StopWordle(c *client.Client) bool {
	if wordGuessingManager == nil {
		return false
	}

	wordGuessingManager.mu.Lock()
	defer wordGuessingManager.mu.Unlock()

	if wordGuessingManager.activeGame == nil {
		return false
	}

	gameName := "Wordle"
	if wordGuessingManager.activeGame.Type == GameTypeHangman {
		gameName = "Hangman"
	}

	word := wordGuessingManager.activeGame.Word
	wordGuessingManager.activeGame = nil
	c.SendChatMessage(fmt.Sprintf("%s stopped. The word was: %s", gameName, word))
	return true
}

// ProcessGuess checks if a message is a valid guess and processes it
// Returns true if the message was processed as a guess
func ProcessGuess(c *client.Client, sender, message string) bool {
	if wordGuessingManager == nil {
		return false
	}

	wordGuessingManager.mu.Lock()
	defer wordGuessingManager.mu.Unlock()

	if wordGuessingManager.activeGame == nil {
		return false
	}

	game := wordGuessingManager.activeGame

	if game.Type == GameTypeHangman {
		return processHangmanGuess(c, sender, message, game)
	}
	return processWordleGuess(c, sender, message, game)
}

// processWordleGuess handles wordle-style guesses (full words)
func processWordleGuess(c *client.Client, sender, message string, game *WordGuessingGame) bool {
	// Check if message is the right length and all letters
	message = strings.TrimSpace(message)
	messageRunes := []rune(message)

	if len(messageRunes) != game.Length {
		return false
	}

	// Check if all characters are letters
	for _, r := range messageRunes {
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
		// Only award points for non-custom words
		if !game.CustomWord {
			AddWordlePoint(sender)
			points := GetWordlePoints(sender)
			c.SendChatMessage(fmt.Sprintf("Correct! %s guessed %s in %d guesses! (Total: %d)", sender, game.Word, len(game.Guesses), points))
		} else {
			c.SendChatMessage(fmt.Sprintf("Correct! %s guessed %s in %d guesses!", sender, game.Word, len(game.Guesses)))
		}
		wordGuessingManager.activeGame = nil
		return true
	}

	// Update revealed letters and build result
	result := game.ProcessWordleGuessAndReveal(guess)

	// Check for game over (only if MaxGuesses > 0)
	if game.MaxGuesses > 0 && len(game.Guesses) >= game.MaxGuesses {
		c.SendChatMessage(fmt.Sprintf("%s: %s | Game over! The word was: %s", sender, result, game.Word))
		wordGuessingManager.activeGame = nil
		return true
	}

	// Show progress
	guessInfo := fmt.Sprintf("%d", len(game.Guesses))
	if game.MaxGuesses > 0 {
		guessInfo = fmt.Sprintf("%d/%d", len(game.Guesses), game.MaxGuesses)
	}
	c.SendChatMessage(fmt.Sprintf("%s: %s (%s) %s", sender, result, guessInfo, game.GetProgressDisplay()))

	return true
}

// processHangmanGuess handles hangman-style guesses (single letters)
func processHangmanGuess(c *client.Client, sender, message string, game *WordGuessingGame) bool {
	message = strings.TrimSpace(message)
	messageRunes := []rune(message)

	// Only accept single letter guesses
	if len(messageRunes) != 1 {
		return false
	}

	r := messageRunes[0]
	if !unicode.IsLetter(r) {
		return false
	}

	guess := strings.ToUpper(string(r))

	// Check if already guessed
	for _, g := range game.Guesses {
		if g == guess {
			c.SendChatMessage(fmt.Sprintf("%s: Already guessed '%s'!", sender, guess))
			return true
		}
	}

	// Add guess
	game.Guesses = append(game.Guesses, guess)

	// Check if letter is in word
	wordRunes := []rune(game.Word)
	found := false
	allRevealed := true

	for i, wr := range wordRunes {
		if strings.ToUpper(string(wr)) == guess {
			game.Revealed[i] = true
			found = true
		}
		if !game.Revealed[i] {
			allRevealed = false
		}
	}

	if !found {
		game.WrongGuesses = append(game.WrongGuesses, guess)
	}

	// Check for win
	if allRevealed {
		// Only award points for non-custom words
		if !game.CustomWord {
			AddWordlePoint(sender)
			points := GetWordlePoints(sender)
			c.SendChatMessage(fmt.Sprintf("Correct! %s completed %s! Wrong guesses: %d (Total: %d)",
				sender, game.Word, len(game.WrongGuesses), points))
		} else {
			c.SendChatMessage(fmt.Sprintf("Correct! %s completed %s! Wrong guesses: %d",
				sender, game.Word, len(game.WrongGuesses)))
		}
		wordGuessingManager.activeGame = nil
		return true
	}

	// Check for game over (only if MaxGuesses > 0, based on wrong guesses)
	if game.MaxGuesses > 0 && len(game.WrongGuesses) >= game.MaxGuesses {
		c.SendChatMessage(fmt.Sprintf("Game over! The word was: %s", game.Word))
		wordGuessingManager.activeGame = nil
		return true
	}

	// Show progress
	wrongInfo := ""
	if len(game.WrongGuesses) > 0 {
		if game.MaxGuesses > 0 {
			wrongInfo = fmt.Sprintf(" | Wrong (%d/%d): %s", len(game.WrongGuesses), game.MaxGuesses, strings.Join(game.WrongGuesses, ","))
		} else {
			wrongInfo = fmt.Sprintf(" | Wrong: %s", strings.Join(game.WrongGuesses, ","))
		}
	}

	if found {
		c.SendChatMessage(fmt.Sprintf("%s: '%s' is in the word! %s%s", sender, guess, game.GetProgressDisplay(), wrongInfo))
	} else {
		c.SendChatMessage(fmt.Sprintf("%s: '%s' is not in the word. %s%s", sender, guess, game.GetProgressDisplay(), wrongInfo))
	}

	return true
}

// ProcessWordleGuessAndReveal processes a wordle guess and updates revealed letters
// Returns a simple result string like "H.LL*" where:
// uppercase = correct position, lowercase = wrong position, . = not in word
func (g *WordGuessingGame) ProcessWordleGuessAndReveal(guess string) string {
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
func (g *WordGuessingGame) GetProgressDisplay() string {
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
	log.Printf("[WordGuessing] API failed, using fallback word list")
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
	for i := range length {
		if i%2 == 0 {
			result[i] = consonants[rand.Intn(len(consonants))]
		} else {
			result[i] = vowels[rand.Intn(len(vowels))]
		}
	}
	return string(result)
}
