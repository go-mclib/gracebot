package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func TestHistoricalPackingKeepsReceiptsAndCurrentResults(t *testing.T) {
	const receipt = `{"transferred":7,"recipient":"Bob","remaining":3}`
	observation := strings.Repeat("observed block ", 16000) + "CURRENT_RESULT_END"
	tools := newToolRegistry([]toolSpec{
		makeTool("observe", "read state", nil, nil, func(context.Context, string, json.RawMessage) (any, error) { return observation, nil }),
		makeTool("transfer", "transfer items", nil, nil, func(context.Context, string, json.RawMessage) (any, error) { return json.RawMessage(receipt), nil }),
	})
	calls := 0
	var prior []openai.ChatCompletionMessage
	api := completionFunc(func(_ context.Context, req completionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		available := make(map[string]bool)
		for _, tool := range req.Tools {
			if tool.Function != nil {
				available[tool.Function.Name] = true
			}
		}
		if !available["observe"] || !available["transfer"] || !available["respond"] {
			t.Fatal("context packing removed a required tool capability")
		}
		switch calls {
		case 1:
			return completionMessage(openai.ChatCompletionMessage{ToolCalls: []openai.ToolCall{
				{ID: "observation", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "observe", Arguments: `{}`}},
				{ID: "effect", Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "transfer", Arguments: `{"recipient":"Bob","count":7}`}},
			}}), nil
		case 2:
			if !strings.Contains(req.Messages[len(req.Messages)-2].Content, "CURRENT_RESULT_END") || req.Messages[len(req.Messages)-1].Content != receipt {
				t.Fatal("current decision lost observation data or action outcome")
			}
			answer := responseCompletion("continue", "sent seven to bob; anything else?")
			prior = append([]openai.ChatCompletionMessage(nil), req.Messages[2:]...)
			prior = append(prior, answer.Choices[0].Message, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "reply", Content: `{"status":"accepted"}`})
			return answer, nil
		case 3:
			data, _ := json.Marshal(req)
			text := string(data)
			if strings.Contains(text, "CURRENT_RESULT_END") || !strings.Contains(text, "Details omitted") || !strings.Contains(text, `\"transferred\":7`) || !strings.Contains(text, "FOLLOWUP_CURRENT") {
				t.Fatal("packed history lost factual receipt/current input or retained oversized observation")
			}
			// Compare identical serialized request fixtures, differing only in
			// historical packing. Bytes are not provider token counts.
			legacy := req
			legacy.Messages = append(append(append([]openai.ChatCompletionMessage(nil), req.Messages[:2]...), prior...), req.Messages[len(req.Messages)-1])
			before, _ := json.Marshal(legacy)
			if len(before)-len(data) < len(observation)-8192 {
				t.Fatalf("oversized historical observation remained: %d vs %d bytes", len(data), len(before))
			}
			t.Logf("serialized request fixture: unpacked=%d bytes, packed=%d bytes; not token counts", len(before), len(data))
			return responseCompletion("disengage", "all done"), nil
		default:
			t.Fatal("unexpected model call")
			return openai.ChatCompletionResponse{}, nil
		}
	})
	a := newAssistant(api, "model", "", tools, nil)
	r := fragmentRequest(context.Background(), "Alice", "grace transfer seven", time.Now(), true, 1)
	r.conversationID = 10
	if _, err := a.respond(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.messages = []chatFragment{{ID: 2, At: time.Now(), Text: "FOLLOWUP_CURRENT"}}
	if _, err := a.respond(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestBackgroundDeduplicatesTranscriptAndKeepsProvenance(t *testing.T) {
	at := time.Now().UTC()
	input, _ := json.Marshal(map[string]any{"messages": []chatFragment{{ID: 2, At: at, Text: "REQUEST_ONCE"}}})
	turn := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: string(input)}, responseCompletion("continue", "BOT_ONCE").Choices[0].Message}
	receipt, _ := json.Marshal(map[string]any{"messages_submitted": 1, "sent_between": [2]time.Time{at.Add(-time.Second), at.Add(time.Second)}})
	turn = append(turn, openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: "reply", Content: string(receipt)})
	page, _ := json.Marshal(map[string]any{"type": "chat_history_data", "history": map[string]any{"entries": []chatEntry{
		{ID: 6, At: at, Kind: "private", Owner: "alice", Text: "PRIVATE_NEVER"},
		{ID: 5, At: at, Kind: "discord", Sender: discordBridgeActor, DisplayName: "RealAdmin", Text: "NEW_OBSERVATION"},
		{ID: 4, At: at, Kind: "bot", Sender: "GraceLocal", Text: "BOT_ONCE"},
		{ID: 2, At: at, Kind: "player", Sender: "Alice", Text: "REQUEST_ONCE"},
		{ID: 1, At: at, Kind: "player", Sender: "Bob", Text: "IDLE_OBSERVATION"},
	}}})
	got := backgroundContext(string(page), [][]openai.ChatCompletionMessage{turn})
	for _, absent := range []string{"PRIVATE_NEVER", "REQUEST_ONCE", "BOT_ONCE"} {
		if strings.Contains(got, absent) {
			t.Fatalf("private or duplicate text retained: %s", absent)
		}
	}
	if !strings.Contains(got, `"#discord_bridge","RealAdmin"`) || !strings.Contains(got, "NEW_OBSERVATION") || strings.Index(got, "IDLE_OBSERVATION") > strings.Index(got, "NEW_OBSERVATION") {
		t.Fatalf("lost public context, chronology or bridge provenance: %s", got)
	}
	first := backgroundContext(string(page), nil)
	if !strings.Contains(first, "REQUEST_ONCE") || !strings.Contains(first, "BOT_ONCE") {
		t.Fatal("first engagement lost idle transcript")
	}
	unsent := backgroundContext(string(page), [][]openai.ChatCompletionMessage{turn[:2]})
	if !strings.Contains(unsent, "BOT_ONCE") {
		t.Fatal("an unsubmitted reply erased an unrelated observed bot message")
	}
}

