package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
)

const gameHistoryLimit = 10

type gameManager struct {
	mu     sync.Mutex
	store  *playerStore
	active *wordGame
}

type wordGame struct {
	game         string
	word         string
	owner        string
	custom       bool
	maxAttempts  int
	attemptsUsed int
	guessCount   int
	seen         map[string]struct{}
	revealed     []bool
	recent       []gameGuess
	puzzle       string
	hint         string
	lower        int
	upper        int
}

type gameGuess struct {
	Player  string   `json:"player"`
	Guess   string   `json:"guess"`
	Correct bool     `json:"correct"`
	Letters []string `json:"letters,omitempty"`
}

// gameResult is a detached snapshot; the answer is added only after a game ends.
// Wordle progress reflects the latest guess, rather than combining clues into
// an entire answer while the game is still active.
type gameResult struct {
	Status            string      `json:"status"`
	Cooperative       bool        `json:"cooperative"`
	Game              string      `json:"game,omitempty"`
	Owner             string      `json:"owner,omitempty"`
	Length            int         `json:"length,omitempty"`
	CustomWord        bool        `json:"custom_word"`
	Progress          string      `json:"progress,omitempty"`
	Puzzle            string      `json:"puzzle,omitempty"`
	Hint              string      `json:"hint,omitempty"`
	Lower             int         `json:"lower,omitempty"`
	Upper             int         `json:"upper,omitempty"`
	MaxAttempts       int         `json:"max_attempts"`
	Unlimited         bool        `json:"unlimited"`
	AttemptsUsed      int         `json:"attempts_used"`
	RemainingAttempts *int        `json:"remaining_attempts,omitempty"`
	GuessCount        int         `json:"guess_count"`
	RecentGuesses     []gameGuess `json:"recent_guesses,omitempty"`
	Answer            string      `json:"answer,omitempty"`
	Winner            string      `json:"winner,omitempty"`
	PointsAwarded     int         `json:"points_awarded"`
	WinnerPoints      *int        `json:"winner_points,omitempty"`
}

func newGameManager(store *playerStore) *gameManager {
	return &gameManager{store: store}
}

func (m *gameManager) tools() []toolSpec {
	return []toolSpec{
		makeTool("start_game", "Start a shared cooperative Hangman, Wordle, anagram, or number game. The runtime sends the built-in game reply directly; do not invent clues or repeat the answer. Refuses to replace an active game. Custom words earn no points. Hangman charges only wrong guesses; other games charge every valid, nonduplicate guess.", map[string]any{
			"game":     map[string]any{"type": "string", "enum": []string{"hangman", "wordle", "anagram", "number"}},
			"length":   map[string]any{"type": "integer", "minimum": 3, "maximum": 15, "description": "Word games only. Default 5 for a random word; inferred from a custom word if omitted."},
			"word":     map[string]any{"type": "string", "minLength": 3, "maxLength": 15, "pattern": "^[A-Za-z]+$", "description": "Word games only. Optional custom answer; ASCII English letters only."},
			"attempts": map[string]any{"type": "integer", "minimum": 0, "description": "Defaults to 7 for number, 6 otherwise. Explicit 0 means unlimited."},
		}, []string{"game"}, m.start),
		makeTool("game_status", "Ask the runtime to send the built-in shared game status directly, without revealing the active answer. Everyone plays the same game.", nil, nil, m.status),
		makeTool("stop_game", "Stop the shared game. The runtime sends the built-in reply and reveals its answer directly. Only the player who started it or an administrator may stop it. Stopping awards no points.", nil, nil, m.stop),
	}
}

