package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/go-mclib/client/modules/collisions"
	"github.com/go-mclib/client/modules/inventory"
	"github.com/go-mclib/client/modules/self"
	"github.com/go-mclib/data/blocks"
	"github.com/go-mclib/data/items"
	"github.com/go-mclib/data/packets"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

type buildCell struct {
	Pos   [3]int
	Name  string
	Item  int32
	State int32
}
type buildPlan struct {
	cells           []buildCell
	occupied        map[[3]int]bool
	preflight       bool
	index           int
	preflightIndex  int
	dataChecked     bool
	pending         bool
	pendingAt       time.Time
	inventoryBefore int
	slot            int
	slotVersion     uint64
	swapping        bool
	swapItem        int32
	swapVersion     uint64
	swapAt          time.Time
	stand           actionPoint
}

type buildArgs struct {
	Origin  []int             `json:"origin"`
	Palette map[string]string `json:"palette"`
	Layers  [][]string        `json:"layers"`
	Size    []int             `json:"size"`
	Block   string            `json:"block"`
	Seconds int               `json:"seconds"`
}

func (m *actionManager) buildTool() toolSpec {
	return makeTool("build_arrangement", "Trusted-only survival construction, at most 64 solid blocks in an 8x4x8 volume within 32 blocks. Supply origin [x,y,z] and EITHER size [width,height,depth]+block OR palette+layers (layers bottom to top; each layer has z rows of x characters; '.' skips). Supports inert full cubes such as stone, cobblestone, bricks, planks, wool and concrete. Requires existing/planned full-block support below every cell; no floating blocks, replacement, digging, commands, orientation-sensitive or interactive blocks. Preflights materials and outside access before placing; each placement requires server block plus inventory confirmation. Cancellation/partial failure never rolls back.", map[string]any{
		"origin":  map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "minItems": 3, "maxItems": 3},
		"size":    map[string]any{"type": "array", "items": map[string]any{"type": "integer", "minimum": 1, "maximum": 8}, "minItems": 3, "maxItems": 3},
		"block":   map[string]any{"type": "string", "maxLength": 80},
		"palette": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string", "maxLength": 80}},
		"layers":  map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "string", "maxLength": 8}}},
		"seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
	}, []string{"origin"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a buildArgs
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		plan, err := compileBuild(a)
		if err != nil {
			return nil, err
		}
		if a.Seconds == 0 {
			a.Seconds = 120
		}
		return m.submit(ctx, actor, "build", &actionJob{build: plan, seconds: a.Seconds})
	})
}

