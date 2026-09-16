package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/go-mclib/client/modules/interaction"
	"github.com/go-mclib/client/modules/inventory"
	"github.com/go-mclib/data/items"
	jp "github.com/go-mclib/protocol/java_protocol"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

const itemUseAckWait = 5 * time.Second
const itemUseReleaseWait = 2 * time.Second

type itemUsePlan struct {
	// stopCurrent may request cleanup before acquiring the action-owner barrier.
	// Serialize that request with the single begin submission, not with network IO.
	mu                                             sync.Mutex
	hand                                           int
	duration                                       time.Duration
	yaw, pitch                                     *float64
	confirmed                                      bool
	started, stopped, released, natural, seenUsing bool
	sequence                                       int32
	slot                                           int
	before                                         *items.ItemStack
	startedAt, releasedAt                          time.Time
	releaseSend                                    func(jp.Packet) error
	releaseDone                                    chan struct{}
	releaseErr                                     error
}

func (m *actionManager) useItemTool() toolSpec {
	return makeTool("use_item", "Hold or release the selected real item: action=hold (default), hand=0 main or 1 offhand, seconds=0.05..120 (default 3), optional yaw/pitch in degrees. Begins once, holds until duration or observed natural completion, then releases; holding a bow and releasing can fire, and food/drink can finish naturally. Requires actual items/ammunition and server acceptance; packet acknowledgement alone does not prove consumption or firing. action=release releases your current held-use job (administrators may release another owner's); stop_action is the public emergency stop. Inspect action_status and inventory for observations.", map[string]any{
		"action":  map[string]any{"type": "string", "enum": []string{"hold", "release"}},
		"hand":    map[string]any{"type": "integer", "enum": []int{0, 1}},
		"seconds": map[string]any{"type": "number", "minimum": 0.05, "maximum": 120},
		"yaw":     map[string]any{"type": "number"},
		"pitch":   map[string]any{"type": "number", "minimum": -90, "maximum": 90},
	}, nil, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a struct {
			Action  string   `json:"action"`
			Hand    *int     `json:"hand"`
			Seconds *float64 `json:"seconds"`
			Yaw     *float64 `json:"yaw"`
			Pitch   *float64 `json:"pitch"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if a.Hand != nil && (*a.Hand < 0 || *a.Hand > 1) {
			return nil, fmt.Errorf("hand must be 0 or 1")
		}
		if a.Action == "release" {
			if a.Seconds != nil || a.Yaw != nil || a.Pitch != nil {
				return nil, fmt.Errorf("release does not accept seconds or aim")
			}
			return m.releaseItemTool(ctx, actor, a.Hand)
		}
		if a.Action != "" && a.Action != "hold" {
			return nil, fmt.Errorf("action must be hold or release")
		}
		seconds := 3.0
		if a.Seconds != nil {
			seconds = *a.Seconds
		}
		if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0.05 || seconds > 120 {
			return nil, fmt.Errorf("seconds must be 0.05..120")
		}
		if a.Yaw != nil {
			if math.IsNaN(*a.Yaw) || math.IsInf(*a.Yaw, 0) {
				return nil, fmt.Errorf("yaw must be finite")
			}
			*a.Yaw = math.Mod(*a.Yaw, 360)
		}
		if a.Pitch != nil && (math.IsNaN(*a.Pitch) || math.IsInf(*a.Pitch, 0) || *a.Pitch < -90 || *a.Pitch > 90) {
			return nil, fmt.Errorf("pitch must be finite and within -90..90")
		}
		p := &itemUsePlan{duration: time.Duration(seconds * float64(time.Second)), yaw: a.Yaw, pitch: a.Pitch}
		if a.Hand != nil {
			p.hand = *a.Hand
		}
		// The job deadline includes bounded acknowledgement/cleanup time; it is
		// not the requested hold duration and must not cancel the release packet.
		return m.submit(ctx, actor, "use_item", &actionJob{seconds: int(math.Ceil(seconds)) + 8, use: p})
	})
}

func (m *actionManager) releaseItemTool(ctx context.Context, actor string, hand *int) (any, error) {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.execMu.Lock()
	defer m.execMu.Unlock()
	m.mu.Lock()
	j, s := m.current, m.session
	m.mu.Unlock()
	if j == nil || j.use == nil {
		return nil, fmt.Errorf("no held-item use job is active")
	}
	if !strings.EqualFold(j.status.Owner, actor) && !m.admin(actor) {
		return nil, fmt.Errorf("action %d belongs to %s; use stop_action for an emergency stop", j.status.ID, j.status.Owner)
	}
	if j.session != s || j.ctx.Err() != nil || s.ctx.Err() != nil {
		return nil, fmt.Errorf("held-item use session ended")
	}
	p := j.use
	p.mu.Lock()
	if hand != nil && *hand != p.hand {
		p.mu.Unlock()
		return nil, fmt.Errorf("active item use belongs to hand %d", p.hand)
	}
	p.stopped = true
	err := p.releaseLocked()
	started := p.started
	p.mu.Unlock()
	if err != nil {
		m.finish(j, "failed", err.Error())
		return nil, err
	}
	if !started {
		m.finish(j, "cancelled", "released before item use began")
		return j.status, nil
	}
	j.status.Detail = "release queued once; waiting for observed server use state, not assuming firing or consumption"
	snapshot := j.status
	m.mu.Lock()
	if m.current == j {
		m.published = &snapshot
	}
	m.mu.Unlock()
	return snapshot, nil
}

func (j *actionJob) useItemTick() (bool, error) {
	p, s := j.use, j.session
	if p == nil {
		return false, fmt.Errorf("item-use plan missing")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started {
		if p.stopped || j.ctx.Err() != nil {
			return false, context.Canceled
		}
		if s.inventory == nil || s.inventory.ContainerOpen() {
			return false, fmt.Errorf("item use requires inventory and a closed container")
		}
		s.mu.Lock()
		known := s.inventoryKnown
		s.mu.Unlock()
		if !known {
			return false, fmt.Errorf("wait for authoritative inventory before using an item")
		}
		if s.me.UsingItem() {
			return false, fmt.Errorf("server already reports item use; stop the existing use first")
		}
		p.slot = inventory.SlotHotbarStart + s.inventory.HeldSlotIndex()
		if p.hand == 1 {
			p.slot = inventory.SlotOffhand
		}
		p.before = s.inventory.GetSlot(p.slot)
		if p.before.IsEmpty() {
			return false, fmt.Errorf("selected hand is empty")
		}
		yaw, pitch := s.me.Rotation()
		y, x := float64(yaw), float64(pitch)
		if p.yaw != nil {
			y = *p.yaw
		}
		if p.pitch != nil {
			x = *p.pitch
		}
		if math.IsNaN(y) || math.IsInf(y, 0) || math.IsNaN(x) || math.IsInf(x, 0) || x < -90 || x > 90 {
			return false, fmt.Errorf("item use requires finite yaw and pitch within -90..90")
		}
		s.stopMotion()
		s.me.SetRotation(math.Mod(y, 360), x)
		// Capture the session context, not the job context which finish cancels.
		p.releaseSend = func(pkt jp.Packet) error { return s.physics.QueuePacketContext(s.ctx, pkt) }
		submission, err := interaction.BeginItemUse(s.client, p.hand, j.send)
		if err != nil {
			return false, err
		}
		p.sequence, p.started, p.startedAt = submission.Sequence, true, time.Now()
		j.status.Detail = "item use queued once; waiting for server use state"
		return false, nil
	}
	using := s.me.UsingItem()
	if using && s.me.UsingOffhand() != (p.hand == 1) {
		return false, fmt.Errorf("server active item hand changed")
	}
	if p.hand == 0 && inventory.SlotHotbarStart+s.inventory.HeldSlotIndex() != p.slot {
		return false, fmt.Errorf("selected hotbar slot changed during item use")
	}
	if using {
		p.seenUsing = true
	}
	current := s.inventory.GetSlot(p.slot)
	changed := itemUseStackChanged(p.before, current)
	s.mu.Lock()
	ack := s.blockAck >= p.sequence
	s.mu.Unlock()
	if !using && (p.seenUsing || changed && ack) {
		p.confirmed = true
		// If metadata actually transitioned, no active use remains to release.
		// Inventory evidence without metadata still needs the safety release.
		p.natural = p.seenUsing && !p.released
		j.status.Detail = "server ended the observed item-use state; firing or consumption is not inferred"
		if changed {
			j.status.Detail = "server held-item stack changed after use; inspect inventory for the actual result"
		}
		return true, nil
	}
	if p.released {
		if p.releaseErr != nil {
			return false, p.releaseErr
		}
		if time.Since(p.releasedAt) >= itemUseReleaseWait {
			if using {
				return false, fmt.Errorf("release queued but server still reports active item use")
			}
			j.status.Detail = "use and release submitted once; no server use transition or held-stack change confirmed an effect"
			if ack {
				j.status.Detail += "; server acknowledged the request, not its success"
			}
			return true, nil
		}
		return false, nil
	}
	if !p.seenUsing && time.Since(p.startedAt) >= itemUseAckWait {
		return false, fmt.Errorf("server did not confirm item use; item may be unusable, on cooldown, or missing ammunition")
	}
	if time.Since(p.startedAt) >= p.duration {
		if err := p.releaseLocked(); err != nil {
			return false, err
		}
		j.status.Detail = "hold duration elapsed; release queued once, awaiting server state"
	} else if using {
		j.status.Detail = "server reports active item use; holding without repeated right clicks"
	}
	return false, nil
}

func itemUseStackChanged(before, after *items.ItemStack) bool {
	if before == after {
		return false
	}
	if before.IsEmpty() || after.IsEmpty() {
		return before.IsEmpty() != after.IsEmpty()
	}
	return before.ID != after.ID || before.Count != after.Count || !reflect.DeepEqual(before.Components, after.Components)
}

// releaseItemUse is an idempotent, nonblocking cleanup barrier. Call before job
// cancellation, including the early cancellation in stopCurrent. Holding p.mu
// prevents a tick from beginning use after an emergency stop requested cleanup.
func (j *actionJob) releaseItemUse() {
	if j.use == nil {
		return
	}
	p := j.use
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopped = true
	_ = p.releaseLocked()
}

func (p *itemUsePlan) releaseLocked() error {
	if !p.started || p.natural || p.released {
		return p.releaseErr
	}
	p.released, p.releasedAt = true, time.Now()
	p.releaseDone = make(chan struct{})
	p.releaseErr = interaction.ReleaseItemUse(func(pkt jp.Packet) error {
		return p.releaseSend(&itemUseReleasePacket{Packet: pkt, serialized: p.releaseDone})
	})
	return p.releaseErr
}

// Signalling from Write puts subsequent direct inventory writes behind this
// packet in TCPClient's write mutex. It is an ordering barrier, NOT a server ack.
// Session guards wrap this packet, so an expired session cannot signal success.
type itemUseReleasePacket struct {
	jp.Packet
	serialized chan struct{}
	once       sync.Once
}

func (p *itemUseReleasePacket) Write(buf *ns.PacketBuffer) error {
	if err := p.Packet.Write(buf); err != nil {
		return err
	}
	p.once.Do(func() { close(p.serialized) })
	return nil
}

// waitItemUseRelease is only for external mutation callers, after releasing
// execMu and before direct inventory writes. Never call it in a physics callback.
func (j *actionJob) waitItemUseRelease(ctx context.Context) error {
	if j == nil || j.use == nil {
		return nil
	}
	p := j.use
	p.mu.Lock()
	done, err := p.releaseDone, p.releaseErr
	p.mu.Unlock()
	if err != nil || done == nil {
		return err
	}
	timer := time.NewTimer(itemUseReleaseWait)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-j.session.ctx.Done():
		return fmt.Errorf("item-use session ended before release was transmitted")
	case <-timer.C:
		return fmt.Errorf("item-use release has not reached transport; inventory mutation refused")
	}
}
