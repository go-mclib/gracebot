package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/go-mclib/client"
	"github.com/go-mclib/client/modules/playerlist"
	"github.com/go-mclib/client/modules/self"
	"github.com/go-mclib/client/modules/world"
	"github.com/go-mclib/data/chunks"
	dataentities "github.com/go-mclib/data/entities"
	"github.com/go-mclib/data/items"
	"github.com/go-mclib/data/packets"
	jp "github.com/go-mclib/protocol/java_protocol"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

func observationPacket(t *testing.T, packet jp.Packet) *jp.WirePacket {
	t.Helper()
	wire, err := jp.ToWire(packet)
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func observationFixture(t *testing.T) (*observations, *client.Client) {
	t.Helper()
	c := client.New("unused", "Grace", false)
	c.SetState(jp.StatePlay)
	me, players, w := self.New(), playerlist.New(), world.New()
	c.Register(me)
	c.Register(players)
	c.Register(w)
	o := newObservations()
	o.register(c, me, players, w)
	c.FirePlay()
	me.SetPosition(1, 64, 1)
	o.session.positionKnown = true
	return o, c
}

func observationResultMap(t *testing.T, value any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestObservationLoadedRangeEquipmentAndReset(t *testing.T) {
	o, c := observationFixture(t)
	buf := ns.NewWriter()
	_ = buf.WriteByte(1 | 8)
	_ = buf.WriteVarInt(1)
	_ = buf.WriteUUID(ns.UUID{7})
	_ = buf.WriteString("Steve")
	_ = buf.WriteVarInt(0)
	_ = buf.WriteBool(true)
	o.session.players.HandlePacket(&jp.WirePacket{PacketID: (&packets.S2CPlayerInfoUpdate{}).ID(), Data: buf.Bytes()})
	o.session.entities.HandlePacket(observationPacket(t, &packets.S2CAddEntity{EntityId: 42, EntityUuid: ns.UUID{7}, Type: ns.VarInt(dataentities.EntityTypeID("minecraft:player")), X: 2, Y: 64, Z: 2}))
	value, err := o.playerPositionTool(context.Background(), "Steve", json.RawMessage(`{}`))
	unknown := observationResultMap(t, value, err)
	if unknown["known"] != false || unknown["x"] != nil {
		t.Fatalf("unloaded location leaked: %v", unknown)
	}
	column := chunks.NewColumn(0, 0)
	chunkData, err := column.EncodeSections()
	if err != nil {
		t.Fatal(err)
	}
	o.session.world.HandlePacket(observationPacket(t, &packets.S2CLevelChunkWithLight{ChunkData: ns.ChunkData{Data: chunkData}}))
	value, err = o.playerPositionTool(context.Background(), "Steve", json.RawMessage(`{}`))
	position := observationResultMap(t, value, err)
	if position["known"] != true || position["x"] != float64(2) {
		t.Fatalf("tracked position = %v", position)
	}
	o.session.entities.HandlePacket(observationPacket(t, &packets.S2CSetEquipment{EntityId: 42, Slots: []packets.EquipmentEntry{{Slot: 0, Item: ns.NewSlot(ns.VarInt(items.ItemID("minecraft:diamond_sword")), 1)}}}))
	value, err = o.playerEquipmentTool(context.Background(), "Steve", json.RawMessage(`{}`))
	equipment := observationResultMap(t, value, err)
	slots := equipment["slots"].(map[string]any)
	if slots["mainhand"].(map[string]any)["name"] != "minecraft:diamond_sword" || slots["offhand"] != nil {
		t.Fatalf("equipment = %v", equipment)
	}
	// An ordinary held-slot change must replace equipment on the same tracked
	// entity, without another spawn or chunk load.
	o.session.entities.HandlePacket(observationPacket(t, &packets.S2CSetEquipment{EntityId: 42, Slots: []packets.EquipmentEntry{
		{Slot: 0, Item: ns.NewSlot(ns.VarInt(items.ItemID("minecraft:iron_axe")), 1)},
		{Slot: 5, Item: ns.NewSlot(ns.VarInt(items.ItemID("minecraft:iron_helmet")), 1)},
	}}))
	value, err = o.playerEquipmentTool(context.Background(), "Steve", json.RawMessage(`{}`))
	updated := observationResultMap(t, value, err)["slots"].(map[string]any)
	if updated["mainhand"].(map[string]any)["name"] != "minecraft:iron_axe" || updated["head"].(map[string]any)["name"] != "minecraft:iron_helmet" {
		t.Fatalf("equipment replacement was not visible: %v", updated)
	}
	o.session.entities.HandlePacket(observationPacket(t, &packets.S2CEntityEvent{EntityId: 42, EntityStatus: 55}))
	value, err = o.playerEquipmentTool(context.Background(), "Steve", json.RawMessage(`{}`))
	swapped := observationResultMap(t, value, err)["slots"].(map[string]any)
	if swapped["mainhand"] != nil || swapped["offhand"].(map[string]any)["name"] != "minecraft:iron_axe" {
		t.Fatalf("hand swap left stale equipment: %v", swapped)
	}
	o.session.entities.HandlePacket(observationPacket(t, &packets.S2CSetEquipment{EntityId: 42, Slots: []packets.EquipmentEntry{
		{Slot: 0, Item: ns.NewSlot(ns.VarInt(items.ItemID("minecraft:diamond_pickaxe")), 1)},
		{Slot: 5, Item: ns.EmptySlot()},
	}}))
	value, err = o.playerEquipmentTool(context.Background(), "Steve", json.RawMessage(`{}`))
	cleared := observationResultMap(t, value, err)["slots"].(map[string]any)
	if cleared["mainhand"].(map[string]any)["name"] != "minecraft:diamond_pickaxe" ||
		cleared["head"].(map[string]any)["count"] != float64(0) ||
		cleared["offhand"].(map[string]any)["name"] != "minecraft:iron_axe" {
		t.Fatalf("partial update after swap lost current equipment: %v", cleared)
	}
	unload := observationPacket(t, &packets.S2CForgetLevelChunk{})
	o.session.world.HandlePacket(unload)
	o.session.entities.HandlePacket(unload)
	value, err = o.playerEquipmentTool(context.Background(), "Steve", json.RawMessage(`{}`))
	if got := observationResultMap(t, value, err); got["known"] != false || got["slots"] != nil {
		t.Fatalf("equipment survived unload: %v", got)
	}
	c.FireTransfer()
	value, err = o.playerPositionTool(context.Background(), "Steve", json.RawMessage(`{}`))
	if got := observationResultMap(t, value, err); got["known"] != false || got["x"] != nil {
		t.Fatalf("position survived transfer: %v", got)
	}
	c.FirePlay()
	value, err = o.playerEquipmentTool(context.Background(), "Steve", json.RawMessage(`{}`))
	if got := observationResultMap(t, value, err); got["known"] != false {
		t.Fatalf("new play session retained player: %v", got)
	}
}

func TestTabToolWorksWithoutIncomingPacketsAndResets(t *testing.T) {
	o, c := observationFixture(t)
	o.session.players.HandlePacket(observationPacket(t, &packets.S2CTabList{Header: ns.NewTextComponent("SteedsGate"), Footer: ns.NewTextComponent("Welcome")}))
	value, err := o.onlinePlayersTool(context.Background(), "Steve", json.RawMessage(`{}`))
	got := observationResultMap(t, value, err)
	if got["header"] != "SteedsGate" || got["footer"] != "Welcome" || got["header_footer_known"] != true {
		t.Fatalf("tab tool = %v", got)
	}
	c.FireDisconnect()
	value, err = o.onlinePlayersTool(context.Background(), "Steve", json.RawMessage(`{}`))
	if got := observationResultMap(t, value, err); got["known"] != false || got["header"] != nil {
		t.Fatalf("tab survived disconnect: %v", got)
	}
}

func TestInspectBlockRejectsUnloadedAndFarCoordinates(t *testing.T) {
	o, _ := observationFixture(t)
	for _, raw := range []string{`{"x":1,"y":64,"z":1}`, `{"x":100,"y":64,"z":1}`} {
		if value, err := o.inspectBlockTool(context.Background(), "Steve", json.RawMessage(raw)); err == nil {
			t.Fatalf("unknown/far block returned as known: %v", value)
		}
	}
	column := chunks.NewColumn(0, 0)
	chunkData, err := column.EncodeSections()
	if err != nil {
		t.Fatal(err)
	}
	o.session.world.HandlePacket(observationPacket(t, &packets.S2CLevelChunkWithLight{ChunkData: ns.ChunkData{Data: chunkData}}))
	value, err := o.inspectBlockTool(context.Background(), "Steve", json.RawMessage(`{"x":1,"y":64,"z":1}`))
	if got := observationResultMap(t, value, err); got["block"] != "minecraft:air" {
		t.Fatalf("loaded air = %v", got)
	}
}