// The allowlist deliberately excludes gravity, axis/facing, waterlogging,
// interaction, damage, redstone and block-entity behavior. Registry validation
// follows suffix matching, so user-provided names cannot invent block types.
func simpleBuildBlock(name string) bool {
	switch name {
	case "minecraft:stone", "minecraft:cobblestone", "minecraft:mossy_cobblestone", "minecraft:stone_bricks", "minecraft:mossy_stone_bricks", "minecraft:cracked_stone_bricks", "minecraft:chiseled_stone_bricks", "minecraft:bricks", "minecraft:dirt", "minecraft:coarse_dirt", "minecraft:granite", "minecraft:polished_granite", "minecraft:diorite", "minecraft:polished_diorite", "minecraft:andesite", "minecraft:polished_andesite", "minecraft:cobbled_deepslate", "minecraft:polished_deepslate", "minecraft:deepslate_bricks", "minecraft:deepslate_tiles", "minecraft:netherrack", "minecraft:nether_bricks", "minecraft:red_nether_bricks", "minecraft:end_stone", "minecraft:end_stone_bricks", "minecraft:obsidian", "minecraft:sandstone", "minecraft:smooth_sandstone", "minecraft:cut_sandstone", "minecraft:red_sandstone", "minecraft:smooth_red_sandstone", "minecraft:cut_red_sandstone", "minecraft:terracotta":
		return true
	}
	return strings.HasSuffix(name, "_planks") || strings.HasSuffix(name, "_wool") || strings.HasSuffix(name, "_concrete") || (strings.HasSuffix(name, "_terracotta") && !strings.HasSuffix(name, "_glazed_terracotta"))
}
func buildMaterial(name string) (string, int32, int32, error) {
	if !strings.Contains(name, ":") {
		name = "minecraft:" + name
	}
	block, item := blocks.BlockID(name), items.ItemID(name)
	if block < 0 || item < 0 || !simpleBuildBlock(name) {
		return "", 0, 0, fmt.Errorf("unsupported build material %q: use an inert full cube; orientation-sensitive/interactive/falling blocks are rejected", name)
	}
	state := blocks.DefaultStateID(block)
	_, props := blocks.StateProperties(int(state))
	if len(props) != 0 {
		return "", 0, 0, fmt.Errorf("unsupported placement properties for %s", name)
	}
	return name, item, state, nil
}
func compileBuild(a buildArgs) (*buildPlan, error) {
	if len(a.Origin) != 3 {
		return nil, fmt.Errorf("origin must contain x,y,z")
	}
	if a.Origin[0] < -30000000 || a.Origin[0] > 29999992 || a.Origin[2] < -30000000 || a.Origin[2] > 29999992 || a.Origin[1] < -2048 || a.Origin[1] > 2040 {
		return nil, fmt.Errorf("build origin out of bounds")
	}
	p := &buildPlan{occupied: make(map[[3]int]bool)}
	add := func(x, y, z int, name string) error {
		name, item, state, err := buildMaterial(name)
		if err != nil {
			return err
		}
		pos := [3]int{a.Origin[0] + x, a.Origin[1] + y, a.Origin[2] + z}
		p.cells = append(p.cells, buildCell{pos, name, item, state})
		p.occupied[pos] = true
		if len(p.cells) > 64 {
			return fmt.Errorf("build volume exceeds 64 blocks")
		}
		return nil
	}
	if len(a.Size) > 0 || a.Block != "" {
		if len(a.Size) != 3 || a.Block == "" || len(a.Layers) > 0 || len(a.Palette) > 0 {
			return nil, fmt.Errorf("supply either size+block or palette+layers")
		}
		x, y, z := a.Size[0], a.Size[1], a.Size[2]
		if x < 1 || x > 8 || y < 1 || y > 4 || z < 1 || z > 8 || x*y*z > 64 {
			return nil, fmt.Errorf("cuboid must fit 8x4x8 and contain at most 64 blocks")
		}
		for iy := range y {
			for iz := range z {
				for ix := range x {
					if err := add(ix, iy, iz, a.Block); err != nil {
						return nil, err
					}
				}
			}
		}
	} else {
		if len(a.Layers) < 1 || len(a.Layers) > 4 || len(a.Palette) < 1 || len(a.Palette) > 16 {
			return nil, fmt.Errorf("provide 1..4 layers and 1..16 palette entries")
		}
		for key, name := range a.Palette {
			if len(key) != 1 || key == "." || key[0] < 33 || key[0] > 126 {
				return nil, fmt.Errorf("palette keys must be single printable ASCII characters other than '.'")
			}
			if _, _, _, err := buildMaterial(name); err != nil {
				return nil, err
			}
		}
		depth, width := len(a.Layers[0]), 0
		if depth < 1 || depth > 8 {
			return nil, fmt.Errorf("each layer needs 1..8 rows")
		}
		for y, layer := range a.Layers {
			if len(layer) != depth {
				return nil, fmt.Errorf("layers must have equal depth")
			}
			for z, row := range layer {
				if width == 0 {
					width = len(row)
				}
				if len(row) != width || width < 1 || width > 8 {
					return nil, fmt.Errorf("rows must have equal widths of 1..8 ASCII characters")
				}
				for x := range len(row) {
					if row[x] == '.' {
						continue
					}
					name, ok := a.Palette[string(row[x])]
					if !ok {
						return nil, fmt.Errorf("undefined palette character %q", row[x])
					}
					if err := add(x, y, z, name); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	if len(p.cells) == 0 {
		return nil, fmt.Errorf("arrangement contains no blocks")
	}
	return p, nil
}

func isBuildAir(state int32) bool {
	name := blocks.BlockName(blocks.StateBlockID(state))
	return name == "minecraft:air" || name == "minecraft:cave_air" || name == "minecraft:void_air"
}
func buildSupport(state int32) bool {
	name := blocks.BlockName(blocks.StateBlockID(state))
	return simpleBuildBlock(name) || name == "minecraft:grass_block" || name == "minecraft:bedrock"
}
func inventoryCount(inv *inventory.Module, item int32) int {
	n := 0
	for slot := inventory.SlotMainStart; slot < inventory.SlotHotbarEnd; slot++ {
		stack := inv.GetSlot(slot)
		if !stack.IsEmpty() && stack.ID == item {
			n += int(stack.Count)
		}
	}
	return n
}

// preflightBuildData is also the deterministic planner boundary: absent terrain
// cannot become air, and planned support is valid only after its earlier layer.
func preflightBuildData(cells []buildCell, block func([3]int) (int32, bool), count func(int32) int) error {
	needed := make(map[int32]int)
	planned := make(map[[3]int]bool)
	for _, cell := range cells {
		state, ok := block(cell.Pos)
		if !ok {
			return fmt.Errorf("unloaded build cell %v", cell.Pos)
		}
		if !isBuildAir(state) {
			return fmt.Errorf("occupied build cell %v; destructive replacement is forbidden", cell.Pos)
		}
		below := cell.Pos
		below[1]--
		support, known := block(below)
		if !known {
			return fmt.Errorf("unloaded support below %v", cell.Pos)
		}
		if !planned[below] && !buildSupport(support) {
			return fmt.Errorf("unsupported cell %v: requires inert full-block support below", cell.Pos)
		}
		planned[cell.Pos] = true
		needed[cell.Item]++
	}
	for item, n := range needed {
		if have := count(item); have < n {
			return fmt.Errorf("insufficient %s: need %d, have %d", items.ItemName(item), n, have)
		}
	}
	return nil
}

func (j *actionJob) buildTick() (bool, error) {
	p, s := j.build, j.session
	if p == nil {
		return false, fmt.Errorf("build plan missing")
	}
	if s.me.Gamemode() != 0 {
		return false, fmt.Errorf("survival building requires survival gamemode, not creative/adventure/spectator")
	}
	if s.inventory.ContainerOpen() || !s.inventory.CursorItem().IsEmpty() {
		return false, fmt.Errorf("close inventory container and clear cursor before building")
	}
	s.mu.Lock()
	known := s.inventoryKnown
	s.mu.Unlock()
	if !known {
		return false, fmt.Errorf("inventory not yet confirmed by server since session reset")
	}
	if !p.preflight {
		if !p.dataChecked {
			block := func(pos [3]int) (int32, bool) { return s.world.GetLoadedBlock(pos[0], pos[1], pos[2]) }
			if err := preflightBuildData(p.cells, block, func(item int32) int { return inventoryCount(s.inventory, item) }); err != nil {
				return false, err
			}
			for _, cell := range p.cells {
				if j.cellOccupiedByEntity(cell.Pos) {
					return false, fmt.Errorf("entity occupies preflight cell %v", cell.Pos)
				}
			}
			p.dataChecked = true
		}
		// Bound planning work per physics tick; no placement until every cell passes.
		cell := p.cells[p.preflightIndex]
		if err := j.checkPoint(actionPoint{float64(cell.Pos[0]), float64(cell.Pos[1]), float64(cell.Pos[2])}); err != nil {
			return false, err
		}
		if _, err := j.buildStand(cell); err != nil {
			return false, fmt.Errorf("preflight %v: %w", cell.Pos, err)
		}
		p.preflightIndex++
		if p.preflightIndex < len(p.cells) {
			return false, nil
		}
		p.preflight = true
		j.status.Total = len(p.cells)
		j.status.Detail = "materials, support, occupancy and loaded outside access preflighted"
	}
	if p.index == len(p.cells) {
		j.status.Detail = "all blocks and material consumption confirmed by server"
		return true, nil
	}
	cell := p.cells[p.index]
	if err := j.checkPoint(actionPoint{float64(cell.Pos[0]), float64(cell.Pos[1]), float64(cell.Pos[2])}); err != nil {
		return false, err
	}
	if p.pending {
		state, loaded := s.world.GetLoadedBlock(cell.Pos[0], cell.Pos[1], cell.Pos[2])
		if !loaded {
			return false, fmt.Errorf("placement terrain unloaded")
		}
		s.mu.Lock()
		version := s.slotVersion[p.slot]
		s.mu.Unlock()
		consumed := inventoryCount(s.inventory, cell.Item) < p.inventoryBefore
		if placementConfirmed(state, cell.State, version, p.slotVersion, consumed) {
			p.pending = false
			p.index++
			j.status.Placed = p.index
			j.navStarted = false
			return false, nil
		}
		if !isBuildAir(state) && state != cell.State {
			return false, fmt.Errorf("unexpected server block at %v", cell.Pos)
		}
		if time.Since(p.pendingAt) > 4*time.Second {
			return false, fmt.Errorf("placement %v not confirmed by both server block and inventory updates (confirmed %d/%d)", cell.Pos, p.index, len(p.cells))
		}
		return false, nil
	}
	if p.swapping {
		s.mu.Lock()
		version := s.slotVersion[inventory.SlotHotbarStart]
		s.mu.Unlock()
		stack := s.inventory.GetSlot(inventory.SlotHotbarStart)
		if version > p.swapVersion && !stack.IsEmpty() && stack.ID == p.swapItem {
			p.swapping = false
		} else {
			if time.Since(p.swapAt) > 4*time.Second {
				return false, fmt.Errorf("server did not confirm inventory-to-hotbar swap")
			}
			return false, nil
		}
	}
	state, loaded := s.world.GetLoadedBlock(cell.Pos[0], cell.Pos[1], cell.Pos[2])
	if !loaded || !isBuildAir(state) {
		return false, fmt.Errorf("build cell %v changed or unloaded before placement", cell.Pos)
	}
	below := cell.Pos
	below[1]--
	support, loaded := s.world.GetLoadedBlock(below[0], below[1], below[2])
	if !loaded || !buildSupport(support) {
		return false, fmt.Errorf("support below %v changed before placement", cell.Pos)
	}
	if !j.canPlaceFrom(actionPosition(s.me), cell) {
		if j.navStarted && s.path.IsNavigating() {
			return false, nil
		}
		if j.navStarted && s.navigationResult.Load() < 0 {
			return false, fmt.Errorf("build work position became unreachable")
		}
		stand, err := j.buildStand(cell)
		if err != nil {
			return false, err
		}
		p.stand = stand
		if err = j.navigate(stand); err != nil {
			return false, err
		}
		return false, nil
	}
	s.stopMotion()
	j.navStarted = false
	if j.cellOccupiedByEntity(cell.Pos) {
		return false, fmt.Errorf("entity occupies placement cell %v", cell.Pos)
	}
	slot := s.inventory.FindItem(cell.Item)
	if slot < 0 {
		return false, fmt.Errorf("material %s no longer available", cell.Name)
	}
	if slot < inventory.SlotHotbarStart {
		s.mu.Lock()
		p.swapVersion = s.slotVersion[inventory.SlotHotbarStart]
		s.mu.Unlock()
		if err := s.inventory.RequestHotbarSwap(slot, 0, j.send); err != nil {
			return false, err
		}
		p.swapping = true
		p.swapItem = cell.Item
		p.swapAt = time.Now()
		return false, nil
	}
	if err := s.inventory.SelectHotbar(slot-inventory.SlotHotbarStart, j.send); err != nil {
		return false, err
	}
	p.inventoryBefore = inventoryCount(s.inventory, cell.Item)
	s.mu.Lock()
	p.slotVersion = s.slotVersion[slot]
	s.mu.Unlock()
	p.slot = slot
	// Top face only: no orientation ambiguity, interactive support, or replacing.
	s.me.LookAt(float64(cell.Pos[0])+0.5, float64(cell.Pos[1]), float64(cell.Pos[2])+0.5)
	if err := j.send(&packets.C2SUseItemOn{Hand: 0, Location: ns.Position{X: below[0], Y: below[1], Z: below[2]}, Face: 1, CursorPositionX: 0.5, CursorPositionY: 1, CursorPositionZ: 0.5, Sequence: ns.VarInt(s.client.NextBISequence())}); err != nil {
		return false, err
	}
	p.pending = true
	p.pendingAt = time.Now()
	j.status.Detail = fmt.Sprintf("awaiting server confirmation for block %d/%d", p.index+1, len(p.cells))
	return false, nil
}
func placementConfirmed(actual, expected int32, version, before uint64, consumed bool) bool {
	return actual == expected && version > before && consumed
}

func (j *actionJob) cellOccupiedByEntity(pos [3]int) bool {
	s := j.session
	box := collisions.AABB{MinX: float64(pos[0]), MinY: float64(pos[1]), MinZ: float64(pos[2]), MaxX: float64(pos[0] + 1), MaxY: float64(pos[1] + 1), MaxZ: float64(pos[2] + 1)}
	p := actionPosition(s.me)
	if box.Intersects(collisions.EntityAABB(p.X, p.Y, p.Z, collisions.PlayerWidth, collisions.PlayerHeight)) {
		return true
	}
	return len(s.entities.GetEntitiesInAABB(box.MinX, box.MinY, box.MinZ, box.MaxX, box.MaxY, box.MaxZ)) > 0
}
func (j *actionJob) canPlaceFrom(stand actionPoint, cell buildCell) bool {
	eye := stand
	eye.Y += self.EyeHeight
	face := actionPoint{float64(cell.Pos[0]) + 0.5, float64(cell.Pos[1]) + 0.001, float64(cell.Pos[2]) + 0.5}
	if eye.Y <= face.Y || eye.distance(face) > 4.25 {
		return false
	}
	// Unknown blocks anywhere along the ray cannot be assumed transparent.
	if !j.loadedRay(eye, face) {
		return false
	}
	col := collisions.From(j.session.client)
	hit, _, _, _ := col.RaycastBlocks(eye.X, eye.Y, eye.Z, face.X, face.Y, face.Z)
	return !hit
}
func (j *actionJob) loadedRay(from, to actionPoint) bool {
	steps := int(math.Ceil(from.distance(to)*4)) + 1
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		x, y, z := from.X+(to.X-from.X)*t, from.Y+(to.Y-from.Y)*t, from.Z+(to.Z-from.Z)*t
		if _, ok := j.session.world.GetLoadedBlock(int(math.Floor(x)), int(math.Floor(y)), int(math.Floor(z))); !ok {
			return false
		}
	}
	return true
}
func (j *actionJob) buildStand(cell buildCell) (actionPoint, error) {
	s, p := j.session, j.build
	col := collisions.From(s.client)
	current := actionPosition(s.me)
	candidates := make([]actionPoint, 0, 64)
	for dx := -4; dx <= 4; dx++ {
		for dz := -4; dz <= 4; dz++ {
			for dy := -2; dy <= 1; dy++ {
				x, y, z := cell.Pos[0]+dx, cell.Pos[1]+dy, cell.Pos[2]+dz
				blocked := false
				for h := 0; h <= 2; h++ {
					if p.occupied[[3]int{x, y + h, z}] {
						blocked = true
						break
					}
				}
				if blocked {
					continue
				}
				point := actionPoint{float64(x) + 0.5, float64(y), float64(z) + 0.5}
				if j.checkPoint(point) != nil {
					continue
				}
				surface, ok := col.SupportSurfaceY(x, y, z, collisions.PlayerWidth)
				if !ok {
					continue
				}
				point.Y = surface
				if !col.CanFitAt(point.X, point.Y, point.Z, collisions.PlayerWidth, collisions.PlayerHeight) || !j.canPlaceFrom(point, cell) {
					continue
				}
				candidates = append(candidates, point)
			}
		}
	}
	sort.Slice(candidates, func(a, b int) bool {
		da, db := candidates[a].distance(current), candidates[b].distance(current)
		if da != db {
			return da < db
		}
		if candidates[a].X != candidates[b].X {
			return candidates[a].X < candidates[b].X
		}
		if candidates[a].Y != candidates[b].Y {
			return candidates[a].Y < candidates[b].Y
		}
		return candidates[a].Z < candidates[b].Z
	})
	// A bounded number of actual path searches, not just geometric reach.
	for _, point := range candidates[:min(len(candidates), 12)] {
		if err := j.ctx.Err(); err != nil {
			return actionPoint{}, err
		}
		route, err := s.path.FindPathContext(j.ctx, point.X, point.Y, point.Z)
		if err != nil {
			continue
		}
		clear := true
		for _, node := range route {
			for dy := 0; dy <= 2; dy++ {
				if p.occupied[[3]int{node.X, node.Y + dy, node.Z}] {
					clear = false
					break
				}
			}
			if !clear {
				break
			}
		}
		if clear {
			return point, nil
		}
	}
	return actionPoint{}, fmt.Errorf("no loaded, supported, reachable outside work position for %v", cell.Pos)
}