func (m *gameManager) start(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Game     string  `json:"game"`
		Length   *int    `json:"length"`
		Word     *string `json:"word"`
		Attempts *int    `json:"attempts"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if args.Game != "hangman" && args.Game != "wordle" && args.Game != "anagram" && args.Game != "number" {
		return nil, fmt.Errorf("game must be hangman, wordle, anagram, or number")
	}
	length, attempts := 5, 6
	if args.Game == "number" {
		if args.Length != nil || args.Word != nil {
			return nil, fmt.Errorf("number games do not accept length or word")
		}
		length, attempts = 0, 7
	}
	if args.Length != nil {
		length = *args.Length
	}
	if args.Attempts != nil {
		attempts = *args.Attempts
	}
	if attempts < 0 {
		return nil, fmt.Errorf("attempts must be nonnegative; 0 means unlimited")
	}
	word := ""
	if args.Word != nil {
		if !asciiGameWord(*args.Word) {
			return nil, fmt.Errorf("word must contain only ASCII English letters")
		}
		word = strings.ToLower(*args.Word)
		if args.Length != nil && length != len(word) {
			return nil, fmt.Errorf("length must match the custom word length")
		}
		length = len(word)
	}
	if args.Game != "number" && (length < 3 || length > 15) {
		return nil, fmt.Errorf("word length must be between 3 and 15")
	}
	if args.Game == "anagram" && args.Word != nil && strings.Trim(word, word[:1]) == "" {
		return nil, fmt.Errorf("anagram words must contain at least two distinct letters")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.active != nil {
		return nil, fmt.Errorf("a shared %s game is already active; finish or stop it first", m.active.game)
	}
	if args.Game == "number" {
		word = strconv.Itoa(rand.IntN(100) + 1)
	} else if args.Word == nil {
		var err error
		word, err = randomGameWord(length)
		if err != nil {
			return nil, err
		}
	}
	m.active = &wordGame{
		game: args.Game, word: word, owner: actor, custom: args.Word != nil,
		maxAttempts: attempts, seen: make(map[string]struct{}), revealed: make([]bool, length),
	}
	if args.Game == "number" {
		m.active.lower, m.active.upper = 1, 100
	} else if args.Game == "anagram" {
		letters := []byte(word)
		rand.Shuffle(len(letters), func(i, j int) {
			letters[i], letters[j] = letters[j], letters[i]
		})
		// Guarantee the clue differs from the answer without retrying a shuffle.
		if string(letters) == word {
			for i := 1; i < len(letters); i++ {
				if letters[i] != letters[0] {
					letters[0], letters[i] = letters[i], letters[0]
					break
				}
			}
		}
		m.active.puzzle = string(letters)
	}
	return m.active.snapshot("active"), nil
}

func (m *gameManager) guess(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Guess string `json:"guess"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g := m.active
	if g == nil {
		return nil, fmt.Errorf("no shared game is active")
	}
	guess := strings.ToLower(args.Guess)
	number := 0
	if g.game == "number" {
		if len(guess) < 1 || len(guess) > 3 {
			return nil, fmt.Errorf("guess a whole number from 1 to 100")
		}
		for i := range len(guess) {
			if guess[i] < '0' || guess[i] > '9' {
				return nil, fmt.Errorf("guess a whole number from 1 to 100")
			}
		}
		number, _ = strconv.Atoi(guess)
		if number < 1 || number > 100 {
			return nil, fmt.Errorf("guess a whole number from 1 to 100")
		}
		guess = strconv.Itoa(number)
	} else {
		if len(guess) > 15 || !asciiGameWord(guess) {
			return nil, fmt.Errorf("guess must contain 1 to 15 ASCII English letters")
		}
		if len(guess) != len(g.word) && (g.game != "hangman" || len(guess) != 1) {
			if g.game == "hangman" {
				return nil, fmt.Errorf("guess one letter or a %d-letter word", len(g.word))
			}
			return nil, fmt.Errorf("guess a %d-letter word", len(g.word))
		}
	}
	if _, exists := g.seen[guess]; exists {
		return nil, fmt.Errorf("that guess was already played; no attempt was used")
	}
	g.seen[guess] = struct{}{}
	g.guessCount++
	entry := gameGuess{Player: actor, Guess: guess}
	won := guess == g.word
	switch g.game {
	case "wordle":
		g.attemptsUsed++
		entry.Correct = won
		entry.Letters = evaluateWordle(g.word, guess)
		for i, result := range entry.Letters {
			g.revealed[i] = result == "correct"
		}
	case "hangman":
		if len(guess) == 1 {
			for i := range len(g.word) {
				if g.word[i] == guess[0] {
					g.revealed[i] = true
					entry.Correct = true
				}
			}
			won = true
			for _, revealed := range g.revealed {
				won = won && revealed
			}
		} else {
			entry.Correct = won
			if won {
				for i := range g.revealed {
					g.revealed[i] = true
				}
			}
		}
		if !entry.Correct {
			g.attemptsUsed++
		}
	case "anagram":
		g.attemptsUsed++
		entry.Correct = won
	case "number":
		g.attemptsUsed++
		entry.Correct = won
		answer, _ := strconv.Atoi(g.word)
		switch {
		case number < answer:
			g.hint = "higher"
			g.lower = max(g.lower, number+1)
		case number > answer:
			g.hint = "lower"
			g.upper = min(g.upper, number-1)
		default:
			g.hint = ""
			g.lower, g.upper = answer, answer
		}
	}
	if len(g.recent) == gameHistoryLimit {
		copy(g.recent, g.recent[1:])
		g.recent[len(g.recent)-1] = entry
	} else {
		g.recent = append(g.recent, entry)
	}
	if won {
		result := g.snapshot("won")
		result.Winner = actor
		if !g.custom {
			points := m.store.addPoint(actor, g.game)
			result.PointsAwarded = 1
			result.WinnerPoints = &points
		}
		m.active = nil
		return result, nil
	}
	if g.maxAttempts > 0 && g.attemptsUsed >= g.maxAttempts {
		result := g.snapshot("lost")
		m.active = nil
		return result, nil
	}
	return g.snapshot("active"), nil
}