func TestAutomaticContextHasByteAndTurnBounds(t *testing.T) {
	conv := &conversation{}
	for i := range maxHistoryTurns + 4 {
		var turn []openai.ChatCompletionMessage
		for j := range 16 {
			id := fmt.Sprintf("%d-%d", i, j)
			turn = append(turn,
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{ID: id, Type: openai.ToolTypeFunction, Function: openai.FunctionCall{Name: "inspect", Arguments: `{}`}}}},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: id, Content: strings.Repeat("world observation ", 1000)},
			)
		}
		conv.turns = append(conv.turns, turn)
	}
	packConversation(conv)
	size := 0
	for _, turn := range conv.turns {
		size += turnContentBytes(turn)
	}
	if size > maxHistoryBytes || len(conv.turns) >= maxHistoryTurns || !conv.omittedHistory {
		t.Fatalf("content budget not enforced independently of turn budget: %d bytes, %d turns", size, len(conv.turns))
	}
	before, _ := json.Marshal(conv.turns)
	packConversation(conv)
	after, _ := json.Marshal(conv.turns)
	if string(before) != string(after) {
		t.Fatal("repacking changed retained factual context")
	}
	var entries []chatEntry
	for i := range 24 {
		entries = append(entries, chatEntry{ID: uint64(i + 1), At: time.Now(), Kind: "player", Sender: "Bob", Text: strings.Repeat("x", 2000)})
	}
	page, _ := json.Marshal(map[string]any{"history": map[string]any{"entries": entries}})
	background := backgroundContext(string(page), nil)
	if len(background) > maxBackgroundBytes+512 || !strings.Contains(background, "Older background omitted") {
		t.Fatal("automatic background is not bounded with explicit retrieval guidance")
	}
}

