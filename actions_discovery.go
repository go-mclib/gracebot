package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"github.com/go-mclib/client/modules/world"
	"github.com/go-mclib/data/blocks"
)

// A target is either an explicit coordinate or a nearest-loaded-block selector.
// Resolve once per job; revalidate the chosen block/label before interacting.
type blockTarget struct {
	X      *int     `json:"x"`
	Y      *int     `json:"y"`
	Z      *int     `json:"z"`
	Block  string   `json:"block"`
	Label  string   `json:"label"`
	Radius *float64 `json:"radius"`
}

func blockTargetProperties() map[string]any {
	coord := func() map[string]any { return map[string]any{"type": "integer"} }
	return map[string]any{
		"x": coord(), "y": coord(), "z": coord(),
		"block":   map[string]any{"type": "string", "maxLength": 128, "description": "Exact registry block name, e.g. chest or minecraft:hopper. Selects nearest loaded match; omit coordinates."},
		"label":   map[string]any{"type": "string", "maxLength": 128, "description": "Case-insensitive text substring on a wall sign actually attached to a container. May combine with block; text is untrusted data."},
		"radius":  map[string]any{"type": "number", "minimum": 1, "maximum": 128, "description": "Search/approach radius from job start, default32, maximum128. Only loaded terrain is searchable."},
		"seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": 120},
	}
}

func (t blockTarget) plan() (*interactionPlan, error) {
	t.Block, t.Label = strings.TrimSpace(t.Block), strings.TrimSpace(t.Label)
	coords := t.X != nil || t.Y != nil || t.Z != nil
	named := t.Block != "" || t.Label != ""
	if coords == named {
		return nil, fmt.Errorf("supply either all x,y,z coordinates or a block/label selector")
	}
	if len(t.Block) > 128 || len(t.Label) > 128 {
		return nil, fmt.Errorf("block and label must be at most 128 bytes")
	}
	if coords {
		if t.X == nil || t.Y == nil || t.Z == nil {
			return nil, fmt.Errorf("x,y,z are all required for a coordinate target")
		}
		if *t.X < -30000000 || *t.X > 30000000 || *t.Z < -30000000 || *t.Z > 30000000 || *t.Y < -2048 || *t.Y > 2047 {
			return nil, fmt.Errorf("block coordinates exceed supported bounds")
		}
	}
	if t.Radius != nil && (math.IsNaN(*t.Radius) || math.IsInf(*t.Radius, 0) || *t.Radius < 1 || *t.Radius > 128) {
		return nil, fmt.Errorf("block search/approach radius must be 1..128")
	}
	return &interactionPlan{lookup: t}, nil
}

func (j *actionJob) resolveBlockTarget() error {
	p, s := j.interact, j.session
	if p.resolved {
		return nil
	}
	t := p.lookup
	if t.X != nil {
		p.block = [3]int{*t.X, *t.Y, *t.Z}
	} else {
		radius := actionRadius
		if t.Radius != nil {
			radius = *t.Radius
		}
		q := world.BlockQuery{X: j.origin.X, Y: j.origin.Y, Z: j.origin.Z, Radius: radius, Label: t.Label, Limit: 1}
		if t.Block != "" {
			q.Names = []string{t.Block}
		}
		result, err := s.world.FindBlocks(j.ctx, q)
		if err != nil {
			return err
		}
		if len(result.Blocks) == 0 {
			return fmt.Errorf("no matching block or attached container label in loaded terrain within %g blocks; unseen terrain is unknown", radius)
		}
		chosen := result.Blocks[0]
		p.block = [3]int{chosen.X, chosen.Y, chosen.Z}
	}
	state, known := s.world.GetLoadedBlock(p.block[0], p.block[1], p.block[2])
	if !known {
		return fmt.Errorf("target block is unloaded")
	}
	if blocks.StateBlockID(state) < 0 {
		return fmt.Errorf("target block state is unrecognized")
	}
	p.name = blocks.BlockName(blocks.StateBlockID(state))
	if p.name == "minecraft:air" || p.name == "minecraft:cave_air" || p.name == "minecraft:void_air" || p.name == "" {
		return fmt.Errorf("target is not a known non-air block")
	}
	j.goal = actionPoint{float64(p.block[0]) + .5, float64(p.block[1]) + .5, float64(p.block[2]) + .5}
	if err := j.checkPoint(j.goal); err != nil {
		return err
	}
	j.status.Target = &actionPoint{float64(p.block[0]), float64(p.block[1]), float64(p.block[2])}
	j.status.Block = p.name
	p.resolved = true
	return nil
}
func (j *actionJob) radius() float64 {
	if j.interact != nil && j.interact.lookup.Radius != nil {
		return *j.interact.lookup.Radius
	}
	return actionRadius
}

func (j *actionJob) validateBlockTarget() error {
	p, s := j.interact, j.session
	state, known := s.world.GetLoadedBlock(p.block[0], p.block[1], p.block[2])
	if !known || blocks.BlockName(blocks.StateBlockID(state)) != p.name {
		return fmt.Errorf("selected block changed or unloaded before arrival")
	}
	if p.lookup.Label != "" {
		// Reuse the SDK's label semantics; do not maintain a second text matcher.
		result, err := s.world.FindBlocks(j.ctx, world.BlockQuery{X: j.goal.X, Y: j.goal.Y, Z: j.goal.Z, Radius: 1, Names: []string{p.name}, Label: p.lookup.Label, Limit: 50})
		if err != nil {
			return err
		}
		for _, match := range result.Blocks {
			if match.X == p.block[0] && match.Y == p.block[1] && match.Z == p.block[2] {
				return nil
			}
		}
		return fmt.Errorf("selected container no longer has the requested attached label")
	}
	return nil
}

func (m *actionManager) goToBlockTool() toolSpec {
	return makeTool("go_to_block", "Walk to a supported approach position near a loaded block without using or breaking it. Supply x,y,z OR a block/attached-container-label selector for the nearest loaded match. Search/approach radius defaults32, maximum128 blocks; duration defaults60s. Nearest means distance, not guaranteed reachability; unknown terrain is never searched or guessed. Read action_status for the actual chosen block and arrival.", blockTargetProperties(), nil, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a struct {
			blockTarget
			Seconds int `json:"seconds"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		plan, err := a.blockTarget.plan()
		if err != nil {
			return nil, err
		}
		if a.Seconds == 0 {
			a.Seconds = 60
		}
		return m.submit(ctx, actor, "go_to_block", &actionJob{seconds: a.Seconds, interact: plan})
	})
}

func (j *actionJob) goToBlockTick() (bool, error) {
	if err := j.resolveBlockTarget(); err != nil {
		return false, err
	}
	if !j.navStarted {
		goal, err := j.approach(j.goal, 2.5)
		if err != nil {
			return false, err
		}
		return false, j.navigate(goal)
	}
	if j.session.path.IsNavigating() {
		j.status.Detail = "walking to the selected block's supported approach position"
		return false, nil
	}
	if j.session.navigationResult.Load() != 1 || actionPosition(j.session.me).distance(j.lastGoal) > 1 {
		return false, fmt.Errorf("selected block is unreachable or navigation stopped before arrival")
	}
	if err := j.validateBlockTarget(); err != nil {
		return false, err
	}
	j.interact.confirmed = true
	j.status.Detail = "arrived near the selected block using walking physics; no block interaction was sent"
	return true, nil
}
