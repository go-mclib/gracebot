package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-mclib/client"
	"github.com/go-mclib/client/modules/collisions"
	"github.com/go-mclib/client/modules/combat"
	"github.com/go-mclib/client/modules/entities"
	"github.com/go-mclib/client/modules/pathfinding"
	"github.com/go-mclib/client/modules/physics"
	"github.com/go-mclib/client/modules/self"
	"github.com/go-mclib/client/modules/world"
	"github.com/go-mclib/data/blocks"
	"github.com/go-mclib/data/items"
	"github.com/go-mclib/data/packets"
	jp "github.com/go-mclib/protocol/java_protocol"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

func inertActionSession() *actionSession {
	ctx, cancel := context.WithCancel(context.Background())
	return &actionSession{ctx: ctx, cancel: cancel, me: self.New(), physics: physics.New(), path: pathfinding.New(), combat: combat.New(), world: world.New(), entities: entities.New()}
}

func activeActionSession(t *testing.T) *actionSession {
	t.Helper()
	s := inertActionSession()
	c := client.New("unused", "Grace", false)
	c.SetState(jp.StatePlay)
	s.client = c
	col := collisions.New()
	for _, module := range []client.Module{s.me, s.world, col, s.physics} {
		c.Register(module)
	}
	if err := col.Wire(); err != nil {
		t.Fatal(err)
	}
	if err := s.physics.Wire(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.cancel(); s.physics.Reset() })
	s.me.HandlePacket(observationPacket(t, &packets.S2CLogin{EntityId: 1, DimensionName: "minecraft:overworld"}))
	s.me.HandlePacket(observationPacket(t, &packets.S2CPlayerPosition{X: 1, Y: 64, Z: 1}))
	s.positionKnown = true
	return s
}

