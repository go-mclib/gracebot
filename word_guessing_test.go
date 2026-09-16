package main

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
)

func TestWordleRepeatedLetters(t *testing.T) {
	for _, tc := range []struct {
		guess string
		want  []string
	}{
		{"allee", []string{"correct", "present", "absent", "absent", "correct"}},
		{"eerie", []string{"absent", "absent", "absent", "absent", "correct"}},
		{"poppy", []string{"present", "absent", "correct", "absent", "absent"}},
	} {
		t.Run(tc.guess, func(t *testing.T) {
			if got := evaluateWordle("apple", tc.guess); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("feedback = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHangmanRejectedGuessesPreserveAttempts(t *testing.T) {
	ctx := context.Background()
	manager := newGameManager(nil) // A custom game must never access the scoring store.
	if _, err := manager.start(ctx, "Owner", json.RawMessage(`{"game":"hangman","word":"apple","attempts":2}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.guess(ctx, "Other", json.RawMessage(`{"guess":"z"}`)); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{"Z", "ab", "é", "1", ""} {
		raw, err := json.Marshal(map[string]string{"guess": guess})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.guess(ctx, "Other", raw); err == nil {
			t.Fatalf("accepted invalid or repeated guess %q", guess)
		}
	}
	if _, err := manager.start(ctx, "Other", json.RawMessage(`{"game":"wordle","word":"world"}`)); err == nil {
		t.Fatal("replaced an active shared game")
	}
	value, err := manager.status(ctx, "Other", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	state := value.(gameResult)
	if state.AttemptsUsed != 1 || state.GuessCount != 1 || state.RemainingAttempts == nil || *state.RemainingAttempts != 1 || state.Answer != "" {
		t.Fatalf("rejected guesses changed active state: %+v", state)
	}
	value, err = manager.guess(ctx, "Winner", json.RawMessage(`{"guess":"APPLE"}`))
	if err != nil {
		t.Fatal(err)
	}
	state = value.(gameResult)
	if state.Status != "won" || state.Answer != "apple" || state.Winner != "Winner" || state.PointsAwarded != 0 || state.WinnerPoints != nil {
		t.Fatalf("unexpected custom-word win: %+v", state)
	}
	if _, err := manager.guess(ctx, "Winner", json.RawMessage(`{"guess":"apple"}`)); err == nil {
		t.Fatal("accepted another guess after the win")
	}
}

func TestWordleProgressDoesNotAssembleActiveAnswer(t *testing.T) {
	ctx := context.Background()
	manager := newGameManager(nil)
	if _, err := manager.start(ctx, "Owner", json.RawMessage(`{"game":"wordle","word":"apple"}`)); err != nil {
		t.Fatal(err)
	}
	for _, guess := range []string{"aptly", "maple"} {
		raw, err := json.Marshal(map[string]string{"guess": guess})
		if err != nil {
			t.Fatal(err)
		}
		value, err := manager.guess(ctx, "Other", raw)
		if err != nil {
			t.Fatal(err)
		}
		state := value.(gameResult)
		if state.Status != "active" || state.Answer != "" || state.Progress == "apple" {
			t.Fatalf("ongoing game revealed answer: %+v", state)
		}
	}
}

func gameActionResult(t *testing.T, action func(context.Context, string, json.RawMessage) (any, error), actor, raw string) gameResult {
	t.Helper()
	value, err := action(context.Background(), actor, json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return value.(gameResult)
}

func TestHangmanCumulativeLettersWinWithoutWrongAttempts(t *testing.T) {
	manager := newGameManager(nil)
	gameActionResult(t, manager.start, "Owner", `{"game":"hangman","word":"apple","attempts":1}`)
	for i, step := range []struct{ guess, progress string }{
		{"p", "_pp__"}, {"a", "app__"}, {"l", "appl_"}, {"e", "apple"},
	} {
		state := gameActionResult(t, manager.guess, "Helper", `{"guess":"`+step.guess+`"}`)
		if state.Progress != step.progress || state.AttemptsUsed != 0 || state.GuessCount != i+1 {
			t.Fatalf("letter reveals were not cumulative: %+v", state)
		}
		if i < 3 && (state.Status != "active" || state.Answer != "") {
			t.Fatalf("game ended before all letters were found: %+v", state)
		}
		if i == 3 && (state.Status != "won" || state.Winner != "Helper" || state.Answer != "apple") {
			t.Fatalf("last letter did not win: %+v", state)
		}
	}
}

func TestWordleSnapshotsKeepLatestFeedbackDetached(t *testing.T) {
	manager := newGameManager(nil)
	gameActionResult(t, manager.start, "Owner", `{"game":"wordle","word":"apple","attempts":3}`)
	first := gameActionResult(t, manager.guess, "Alice", `{"guess":"allee"}`)
	want := []string{"correct", "present", "absent", "absent", "correct"}
	if first.Progress != "a___e" || !reflect.DeepEqual(first.RecentGuesses[0].Letters, want) {
		t.Fatalf("wrong repeated-letter feedback: %+v", first)
	}
	first.RecentGuesses[0].Letters[0] = "absent"
	first.RecentGuesses[0].Guess = "xxxxx"
	*first.RemainingAttempts = 99
	second := gameActionResult(t, manager.guess, "Bob", `{"guess":"poppy"}`)
	if second.Progress != "__p__" || second.AttemptsUsed != 2 || *second.RemainingAttempts != 1 ||
		second.RecentGuesses[0].Guess != "allee" || !reflect.DeepEqual(second.RecentGuesses[0].Letters, want) {
		t.Fatalf("snapshot mutation or old exact matches affected the engine: %+v", second)
	}
	if _, err := manager.guess(context.Background(), "Bob", json.RawMessage(`{"guess":"ALLEE"}`)); err == nil {
		t.Fatal("repeated Wordle guess consumed an attempt")
	}
	won := gameActionResult(t, manager.guess, "Carol", `{"guess":"apple"}`)
	if won.Status != "won" || won.AttemptsUsed != 3 || won.GuessCount != 3 || won.Answer != "apple" {
		t.Fatalf("correct final guess must win rather than lose: %+v", won)
	}
	if second.RecentGuesses[1].Letters[0] != "present" {
		t.Fatal("later guess changed an earlier snapshot")
	}
}

func TestAnagramFixedPuzzleAndAttemptBoundaries(t *testing.T) {
	manager := newGameManager(nil)
	if _, err := manager.start(context.Background(), "Owner", json.RawMessage(`{"game":"anagram","word":"aaa"}`)); err == nil {
		t.Fatal("accepted an anagram that can only display its answer")
	}
	start := gameActionResult(t, manager.start, "Owner", `{"game":"anagram","word":"apple","attempts":2}`)
	letters := []byte(start.Puzzle)
	slices.Sort(letters)
	if string(letters) != "aelpp" || start.Puzzle == "apple" || start.Answer != "" {
		t.Fatalf("invalid or revealed starting puzzle: %+v", start)
	}
	first := gameActionResult(t, manager.guess, "Helper", `{"guess":"ample"}`)
	for _, guess := range []string{"AMPLE", "p", "12345"} {
		if _, err := manager.guess(context.Background(), "Helper", json.RawMessage(`{"guess":"`+guess+`"}`)); err == nil {
			t.Fatalf("accepted invalid or duplicate anagram guess %q", guess)
		}
	}
	status := gameActionResult(t, manager.status, "Other", `{}`)
	if first.Puzzle != start.Puzzle || status.Puzzle != start.Puzzle || status.Answer != "" ||
		status.AttemptsUsed != 1 || status.GuessCount != 1 || *status.RemainingAttempts != 1 {
		t.Fatalf("puzzle or attempts changed unexpectedly: %+v", status)
	}
	lost := gameActionResult(t, manager.guess, "Other", `{"guess":"alien"}`)
	if lost.Status != "lost" || lost.Answer != "apple" || lost.Puzzle != start.Puzzle ||
		lost.AttemptsUsed != 2 || lost.PointsAwarded != 0 {
		t.Fatalf("wrong loss boundary: %+v", lost)
	}
	gameActionResult(t, manager.start, "Owner", `{"game":"anagram","word":"apple","attempts":1}`)
	won := gameActionResult(t, manager.guess, "Other", `{"guess":"APPLE"}`)
	if won.Status != "won" || won.AttemptsUsed != 1 || won.PointsAwarded != 0 || won.WinnerPoints != nil {
		t.Fatalf("custom final-attempt win awarded points or failed: %+v", won)
	}
}

func TestNumberValidationNarrowingAndTerminalBoundaries(t *testing.T) {
	manager := newGameManager(testPlayerStore(t))
	for _, raw := range []string{`{"game":"number","word":"apple"}`, `{"game":"number","length":5}`} {
		if _, err := manager.start(context.Background(), "Owner", json.RawMessage(raw)); err == nil {
			t.Fatal("number game accepted word-game options")
		}
	}
	start := gameActionResult(t, manager.start, "Owner", `{"game":"number","attempts":4}`)
	manager.active.word = "42"
	if start.Answer != "" || start.Length != 0 || start.Progress != "" || start.Lower != 1 || start.Upper != 100 {
		t.Fatalf("number start leaked answer information: %+v", start)
	}
	for i, step := range []struct {
		guess, hint  string
		lower, upper int
	}{
		{"25", "higher", 26, 100},
		{"75", "lower", 26, 74},
		{"20", "higher", 26, 74},
	} {
		state := gameActionResult(t, manager.guess, "Helper", `{"guess":"`+step.guess+`"}`)
		if state.Hint != step.hint || state.Lower != step.lower || state.Upper != step.upper ||
			state.AttemptsUsed != i+1 || state.Answer != "" || state.Status != "active" {
			t.Fatalf("incorrect range feedback: %+v", state)
		}
	}
	for _, guess := range []string{"025", "0", "101", "4.2", "+42", "apple"} {
		if _, err := manager.guess(context.Background(), "Helper", json.RawMessage(`{"guess":"`+guess+`"}`)); err == nil {
			t.Fatalf("accepted invalid or duplicate number %q", guess)
		}
	}
	state := gameActionResult(t, manager.status, "Other", `{}`)
	if state.AttemptsUsed != 3 || state.GuessCount != 3 || *state.RemainingAttempts != 1 {
		t.Fatalf("rejected numbers consumed attempts: %+v", state)
	}
	won := gameActionResult(t, manager.guess, "Winner", `{"guess":"42"}`)
	if won.Status != "won" || won.Answer != "42" || won.AttemptsUsed != 4 || won.PointsAwarded != 1 || won.WinnerPoints == nil || *won.WinnerPoints != 1 {
		t.Fatalf("last number attempt did not win: %+v", won)
	}
	gameActionResult(t, manager.start, "Owner", `{"game":"number","attempts":1}`)
	manager.active.word = "42"
	lost := gameActionResult(t, manager.guess, "Other", `{"guess":"41"}`)
	if lost.Status != "lost" || lost.Answer != "42" || lost.PointsAwarded != 0 || lost.AttemptsUsed != 1 {
		t.Fatalf("wrong number loss boundary: %+v", lost)
	}
	if state := gameActionResult(t, manager.status, "Other", `{}`); state.Status != "none" {
		t.Fatalf("terminal game remains active: %+v", state)
	}
}

func TestRandomGamesAwardOneCooperativeWinner(t *testing.T) {
	for _, game := range []string{"hangman", "wordle", "anagram", "number"} {
		t.Run(game, func(t *testing.T) {
			store := testPlayerStore(t)
			manager := newGameManager(store)
			start := gameActionResult(t, manager.start, "Owner", `{"game":"`+game+`"}`)
			if start.Answer != "" {
				t.Fatal("random game exposed its answer")
			}
			answer := manager.active.word
			if game == "number" {
				number, err := strconv.Atoi(answer)
				if err != nil || number < 1 || number > 100 || start.MaxAttempts != 7 {
					t.Fatalf("number game cannot be solved within its advertised domain: %+v", start)
				}
			}
			var wg sync.WaitGroup
			results := make(chan gameResult, 2)
			for _, actor := range []string{"Alice", "Bob"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					value, err := manager.guess(context.Background(), actor, json.RawMessage(`{"guess":"`+answer+`"}`))
					if err == nil {
						results <- value.(gameResult)
					}
				}()
			}
			wg.Wait()
			close(results)
			winners := 0
			for result := range results {
				winners++
				if result.Status != "won" || result.Answer != answer || result.PointsAwarded != 1 ||
					result.Winner == "Owner" || result.WinnerPoints == nil || *result.WinnerPoints != 1 {
					t.Fatalf("incorrect cooperative award: %+v", result)
				}
			}
			var points int
			if err := store.db.QueryRow("SELECT COALESCE(SUM(points),0) FROM scores WHERE game=?", game).Scan(&points); err != nil {
				t.Fatal(err)
			}
			if winners != 1 || points != 1 {
				t.Fatalf("simultaneous guesses awarded %d winners and %d points", winners, points)
			}
		})
	}
}

func TestGameUnlimitedAndStopAuthority(t *testing.T) {
	manager := newGameManager(testPlayerStore(t))
	start := gameActionResult(t, manager.start, "Owner", `{"game":"number","attempts":0}`)
	manager.active.word = "42"
	if !start.Unlimited || start.RemainingAttempts != nil {
		t.Fatalf("explicit zero did not enable unlimited guesses: %+v", start)
	}
	for i := 1; i <= 8; i++ {
		state := gameActionResult(t, manager.guess, "Helper", `{"guess":"`+strconv.Itoa(i)+`"}`)
		if state.Status != "active" || state.AttemptsUsed != i || state.RemainingAttempts != nil {
			t.Fatalf("unlimited game stopped counting or ended: %+v", state)
		}
	}
	if _, err := manager.stop(context.Background(), "Other", json.RawMessage(`{}`)); err == nil {
		t.Fatal("non-owner stopped the shared game")
	}
	stopped := gameActionResult(t, manager.stop, "RealAdmin", `{}`)
	if stopped.Status != "stopped" || stopped.Answer != "42" || stopped.PointsAwarded != 0 {
		t.Fatalf("administrator stop did not reveal the answer without points: %+v", stopped)
	}
	gameActionResult(t, manager.start, "Owner", `{"game":"anagram","word":"apple"}`)
	stopped = gameActionResult(t, manager.stop, "OWNER", `{}`)
	if stopped.Status != "stopped" || stopped.Answer != "apple" || stopped.PointsAwarded != 0 {
		t.Fatalf("case-insensitive owner could not stop game: %+v", stopped)
	}
}
