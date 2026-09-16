package main

import (
	"fmt"
	"strings"
)

// renderGameReply renders only the engine's detached snapshot, never a clue
// inferred from an answer. Optional history is omitted before current feedback.
func renderGameReply(action string, result gameResult) string {
	if result.Status == "none" {
		return "game: no shared game is active"
	}
	status := result.Status
	if action == "start_game" && status == "active" {
		status = "started"
	}
	var line strings.Builder
	line.Grow(256)
	line.WriteString(gameReplyASCII(result.Game, 16))
	line.WriteByte(' ')
	line.WriteString(gameReplyASCII(status, 16))
	add := func(text string) bool {
		if line.Len()+2+len(text) > 256 {
			return false
		}
		line.WriteString("; ")
		line.WriteString(text)
		return true
	}
	budget := "tries"
	if result.Game == "hangman" {
		budget = "wrong guesses"
	}
	if result.Unlimited || result.MaxAttempts == 0 {
		add(fmt.Sprintf("%s used %d, unlimited", budget, result.AttemptsUsed))
	} else {
		remaining := result.MaxAttempts - result.AttemptsUsed
		if result.RemainingAttempts != nil {
			remaining = *result.RemainingAttempts
		}
		add(fmt.Sprintf("%s left %d/%d", budget, remaining, result.MaxAttempts))
	}
	ended := result.Status == "won" || result.Status == "lost" || result.Status == "stopped"
	if ended {
		if result.Answer != "" {
			add("answer " + gameReplyASCII(result.Answer, 15))
		}
		if result.Winner != "" {
			add("winner " + gameReplyASCII(result.Winner, 24))
		}
		if result.PointsAwarded > 0 {
			points := fmt.Sprintf("+%d points", result.PointsAwarded)
			if result.WinnerPoints != nil {
				points += fmt.Sprintf(" (total %d)", *result.WinnerPoints)
			}
			add(points)
		}
	} else {
		switch result.Game {
		case "hangman":
			add("mask " + gameReplyASCII(result.Progress, 15))
		case "wordle":
			if len(result.RecentGuesses) == 0 {
				add(fmt.Sprintf("%d letters", result.Length))
			}
		case "anagram":
			add("letters " + gameReplyASCII(result.Puzzle, 15))
		case "number":
			add(fmt.Sprintf("range %d..%d", result.Lower, result.Upper))
			if result.Hint != "" {
				add(gameReplyASCII(result.Hint, 32))
			} else {
				add("higher/lower hints")
			}
		}
	}
	if n := len(result.RecentGuesses); n > 0 {
		latest := result.RecentGuesses[n-1]
		player := gameReplyASCII(latest.Player, 24)
		guess := gameReplyASCII(latest.Guess, 15)
		if result.Game == "wordle" {
			var marks strings.Builder
			for i := range len(guess) {
				if i > 0 {
					marks.WriteByte(' ')
				}
				marks.WriteByte(guess[i])
				mark := byte('?')
				if i < len(latest.Letters) {
					switch latest.Letters[i] {
					case "correct":
						mark = '='
					case "present":
						mark = '~'
					case "absent":
						mark = '-'
					}
				}
				marks.WriteByte(mark)
			}
			// Keep the legend attached to its feedback, even in a long end result.
			add("latest " + player + ": " + marks.String() + " (=right spot ~wrong spot -no match)")
		} else {
			feedback := "miss"
			if latest.Correct {
				feedback = "hit"
			}
			add(player + ": " + guess + " (" + feedback + ")")
		}
	}
	if !ended && (action == "start_game" || action == "game_status") {
		if result.Game == "anagram" {
			add("all players: bare guesses or guess ...")
		} else {
			add("all: guess ...; participants: bare guesses")
		}
	}
	if !ended && result.Game == "hangman" && len(result.RecentGuesses) > 1 {
		var history strings.Builder
		history.WriteString("recent (+hit,-miss)")
		count := 0
		for i := len(result.RecentGuesses) - 2; i >= 0 && count < 6; i-- {
			entry := result.RecentGuesses[i]
			if len(entry.Guess) != 1 && entry.Correct {
				continue
			}
			guess := gameReplyASCII(entry.Guess, 15)
			if line.Len()+2+history.Len()+len(guess)+2 > 256 {
				break
			}
			history.WriteByte(' ')
			history.WriteString(guess)
			if entry.Correct {
				history.WriteByte('+')
			} else {
				history.WriteByte('-')
			}
			count++
		}
		if count > 0 {
			add(history.String())
		}
	}
	return line.String()
}

// Minecraft names and game values are ASCII; sanitize externally supplied
// labels too so every reply remains a single printable, byte-bounded line.
func gameReplyASCII(value string, limit int) string {
	end := min(len(value), limit)
	printable := true
	for i := range end {
		if value[i] < ' ' || value[i] > '~' {
			printable = false
			break
		}
	}
	if printable {
		return value[:end]
	}
	var text strings.Builder
	text.Grow(min(len(value), limit))
	for _, ch := range value {
		if text.Len() == limit {
			break
		}
		if ch < ' ' || ch > '~' {
			text.WriteByte('?')
		} else {
			text.WriteByte(byte(ch))
		}
	}
	return text.String()
}