func TestNewEngagementDiscardsStaleTurnContext(t *testing.T) {
	calls := 0
	a := newAssistant(completionFunc(func(_ context.Context, req completionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		data, _ := json.Marshal(req.Messages)
		if calls == 2 && !strings.Contains(string(data), "OLD_ENGAGEMENT_DETAIL") {
			t.Fatal("ongoing engagement lost genuine conversational context")
		}
		if calls == 3 && strings.Contains(string(data), "OLD_ENGAGEMENT_DETAIL") {
			t.Fatal("new engagement inherited stale instructions")
		}
		return responseCompletion("continue", "what next?"), nil
	}), "model", "", nil, nil)
	r := fragmentRequest(context.Background(), "Alice", "OLD_ENGAGEMENT_DETAIL", time.Now(), true, 1)
	r.conversationID = 1
	if _, err := a.respond(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.messages = []chatFragment{{ID: 2, At: time.Now(), Text: "and next?"}}
	if _, err := a.respond(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	r.conversationID = 2
	r.messages = []chatFragment{{ID: 3, At: time.Now(), Text: "grace new subject"}}
	if _, err := a.respond(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestFailedCompletionDoesNotKeepAmbientEngagement(t *testing.T) {
	a := newAssistant(completionFunc(func(context.Context, completionRequest) (openai.ChatCompletionResponse, error) {
		return openai.ChatCompletionResponse{}, fmt.Errorf("provider unavailable")
	}), "model", "", nil, nil)
	defer a.engagements.reset()
	at := time.Now()
	a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "grace hello", at, true, 1))
	r, _, ready := a.engagements.take(at.Add(3 * time.Second))
	if !ready {
		t.Fatal("initial mention not ready")
	}
	a.handleRequest(context.Background(), r)
	if a.engagements.enqueue(fragmentRequest(context.Background(), "Alice", "yes", at.Add(4*time.Second), false, 2)) {
		t.Fatal("failed one-off request kept ambient conversation open")
	}
}

func TestCachePrefixSurvivesFreshInputAndBackground(t *testing.T) {
	journal := testPlayerStore(t).history()
	var prefix string
	var prefixEnd int
	var eventID uint64
	calls := 0
	a := newAssistant(completionFunc(func(_ context.Context, req completionRequest) (openai.ChatCompletionResponse, error) {
		calls++
		if calls > 1 {
			if req.historyEnd <= 2 {
				t.Fatal("completed conversation has no reusable history prefix")
			}
			data, _ := json.Marshal(req.Messages[:req.historyEnd])
			if strings.Contains(string(data), fmt.Sprintf("BACKGROUND_%d", calls)) {
				t.Fatal("fresh background invalidates the completed-history cache")
			}
			if calls == 2 {
				prefix, prefixEnd = string(data), req.historyEnd
			} else {
				data, _ = json.Marshal(req.Messages[:prefixEnd])
				if string(data) != prefix {
					t.Fatal("fresh request changed an already cacheable history prefix")
				}
			}
		}
		var input struct {
			EventID uint64 `json:"current_request_event_id"`
			At      string `json:"current_time_utc"`
		}
		if err := json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &input); err != nil || input.EventID != eventID {
			t.Fatalf("current request lost its runtime event: %+v, %v", input, err)
		}
		if _, err := time.Parse(time.RFC3339, input.At); err != nil {
			t.Fatalf("current request lost its runtime clock: %v", err)
		}
		return responseCompletion("continue", "what next?"), nil
	}), "model", "", nil, nil)
	a.journal = journal
	for i := 1; i <= 3; i++ {
		at := time.Now()
		if _, err := journal.append("player", "Bob", fmt.Sprintf("BACKGROUND_%d", i), at); err != nil {
			t.Fatal(err)
		}
		var err error
		eventID, err = journal.append("player", "Alice", fmt.Sprintf("REQUEST_%d", i), at)
		if err != nil {
			t.Fatal(err)
		}
		r := fragmentRequest(context.Background(), "Alice", fmt.Sprintf("REQUEST_%d", i), at, true, eventID)
		r.conversationID = 1
		if _, err := a.respond(context.Background(), r); err != nil {
			t.Fatal(err)
		}
	}
}