func TestActionGameplayOpenButConstructionRequiresAdmin(t *testing.T) {
	store := commandStore(t)
	if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Bob","rank":"trusted","action":"add"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Root","rank":"admin","action":"add"}`)); err != nil {
		t.Fatal(err)
	}
	m := newActionManager(store)
	m.session = activeActionSession(t)
	defer m.stopCurrent(context.Background(), "test finished")
	for _, kind := range []string{"move", "follow", "fight", "mount", "dismount"} {
		value, err := m.submit(context.Background(), "Alice", kind, &actionJob{})
		if err != nil {
			t.Fatalf("unranked %s rejected: %v", kind, err)
		}
		if status := value.(actionStatus); status.Owner != "Alice" || status.Kind != kind || status.State != "queued" {
			t.Fatalf("action was not admitted: %+v", status)
		}
		m.stopCurrent(context.Background(), "test next action")
	}
	build := m.buildTool()
	args := json.RawMessage(`{"origin":[1,64,1],"size":[1,1,1],"block":"stone"}`)
	for _, actor := range []string{"Alice", "Bob", discordBridgeActor} {
		if _, err := build.run(context.Background(), actor, args); err == nil {
			t.Fatalf("non-admin %s started construction", actor)
		}
		if m.current != nil {
			t.Fatal("denied construction installed a job")
		}
	}
	value, err := build.run(context.Background(), "Root", args)
	if err != nil {
		t.Fatalf("admin construction rejected: %v", err)
	}
	if value.(actionStatus).Kind != "build" {
		t.Fatalf("admin construction did not start: %+v", value)
	}
	if _, err := store.setRankTool(context.Background(), consoleActor, json.RawMessage(`{"player":"Root","rank":"admin","action":"remove"}`)); err != nil {
		t.Fatal(err)
	}
	m.tick(m.session.client)
	if m.current != nil || m.history[len(m.history)-1].State != "cancelled" {
		t.Fatal("construction survived admin revocation")
	}
}

func TestActionStopIsCancellationAndMotionBarrier(t *testing.T) {
	s := inertActionSession()
	defer s.cancel()
	s.physics.SetInput(1, 0, true)
	ctx, cancel := context.WithCancel(s.ctx)
	j := &actionJob{ctx: ctx, cancel: cancel, session: s, status: actionStatus{ID: 1, Kind: "move", Owner: "Alice", State: "running"}}
	m := newActionManager(nil)
	m.session = s
	m.current = j
	m.execMu.Lock()
	stopped := make(chan struct{})
	go func() { m.stopCurrent(context.Background(), "emergency"); close(stopped) }()
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel in-flight planning")
	}
	select {
	case <-stopped:
		t.Fatal("stop returned before owner yielded")
	default:
	}
	m.execMu.Unlock()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("motion barrier did not finish")
	}
	forward, strafe, jump := s.physics.Input()
	if forward != 0 || strafe != 0 || jump {
		t.Fatal("stop retained movement input")
	}
	if m.current != nil || len(m.history) != 1 || m.history[0].State != "cancelled" {
		t.Fatal("cancelled job outcome lost")
	}
	if err := j.send(&packets.C2SPunch{}); err != context.Canceled {
		t.Fatalf("cancelled job submitted an effect: %v", err)
	}
}

func TestActionOwnerCannotBeReplacedByAnotherPlayer(t *testing.T) {
	store := commandStore(t)
	s := inertActionSession()
	defer s.cancel()
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	current := &actionJob{ctx: ctx, cancel: cancel, session: s, status: actionStatus{ID: 7, Owner: "Alice", State: "running"}}
	m := newActionManager(store)
	m.session = s
	m.current = current
	if _, err := m.submit(context.Background(), "Bob", "move", &actionJob{}); err == nil || !strings.Contains(err.Error(), "belongs to Alice") {
		t.Fatalf("another actor replaced owner: %v", err)
	}
	if ctx.Err() != nil || m.current != current {
		t.Fatal("unauthorized replacement cancelled existing action")
	}
}

func TestActionRejectsUnloadedAndOutOfRadiusTargets(t *testing.T) {
	s := inertActionSession()
	defer s.cancel()
	j := &actionJob{session: s, origin: actionPoint{0, 64, 0}, target: 42}
	if err := j.checkPoint(actionPoint{1, 64, 1}); err == nil || !strings.Contains(err.Error(), "unloaded") {
		t.Fatalf("unknown terrain became reachable: %v", err)
	}
	if err := j.checkPoint(actionPoint{33, 64, 0}); err == nil {
		t.Fatal("radius limit ignored")
	}
	if _, err := j.targetEntity(); err == nil || !strings.Contains(err.Error(), "unloaded or removed") {
		t.Fatalf("missing target was treated as a kill/known target: %v", err)
	}
}

func TestBuildPreflightMaterialsSupportAndOccupiedCells(t *testing.T) {
	plan, err := compileBuild(buildArgs{Origin: []int{0, 64, 0}, Size: []int{1, 2, 1}, Block: "stone"})
	if err != nil {
		t.Fatal(err)
	}
	air := blocks.DefaultStateID(blocks.BlockID("minecraft:air"))
	stone := blocks.DefaultStateID(blocks.BlockID("minecraft:stone"))
	cells := map[[3]int]int32{{0, 63, 0}: stone, {0, 64, 0}: air, {0, 65, 0}: air}
	lookup := func(pos [3]int) (int32, bool) { v, ok := cells[pos]; return v, ok }
	if err = preflightBuildData(plan.cells, lookup, func(int32) int { return 2 }); err != nil {
		t.Fatalf("planned lower layer should support upper layer: %v", err)
	}
	if err = preflightBuildData(plan.cells, lookup, func(int32) int { return 1 }); err == nil {
		t.Fatal("insufficient materials accepted")
	}
	cells[[3]int{0, 63, 0}] = air
	if err = preflightBuildData(plan.cells, lookup, func(int32) int { return 2 }); err == nil {
		t.Fatal("floating build accepted")
	}
	cells[[3]int{0, 63, 0}] = stone
	cells[[3]int{0, 64, 0}] = stone
	if err = preflightBuildData(plan.cells, lookup, func(int32) int { return 2 }); err == nil {
		t.Fatal("destructive replacement accepted")
	}
	delete(cells, [3]int{0, 64, 0})
	if err = preflightBuildData(plan.cells, lookup, func(int32) int { return 2 }); err == nil {
		t.Fatal("unloaded cell accepted")
	}
}

func TestBuildPaletteAndSupportedPlacementBoundaries(t *testing.T) {
	p, err := compileBuild(buildArgs{Origin: []int{1, 64, 2}, Palette: map[string]string{"S": "stone", "W": "white_wool"}, Layers: [][]string{{"SW", "S."}, {"S.", ".."}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.cells) != 4 || p.cells[3].Pos != [3]int{1, 65, 2} || p.cells[1].Item != items.ItemID("minecraft:white_wool") {
		t.Fatal("compact palette did not produce bottom-up arrangement")
	}
	for _, name := range []string{"oak_stairs", "oak_log", "sand", "chest", "white_glazed_terracotta", "tnt"} {
		if _, _, _, err := buildMaterial(name); err == nil {
			t.Fatalf("unsafe/unsupported material %s accepted", name)
		}
	}
	if _, err := compileBuild(buildArgs{Origin: []int{0, 0, 0}, Size: []int{8, 4, 8}, Block: "stone"}); err == nil {
		t.Fatal("oversized build accepted")
	}
}

func TestPlacementNeedsBothServerUpdates(t *testing.T) {
	const expected int32 = 1
	if placementConfirmed(expected, expected, 4, 4, true) {
		t.Fatal("predicted inventory accepted as server confirmation")
	}
	if placementConfirmed(expected, expected, 5, 4, false) {
		t.Fatal("block update without consumption accepted")
	}
	if placementConfirmed(0, expected, 5, 4, true) {
		t.Fatal("inventory update without expected block accepted")
	}
	if !placementConfirmed(expected, expected, 5, 4, true) {
		t.Fatal("server block and inventory confirmation rejected")
	}
}

func TestPassengerConfirmationUsesServerPackets(t *testing.T) {
	s := inertActionSession()
	defer s.cancel()
	// Own entity defaults to zero here; unrelated passenger packets must not mount it.
	wire, err := jp.ToWire(&packets.S2CSetPassengers{EntityId: 5, Passengers: nil})
	if err != nil {
		t.Fatal(err)
	}
	s.observe(wire)
	if s.vehicle != 0 || s.passengerVersion != 0 {
		t.Fatal("unrelated passenger packet manufactured mount state")
	}
	wire, err = jp.ToWire(&packets.S2CSetPassengers{EntityId: 5, Passengers: []ns.VarInt{0}})
	if err != nil {
		t.Fatal(err)
	}
	s.observe(wire)
	if s.vehicle != 5 || s.passengerVersion == 0 {
		t.Fatal("server mount confirmation was lost")
	}
	version := s.passengerVersion
	wire, err = jp.ToWire(&packets.S2CSetPassengers{EntityId: 5})
	if err != nil {
		t.Fatal(err)
	}
	s.observe(wire)
	if s.vehicle != 0 || s.passengerVersion <= version {
		t.Fatal("server dismount confirmation was lost")
	}
}
