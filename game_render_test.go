package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderWordleUsesLatestRepeatedLetterFeedback(t *testing.T) {
	manager := newGameManager(nil)
	ctx := context.Background()
	if _, err := manager.start(ctx, "Owner", json.RawMessage(`{"game":"wordle","word":"music"}`)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		guess string
		marks string
		left  string
	}{
		{"jesus", "j- e- s= u~ s-", "5/6"},
		{"acorn", "a- c~ o- r- n-", "4/6"},
		{"music", "m= u= s= i= c=", "3/6"},
	} {
		raw, err := json.Marshal(map[string]string{"guess": tc.guess})
		if err != nil {
			t.Fatal(err)
		}
		value, err := manager.guess(ctx, "ActualPlayer", raw)
		if err != nil {
			t.Fatal(err)
		}
		result := value.(gameResult)
		line := renderGameReply("guess", result)
		for _, want := range []string{"wordle", "ActualPlayer", tc.marks, tc.left, "=right spot", "~wrong spot", "-no match"} {
			if !strings.Contains(line, want) {
				t.Errorf("guess %s: %q does not contain %q", tc.guess, line, want)
			}
		}
		if strings.Contains(line, "mask") || (result.Status == "active" && strings.Contains(line, "music")) {
			t.Errorf("wordle exposed answer or cumulative mask: %q", line)
		}
		if tc.guess == "acorn" && strings.Contains(line, "s=") {
			t.Errorf("old exact match appeared in latest feedback: %q", line)
		}
	}
}

func TestRenderHangmanKeepsCumulativeMaskAndWrongBudget(t *testing.T) {
	manager := newGameManager(nil)
	ctx := context.Background()
	if _, err := manager.start(ctx, "Owner", json.RawMessage(`{"game":"hangman","word":"music"}`)); err != nil {
		t.Fatal(err)
	}
	var result gameResult
	for _, guess := range []string{"s", "m", "z"} {
		raw, err := json.Marshal(map[string]string{"guess": guess})
		if err != nil {
			t.Fatal(err)
		}
		value, err := manager.guess(ctx, "ActualPlayer", raw)
		if err != nil {
			t.Fatal(err)
		}
		result = value.(gameResult)
	}
	line := renderGameReply("guess", result)
	for _, want := range []string{"hangman", "mask m_s__", "wrong guesses left 5/6", "ActualPlayer: z (miss)", "s+", "m+"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q does not contain %q", line, want)
		}
	}
	if strings.Contains(line, "music") || strings.Contains(line, "~wrong spot") {
		t.Errorf("hangman revealed answer or used wordle feedback: %q", line)
	}
}

func TestRenderActiveSecretsAndExtraGameFields(t *testing.T) {
	for _, tc := range []struct {
		game     string
		progress string
		puzzle   string
		want     string
	}{
		{"hangman", "_____", "", "mask _____"},
		{"wordle", "secretvalue", "", "5 letters"},
		{"anagram", "secretvalue", "canro", "letters canro"},
		{"number", "secretvalue", "", "range 1..100"},
	} {
		t.Run(tc.game, func(t *testing.T) {
			result := gameResult{
				Status: "active", Game: tc.game, Answer: "secretvalue", Length: 5,
				Progress: tc.progress, Puzzle: tc.puzzle, Lower: 1, Upper: 100,
				MaxAttempts: 7,
			}
			for _, action := range []string{"start_game", "game_status"} {
				line := renderGameReply(action, result)
				for _, want := range []string{tc.game, tc.want, "7/7"} {
					if !strings.Contains(line, want) {
						t.Errorf("%q does not contain %q", line, want)
					}
				}
				if strings.Contains(line, "secretvalue") || strings.Contains(line, "answer") {
					t.Errorf("active answer leaked: %q", line)
				}
				if tc.game == "number" && !strings.Contains(line, "higher/lower") {
					t.Errorf("number instructions omit hint rules: %q", line)
				}
			}
		})
	}
	line := renderGameReply("guess", gameResult{
		Status: "active", Game: "number", MaxAttempts: 7, AttemptsUsed: 1,
		Lower: 51, Upper: 100, Hint: "higher",
		RecentGuesses: []gameGuess{{Player: "NumberPlayer", Guess: "50"}},
	})
	for _, want := range []string{"range 51..100", "higher", "NumberPlayer: 50", "6/7"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q does not contain %q", line, want)
		}
	}
}

func TestRenderTerminalResultUsesOnlyRevealedAnswerAndActualScore(t *testing.T) {
	total := 9
	for _, status := range []string{"won", "lost", "stopped"} {
		result := gameResult{
			Status: status, Game: "wordle", Answer: "acorn", Progress: "decoy",
			MaxAttempts: 6, AttemptsUsed: 6,
		}
		if status == "won" {
			result.Winner = "RealWinner"
			result.PointsAwarded = 1
			result.WinnerPoints = &total
		}
		line := renderGameReply("stop_game", result)
		for _, want := range []string{status, "answer acorn", "0/6"} {
			if !strings.Contains(line, want) {
				t.Errorf("%q does not contain %q", line, want)
			}
		}
		if strings.Contains(line, "decoy") {
			t.Errorf("terminal reply inferred an answer from progress: %q", line)
		}
		if status == "won" {
			for _, want := range []string{"RealWinner", "+1 points", "total 9"} {
				if !strings.Contains(line, want) {
					t.Errorf("%q does not contain %q", line, want)
				}
			}
		} else if strings.Contains(line, "winner") || strings.Contains(line, "points") {
			t.Errorf("unawarded winner or score invented: %q", line)
		}
	}
}

func TestRenderLongGameBoundsHistoryAndKeepsNewestFeedback(t *testing.T) {
	recent := make([]gameGuess, 1000)
	for i := range recent {
		recent[i] = gameGuess{Player: "OldPlayer", Guess: "oldwrongword"}
	}
	for i := len(recent) - 8; i < len(recent)-1; i++ {
		recent[i].Guess = "recentwrongword"
	}
	recent[len(recent)-1] = gameGuess{Player: "LatestPlayer\n\u00e9", Guess: "z"}
	line := renderGameReply("game_status", gameResult{
		Status: "active", Game: "hangman", Progress: "m_s__", Unlimited: true,
		AttemptsUsed: 1000, RecentGuesses: recent,
	})
	if len(line) > 256 {
		t.Fatalf("reply exceeds Minecraft chat limit: %d bytes", len(line))
	}
	for _, ch := range line {
		if ch < ' ' || ch > '~' {
			t.Fatalf("reply contains non-printable or non-ASCII character: %q", line)
		}
	}
	for _, want := range []string{"LatestPlayer??: z (miss)", "mask m_s__", "1000", "unlimited", "recentwrongword-"} {
		if !strings.Contains(line, want) {
			t.Errorf("%q does not contain %q", line, want)
		}
	}
	if strings.Contains(line, "oldwrongword") {
		t.Errorf("old guesses displaced recent history: %q", line)
	}
}