func (m *gameManager) status(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.active == nil {
		return gameResult{Status: "none", Cooperative: true}, nil
	}
	return m.active.snapshot("active"), nil
}

func (m *gameManager) stop(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	if err := decodeArgs(raw, &struct{}{}); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.active == nil {
		return nil, fmt.Errorf("no shared game is active")
	}
	if !strings.EqualFold(actor, m.active.owner) && !m.store.hasRank(actor, "admin") {
		return nil, fmt.Errorf("only the game's owner or an administrator may stop it")
	}
	result := m.active.snapshot("stopped")
	m.active = nil
	return result, nil
}

func (g *wordGame) snapshot(status string) gameResult {
	var progress string
	if g.game != "number" {
		letters := make([]byte, len(g.word))
		for i := range letters {
			letters[i] = '_'
			if g.revealed[i] {
				letters[i] = g.word[i]
			}
		}
		progress = string(letters)
	}
	result := gameResult{
		Status: status, Cooperative: true, Game: g.game, Owner: g.owner,
		CustomWord: g.custom, Progress: progress,
		Puzzle: g.puzzle, Hint: g.hint, Lower: g.lower, Upper: g.upper,
		MaxAttempts: g.maxAttempts, Unlimited: g.maxAttempts == 0,
		AttemptsUsed: g.attemptsUsed, GuessCount: g.guessCount,
		RecentGuesses: append([]gameGuess(nil), g.recent...),
	}
	if g.game != "number" {
		result.Length = len(g.word)
	}
	for i := range result.RecentGuesses {
		result.RecentGuesses[i].Letters = append([]string(nil), g.recent[i].Letters...)
	}
	if g.maxAttempts > 0 {
		remaining := g.maxAttempts - g.attemptsUsed
		result.RemainingAttempts = &remaining
	}
	if status == "won" || status == "lost" || status == "stopped" {
		result.Answer = g.word
	}
	return result
}

func asciiGameWord(word string) bool {
	if word == "" {
		return false
	}
	for i := range len(word) {
		c := word[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return true
}

// Exact matches consume letters first, so an extra duplicate cannot steal a
// present mark from an exact match later in the word.
func evaluateWordle(word, guess string) []string {
	result := make([]string, len(word))
	var remaining [26]int
	for i := range len(word) {
		if word[i] == guess[i] {
			result[i] = "correct"
		} else {
			remaining[word[i]-'a']++
		}
	}
	for i := range len(word) {
		if result[i] == "correct" {
			continue
		}
		letter := guess[i] - 'a'
		if remaining[letter] > 0 {
			result[i] = "present"
			remaining[letter]--
		} else {
			result[i] = "absent"
		}
	}
	return result
}

func randomGameWord(length int) (string, error) {
	var words []string
	switch length {
	case 3:
		words = []string{"cat", "dog", "sun", "run", "fun", "hat", "bat", "car", "jar", "bar"}
	case 4:
		words = []string{"word", "game", "play", "time", "love", "home", "book", "tree", "star", "moon"}
	case 5:
		words = []string{"world", "games", "words", "music", "happy", "party", "money", "water", "light", "dream"}
	case 6:
		words = []string{"friend", "family", "player", "garden", "castle", "dragon", "knight", "wizard", "forest", "planet"}
	case 7:
		words = []string{"another", "morning", "evening", "weather", "journey", "mystery", "fantasy", "kingdom", "diamond", "rainbow"}
	case 8:
		words = []string{"computer", "building", "mountain", "treasure", "champion", "surprise", "strength", "football", "birthday", "shopping"}
	case 9:
		words = []string{"adventure", "beautiful", "challenge", "dangerous", "excellent", "fantastic", "happiness", "important", "knowledge", "wonderful"}
	case 10:
		words = []string{"basketball", "absolutely", "background", "celebrated", "determined", "everything", "friendship", "generation", "helicopter", "impossible"}
	case 11:
		words = []string{"celebration", "imagination", "information", "development", "countryside", "personality"}
	case 12:
		words = []string{"appreciation", "construction", "conversation", "intelligence", "relationship", "thunderstorm"}
	case 13:
		words = []string{"communication", "understanding", "entertainment", "determination", "extraordinary"}
	case 14:
		words = []string{"responsibility", "administration", "transportation", "identification", "recommendation"}
	case 15:
		words = []string{"congratulations", "characteristics", "representatives", "instrumentation", "experimentation"}
	default:
		return "", fmt.Errorf("word length must be between 3 and 15")
	}
	return words[rand.IntN(len(words))], nil
}
