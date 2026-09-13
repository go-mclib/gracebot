package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
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
		makeTool("start_game", "Start a shared cooperative Hangman or Wordle game. Any player may start. Refuses to replace an active game. Custom words earn no points; never repeat a custom answer in chat. Hangman charges only wrong guesses; Wordle charges every guess.", map[string]any{
			"game":     map[string]any{"type": "string", "enum": []string{"hangman", "wordle"}},
			"length":   map[string]any{"type": "integer", "minimum": 3, "maximum": 15, "description": "Default 5 for a random word; inferred from a custom word if omitted."},
			"word":     map[string]any{"type": "string", "minLength": 3, "maxLength": 15, "pattern": "^[A-Za-z]+$", "description": "Optional custom answer; ASCII English letters only."},
			"attempts": map[string]any{"type": "integer", "minimum": 0, "description": "Defaults to 6. Explicit 0 means unlimited."},
		}, []string{"game"}, m.start),
		makeTool("guess", "Submit a guess to the shared game as the requesting player. Hangman accepts a single letter or a full word; Wordle requires a full word of the game's length. Repeated guesses do not consume attempts. Wordle letter feedback is correct, present, or absent, with duplicate letters counted correctly.", map[string]any{
			"guess": map[string]any{"type": "string", "minLength": 1, "maxLength": 15, "pattern": "^[A-Za-z]+$"},
		}, []string{"guess"}, m.guess),
		makeTool("game_status", "Show the shared game, progress, attempts, and up to ten recent guesses without revealing the active answer. Everyone plays the same game.", nil, nil, m.status),
		makeTool("stop_game", "Stop the shared game and reveal its answer. Only the player who started it or an administrator may stop it. Stopping awards no points.", nil, nil, m.stop),
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
	if args.Game != "hangman" && args.Game != "wordle" {
		return nil, fmt.Errorf("game must be hangman or wordle")
	}
	length, attempts := 5, 6
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
	if length < 3 || length > 15 {
		return nil, fmt.Errorf("word length must be between 3 and 15")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m.active != nil {
		return nil, fmt.Errorf("a shared %s game is already active; finish or stop it first", m.active.game)
	}
	if args.Word == nil {
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
	return m.active.snapshot("active"), nil
}

func (m *gameManager) guess(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
	var args struct {
		Guess string `json:"guess"`
	}
	if err := decodeArgs(raw, &args); err != nil {
		return nil, err
	}
	if len(args.Guess) > 15 || !asciiGameWord(args.Guess) {
		return nil, fmt.Errorf("guess must contain 1 to 15 ASCII English letters")
	}
	guess := strings.ToLower(args.Guess)
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g := m.active
	if g == nil {
		return nil, fmt.Errorf("no shared game is active")
	}
	if len(guess) != len(g.word) && (g.game != "hangman" || len(guess) != 1) {
		if g.game == "hangman" {
			return nil, fmt.Errorf("guess one letter or a %d-letter word", len(g.word))
		}
		return nil, fmt.Errorf("guess a %d-letter word", len(g.word))
	}
	if _, exists := g.seen[guess]; exists {
		return nil, fmt.Errorf("that guess was already played; no attempt was used")
	}
	g.seen[guess] = struct{}{}
	g.guessCount++
	entry := gameGuess{Player: actor, Guess: guess}
	won := guess == g.word
	if g.game == "wordle" {
		g.attemptsUsed++
		entry.Correct = won
		entry.Letters = evaluateWordle(g.word, guess)
		for i, result := range entry.Letters {
			g.revealed[i] = result == "correct"
		}
	} else {
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
	progress := make([]byte, len(g.word))
	for i := range progress {
		progress[i] = '_'
		if g.revealed[i] {
			progress[i] = g.word[i]
		}
	}
	result := gameResult{
		Status: status, Cooperative: true, Game: g.game, Owner: g.owner,
		Length: len(g.word), CustomWord: g.custom, Progress: string(progress),
		MaxAttempts: g.maxAttempts, Unlimited: g.maxAttempts == 0,
		AttemptsUsed: g.attemptsUsed, GuessCount: g.guessCount,
		RecentGuesses: append([]gameGuess(nil), g.recent...),
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
