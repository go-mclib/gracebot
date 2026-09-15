package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-mclib/client/modules/interaction"
)

type interactionPlan struct {
	block       [3]int
	hand        int
	sneak       bool
	sequence    int32
	beforeState int32
	confirmed   bool
	replans     int
}

func (m *actionManager) interactBlockTool() toolSpec {
	coord := func() map[string]any { return map[string]any{"type": "integer"} }
	return makeTool("interact_block", "Approach and right-click a loaded block within 32 blocks of the job start, using the selected main/offhand item. Can open chests/containers, doors, buttons and other usable blocks; held items may be placed or consumed. hand=0 main, 1 off; sneak=true requests secondary use instead of normal opening. Requests do not bypass server protections. Wait for action_status; opened containers and changed blocks are observed, otherwise the result remains submitted/unconfirmed. Read inventory after opening a container.", map[string]any{
		"x": coord(), "y": coord(), "z": coord(), "hand": map[string]any{"type": "integer", "enum": []int{0, 1}}, "sneak": map[string]any{"type": "boolean"}, "seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
	}, []string{"x", "y", "z"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a struct {
			X       *int `json:"x"`
			Y       *int `json:"y"`
			Z       *int `json:"z"`
			Hand    int  `json:"hand"`
			Sneak   bool `json:"sneak"`
			Seconds int  `json:"seconds"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if a.X == nil || a.Y == nil || a.Z == nil {
			return nil, fmt.Errorf("x,y,z are required")
		}
		if a.Hand < 0 || a.Hand > 1 {
			return nil, fmt.Errorf("hand must be 0 or 1")
		}
		if a.Seconds == 0 {
			a.Seconds = 20
		}
		return m.submit(ctx, actor, "interact_block", &actionJob{goal: actionPoint{float64(*a.X), float64(*a.Y), float64(*a.Z)}, seconds: a.Seconds, interact: &interactionPlan{block: [3]int{*a.X, *a.Y, *a.Z}, hand: a.Hand, sneak: a.Sneak}})
	})
}

func (m *actionManager) interactEntityTool() toolSpec {
	return makeTool("interact_entity", "Approach and right-click one loaded entity by entity_id or visible Minecraft player name, using main/offhand and optional secondary use. This is interaction, not attack; it may trade, feed, mount or open an entity's container depending on held item and server behavior. Target must stay loaded and within 32 blocks of the job start. Submitted is not proof of an effect; inspect action_status and inventory for a server-opened menu.", map[string]any{
		"entity_id": map[string]any{"type": "integer"}, "player": map[string]any{"type": "string", "maxLength": 16}, "hand": map[string]any{"type": "integer", "enum": []int{0, 1}}, "sneak": map[string]any{"type": "boolean"}, "seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
	}, nil, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a struct {
			EntityID *int32 `json:"entity_id"`
			Player   string `json:"player"`
			Hand     int    `json:"hand"`
			Sneak    bool   `json:"sneak"`
			Seconds  int    `json:"seconds"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if (a.EntityID == nil) == (a.Player == "") {
			return nil, fmt.Errorf("supply exactly one of entity_id or player")
		}
		if a.Player != "" && !validUsername(a.Player) {
			return nil, fmt.Errorf("player must be a Minecraft username")
		}
		if a.Hand < 0 || a.Hand > 1 {
			return nil, fmt.Errorf("hand must be 0 or 1")
		}
		if a.Seconds == 0 {
			a.Seconds = 20
		}
		j := &actionJob{player: a.Player, seconds: a.Seconds, interact: &interactionPlan{hand: a.Hand, sneak: a.Sneak}}
		if a.EntityID != nil {
			j.target = *a.EntityID
		}
		return m.submit(ctx, actor, "interact_entity", j)
	})
}

func (j *actionJob) interactTick() (bool, error) {
	p, s := j.interact, j.session
	if p == nil {
		return false, fmt.Errorf("interaction plan missing")
	}
	if j.sent {
		s.mu.Lock()
		window, ready, ack, vehicle := s.containerWindow, s.containerReady, s.blockAck, s.vehicle
		s.mu.Unlock()
		if window != 0 && ready && s.inventory.ContainerOpen() {
			p.confirmed = true
			j.status.Detail = fmt.Sprintf("server opened container window %d and supplied its contents; read inventory before moving items", window)
			return true, nil
		}
		if j.status.Kind == "interact_block" {
			state, loaded := s.world.GetLoadedBlock(p.block[0], p.block[1], p.block[2])
			if loaded && state != p.beforeState {
				p.confirmed = true
				j.status.Detail = "server block state changed after the interaction"
				return true, nil
			}
			if ack >= p.sequence && time.Since(j.sentAt) >= time.Second {
				j.status.Detail = "server acknowledged the block interaction; no container or block change was observed, so effect is unconfirmed"
				return true, nil
			}
		} else {
			if vehicle == j.target {
				p.confirmed = true
				j.status.Detail = "server confirmed mounting the interacted entity"
				return true, nil
			}
			if time.Since(j.sentAt) >= 2*time.Second {
				j.status.Detail = "entity interaction submitted once; no container or passenger confirmation was observed"
				return true, nil
			}
		}
		if time.Since(j.sentAt) > 5*time.Second {
			return false, fmt.Errorf("interaction was queued but no server acknowledgement or observable effect arrived")
		}
		return false, nil
	}
	if s.inventory.ContainerOpen() {
		return false, fmt.Errorf("close the current container before interacting with another target")
	}
	var target actionPoint
	if j.status.Kind == "interact_block" {
		target = j.goal
		if err := j.checkPoint(target); err != nil {
			return false, err
		}
	} else {
		e, err := j.targetEntity()
		if err != nil {
			return false, err
		}
		target = actionPoint{e.X, e.Y, e.Z}
	}
	if s.path.IsNavigating() {
		j.status.Detail = "approaching interaction target"
		return false, nil
	}
	if j.navStarted && s.navigationResult.Load() < 0 {
		return false, fmt.Errorf("interaction target is unreachable")
	}
	s.stopMotion()
	s.me.SetSneaking(p.sneak)
	var err error
	if j.status.Kind == "interact_block" {
		var submission interaction.Submission
		submission, err = interaction.UseBlock(s.client, p.block[0], p.block[1], p.block[2], p.hand, j.send)
		p.sequence, p.beforeState = submission.Sequence, submission.BlockState
	} else {
		err = interaction.UseEntity(s.client, j.target, p.hand, j.send)
	}
	if errors.Is(err, interaction.ErrOutOfReach) || errors.Is(err, interaction.ErrObstructed) {
		if p.replans >= 3 {
			return false, fmt.Errorf("no reachable visible interaction point after replanning")
		}
		p.replans++
		reach := 2.5
		if j.status.Kind == "interact_block" {
			reach = 3.5
		}
		goal, approachErr := j.approach(target, reach)
		if approachErr != nil {
			return false, approachErr
		}
		return false, j.navigate(goal)
	}
	if err != nil {
		return false, err
	}
	j.sent, j.sentAt = true, time.Now()
	j.status.Detail = "interaction queued once; waiting for server state"
	return false, nil
}
