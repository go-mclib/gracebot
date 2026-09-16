package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/sashabaranov/go-openai"
)

type toolSpec struct {
	name, description string
	parameters        map[string]any
	run               func(context.Context, string, json.RawMessage) (any, error)
	endsTurn          bool // Sends its own deterministic reply; no model narration afterward.
}

func makeTool(name, description string, properties map[string]any, required []string, run func(context.Context, string, json.RawMessage) (any, error)) toolSpec {
	if properties == nil {
		properties = map[string]any{}
	}
	if required == nil {
		required = []string{}
	}
	return toolSpec{name: name, description: description, parameters: map[string]any{
		"type": "object", "properties": properties, "required": required, "additionalProperties": false,
	}, run: run}
}

func decodeArgs(raw json.RawMessage, target any) error {
	if len(raw) > 16*1024 {
		return fmt.Errorf("tool arguments are too large")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		return fmt.Errorf("tool arguments must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid tool arguments: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("tool arguments must contain exactly one JSON object")
	}
	return nil
}

type toolRegistry struct {
	definitions []openai.Tool
	byName      map[string]toolSpec
	blocked     func(string) bool
}

func newToolRegistry(groups ...[]toolSpec) *toolRegistry {
	r := &toolRegistry{byName: make(map[string]toolSpec)}
	for _, group := range groups {
		for _, spec := range group {
			if _, exists := r.byName[spec.name]; exists {
				panic("duplicate tool: " + spec.name)
			}
			r.byName[spec.name] = spec
			r.definitions = append(r.definitions, openai.Tool{
				Type:     openai.ToolTypeFunction,
				Function: &openai.FunctionDefinition{Name: spec.name, Description: spec.description, Parameters: spec.parameters},
			})
		}
	}
	return r
}

// The actor comes from the player packet, never from model-supplied arguments.
func (r *toolRegistry) execute(ctx context.Context, actor string, call openai.ToolCall) string {
	var value any
	var err error
	spec, ok := r.byName[call.Function.Name]
	switch {
	case ctx.Err() != nil:
		err = ctx.Err()
	case !ok || call.Type != openai.ToolTypeFunction:
		err = fmt.Errorf("unknown tool %q", call.Function.Name)
	case strings.TrimSpace(actor) == "":
		err = fmt.Errorf("a player identity is required")
	case r.blocked != nil && r.blocked(actor):
		err = fmt.Errorf("speaker is blocked")
	default:
		value, err = spec.run(ctx, actor, json.RawMessage(call.Function.Arguments))
	}
	if err != nil {
		value = map[string]string{"error": err.Error()}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return `{"error":"could not encode tool result"}`
	}
	return string(encoded)
}
