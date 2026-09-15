package main

import (
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	completedFollowupWindow = 15 * time.Second
	maxRoutingNames         = 256
	maxRoutingPrompts       = 32
)

type routingPrompt struct {
	actor, display string
	at             time.Time
}

// Configured bot names override public display-label collisions for routing,
// without granting those labels any privileges or changing the actor.
func (e *conversationEngagement) setBotNames(username string, aliases []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.botNames = make(map[string]bool, len(aliases)+1)
	e.botNames[routingName(username)] = true
	for _, alias := range aliases {
		if name := routingName(alias); name != "" {
			e.botNames[name] = true
		}
	}
}

// observe sees retained public chat only. Names and display labels are audience
// hints, never authenticated identities. Empty text only registers a name.
// Cancellation belongs in enqueue, where the actual mention flag is available.
func (e *conversationEngagement) observe(sender, display, text string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := sender
	if isDiscordActor(sender) {
		name = display
	}
	name = routingName(name)
	if name != "" {
		if _, exists := e.names[name]; !exists && len(e.names) >= maxRoutingNames {
			var oldest string
			for candidate, seen := range e.names {
				if oldest == "" || seen.Before(e.names[oldest]) {
					oldest = candidate
				}
			}
			delete(e.names, oldest)
		}
		e.names[name] = at
	}
	if text == "" || (!publicQuestion(text) && addressedPlayer(text, e.names) == "") {
		return
	}
	if len(e.prompts) == maxRoutingPrompts {
		copy(e.prompts, e.prompts[1:])
		e.prompts = e.prompts[:len(e.prompts)-1]
	}
	e.prompts = append(e.prompts, routingPrompt{actor: strings.ToLower(sender), display: routingName(display), at: at})
}

func publicQuestion(text string) bool {
	if strings.ContainsAny(text, "?？") {
		return true
	}
	words := strings.Fields(strings.ToLower(text))
	if len(words) < 2 {
		return false
	}
	switch words[0] {
	case "who", "what", "where", "when", "why", "how", "anyone":
		return true
	case "do", "are", "can", "will", "would":
		return words[1] == "you"
	}
	return false
}

func routingName(name string) string {
	return strings.ToLower(strings.Join(strings.Fields(name), " "))
}

// Require vocative syntax, not a name mentioned in ordinary prose. A bare
// "Bob is mining" is about Bob, whereas "Bob, ..." and "hey Bob ..." address him.
func addressedPlayer(text string, names map[string]time.Time) string {
	text = routingName(text)
	recipient, position := "", len(text)+1
	for name := range names {
		for _, prefix := range []string{"", "hey ", "hi ", "hello "} {
			rest, ok := strings.CutPrefix(text, prefix+name)
			if !ok {
				continue
			}
			if rest == "" && prefix != "" || strings.HasPrefix(rest, ":") || strings.HasPrefix(rest, ",") || prefix != "" && strings.HasPrefix(rest, " ") || strings.HasPrefix(rest, " are you ") || strings.HasPrefix(rest, " can you ") || strings.HasPrefix(rest, " do you ") {
				if position > 0 || len(name) > len(recipient) {
					recipient, position = name, 0
				}
			}
		}
		// Tags have explicit recipient syntax even at the end of a message.
		for offset := 0; offset < len(text); {
			i := strings.Index(text[offset:], "@"+name)
			if i < 0 {
				break
			}
			i += offset
			end := i + 1 + len(name)
			before, _ := utf8.DecodeLastRuneInString(text[:i])
			after, _ := utf8.DecodeRuneInString(text[end:])
			beforeOK := i == 0 || !routingWord(before)
			afterOK := end == len(text) || !routingWord(after)
			if beforeOK && afterOK {
				if i < position || i == position && len(name) > len(recipient) {
					recipient, position = name, i
				}
				break
			}
			offset = end
		}
	}
	return recipient
}

func routingWord(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'
}

func normalizedUtterance(text string) string {
	return strings.Trim(strings.ToLower(strings.Join(strings.Fields(text), " ")), ".!? ,;:。！？")
}

func endsConversation(text string) bool {
	switch normalizedUtterance(text) {
	case "thanks", "thank you", "ok thanks", "okay thanks", "no thanks", "bye", "goodbye", "never mind", "nevermind", "leave me alone", "stop talking", "that's all", "that is all":
		return true
	}
	return false
}
func ambiguousReply(text string) bool {
	switch normalizedUtterance(text) {
	case "yes", "yeah", "yep", "no", "nope", "ok", "okay", "sure", "maybe", "me", "i do", "i can":
		return true
	}
	return false
}

// This is deliberately not a topic classifier. A short reply or an explicit
// linguistic link gets a small grace period; arbitrary new sentences do not.
// The model should disengage after completed one-offs and wait for unfinished input.
func contextualFollowup(text string) bool {
	words := strings.Fields(normalizedUtterance(text))
	if len(words) == 0 {
		return false
	}
	if len(words) <= 2 {
		return true
	}
	switch words[0] {
	case "and", "but", "also", "actually", "who", "what", "where", "when", "why", "how":
		return true
	case "can", "could", "would", "will", "do", "are":
		if words[1] == "you" {
			return true
		}
	}
	for _, word := range words {
		switch strings.Trim(word, ",.;:!?") {
		case "it", "that", "those", "this", "these", "instead", "again":
			return true
		}
	}
	return false
}

func (e *conversationEngagement) competingPrompt(state *engagement, text string, at time.Time) bool {
	if !ambiguousReply(text) {
		return false
	}
	for _, prompt := range e.prompts {
		if prompt.at.After(at) || !prompt.at.After(state.lastInput) || at.Sub(prompt.at) > completedFollowupWindow {
			continue
		}
		if prompt.actor != strings.ToLower(state.request.sender) || isDiscordActor(state.request.sender) && prompt.display != state.focus {
			return true
		}
	}
	return false
}
