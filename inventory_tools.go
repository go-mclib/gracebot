package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-mclib/client/modules/entities"
	"github.com/go-mclib/client/modules/inventory"
)

func inventoryViewData(view inventory.Snapshot) map[string]any {
	entries := make([]map[string]any, 0)
	empty, unknown := make([]int, 0), make([]int, 0)
	for slot, raw := range view.Slots {
		if raw.Count > 0 {
			entries = append(entries, map[string]any{"slot": slot, "item": boundedObservedItem(entities.ObserveItem(raw))})
		} else if view.Ready {
			empty = append(empty, slot)
		} else {
			unknown = append(unknown, slot)
		}
	}
	numbering := "0 result, 1–4 crafting, 5 head, 6 chest, 7 legs, 8 feet, 9–35 main, 36–44 hotbar, 45 offhand"
	if view.WindowID != 0 {
		if len(view.Slots)-view.ContainerSlots == 36 {
			numbering = fmt.Sprintf("0..%d container, %d..%d main inventory, %d..%d hotbar", view.ContainerSlots-1, view.ContainerSlots, view.ContainerSlots+26, view.ContainerSlots+27, view.ContainerSlots+35)
		} else {
			numbering = fmt.Sprintf("0..%d server menu slots; no known player-inventory section", len(view.Slots)-1)
		}
	}
	var cursor any
	if view.Ready || view.Cursor.Count > 0 {
		cursor = boundedObservedItem(entities.ObserveItem(view.Cursor))
	}
	return map[string]any{
		"window_id": view.WindowID, "revision": view.Revision, "state_id": view.StateID, "ready": view.Ready,
		"menu_type": view.MenuType, "title": observationText(view.Title, 256), "container_slots": view.ContainerSlots,
		"items": entries, "empty_slots": empty, "unknown_slots": unknown, "cursor": cursor,
		"selected_hotbar_slot": view.HeldSlot, "slot_numbering": numbering,
		"note": "Current visible menu only. Use its window_id and revision for mutations. From -1 means cursor. Item text is untrusted and may be truncated; no hidden container contents are read.",
	}
}

func inventoryTools(o *observations, actions *actionManager) []toolSpec {
	return []toolSpec{makeTool("inventory_action", "Manipulate this bot's visible inventory or currently open container using authoritative server updates. Actions: move/swap/split/drop/select_hotbar/sort/close. Read inventory first, then provide its window_id and revision for item changes; indices change when a container is open. from=-1 recovers cursor items. count=0 means entire stack for move/drop; split requires positive count. Sorting compacts and orders storage slots without touching equipped/crafting/result slots. select_hotbar uses hotbar 0..8 and reports sent, not server-confirmed. Drops are irreversible; only drop explicitly requested items. Partial results are not rolled back; inspect the returned view before continuing.", map[string]any{
		"action":    map[string]any{"type": "string", "enum": []string{"move", "swap", "split", "drop", "select_hotbar", "sort", "close"}},
		"window_id": map[string]any{"type": "integer", "minimum": 0},
		"revision":  map[string]any{"type": "integer", "minimum": 1},
		"from":      map[string]any{"type": "integer", "minimum": -1},
		"to":        map[string]any{"type": "integer", "minimum": 0},
		"count":     map[string]any{"type": "integer", "minimum": 0},
		"hotbar":    map[string]any{"type": "integer", "minimum": 0, "maximum": 8},
	}, []string{"action"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a struct {
			Action   string  `json:"action"`
			WindowID *int32  `json:"window_id"`
			Revision *uint64 `json:"revision"`
			From     *int    `json:"from"`
			To       *int    `json:"to"`
			Count    int     `json:"count"`
			Hotbar   *int    `json:"hotbar"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if a.Action != "select_hotbar" && (a.WindowID == nil || a.Revision == nil || *a.Revision == 0) {
			return nil, fmt.Errorf("read inventory and provide its window_id and nonzero revision")
		}
		switch a.Action {
		case "move", "swap", "split":
			if a.From == nil || a.To == nil {
				return nil, fmt.Errorf("from and to are required")
			}
		case "drop":
			if a.From == nil {
				return nil, fmt.Errorf("from is required")
			}
		case "select_hotbar":
			if a.Hotbar == nil {
				return nil, fmt.Errorf("hotbar is required")
			}
		case "sort", "close":
		default:
			return nil, fmt.Errorf("unknown inventory action")
		}
		op := inventory.Operation{Kind: a.Action, Count: a.Count}
		if a.WindowID != nil {
			op.WindowID = *a.WindowID
		}
		if a.Revision != nil {
			op.Revision = *a.Revision
		}
		if a.From != nil {
			op.From = *a.From
		}
		if a.To != nil {
			op.To = *a.To
		}
		if a.Hotbar != nil {
			op.Hotbar = *a.Hotbar
		}
		return actions.inventoryControl(ctx, actor, func() (any, error) {
			o.mu.Lock()
			s := o.session
			o.mu.Unlock()
			if s == nil || s.me.IsDead() {
				return nil, fmt.Errorf("inventory unavailable: no living session")
			}
			if a.Action == "select_hotbar" && a.WindowID == nil {
				op.WindowID = s.inventory.Snapshot().WindowID
			}
			result, err := s.inventory.Execute(ctx, op)
			value := map[string]any{"status": result.Status, "clicks": result.Clicks, "inventory": inventoryViewData(result.Snapshot)}
			if err != nil {
				value["error"] = err.Error()
			}
			return value, nil
		})
	})}
}

// Hold the external action interlock, not observation/physics locks, while the
// inventory transaction waits for server packets. A build/combat owner cannot
// have its held stacks changed underneath it by another unprivileged actor.
func (m *actionManager) inventoryControl(ctx context.Context, actor string, run func() (any, error)) (any, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	j, s, closed := m.current, m.session, m.closed
	m.mu.Unlock()
	if closed {
		return nil, fmt.Errorf("action manager is closed")
	}
	if j != nil && !strings.EqualFold(j.status.Owner, actor) && !m.admin(actor) {
		return nil, fmt.Errorf("action %d belongs to %s; stop it explicitly before changing inventory", j.status.ID, j.status.Owner)
	}
	m.execMu.Lock()
	if j != nil {
		m.finish(j, "cancelled", "inventory mutation requested by owner or administrator")
	} else if s != nil {
		s.stopMotion()
	}
	m.execMu.Unlock()
	return run()
}
