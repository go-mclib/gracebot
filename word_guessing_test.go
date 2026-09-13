package main

import (
	"context"
	"encoding/json"
	"reflect"
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
