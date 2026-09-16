package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-mclib/client"
	"github.com/go-mclib/client/modules/collisions"
	"github.com/go-mclib/client/modules/combat"
	"github.com/go-mclib/client/modules/entities"
	"github.com/go-mclib/client/modules/inventory"
	"github.com/go-mclib/client/modules/pathfinding"
	"github.com/go-mclib/client/modules/physics"
	"github.com/go-mclib/client/modules/playerlist"
	"github.com/go-mclib/client/modules/self"
	"github.com/go-mclib/client/modules/world"
	"github.com/go-mclib/data/packet_ids"
	"github.com/go-mclib/data/packets"
	jp "github.com/go-mclib/protocol/java_protocol"
	ns "github.com/go-mclib/protocol/java_protocol/net_structures"
)

const actionRadius = 32.0
const actionHistoryLimit = 12
const maxMoveSeconds = 3600

type actionPoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (p actionPoint) distance(q actionPoint) float64 {
	return math.Sqrt((p.X-q.X)*(p.X-q.X) + (p.Y-q.Y)*(p.Y-q.Y) + (p.Z-q.Z)*(p.Z-q.Z))
}
func actionPosition(s *self.Module) actionPoint { x, y, z := s.Position(); return actionPoint{x, y, z} }

type actionStatus struct {
	ID       uint64       `json:"id"`
	Kind     string       `json:"kind"`
	Owner    string       `json:"owner"`
	State    string       `json:"state"`
	Detail   string       `json:"detail"`
	Started  time.Time    `json:"started"`
	Finished *time.Time   `json:"finished,omitempty"`
	Placed   int          `json:"confirmed_blocks,omitempty"`
	Total    int          `json:"total_blocks,omitempty"`
	Attacks  int64        `json:"submitted_attacks,omitempty"`
	Target   *actionPoint `json:"target,omitempty"`
	Block    string       `json:"block,omitempty"`
}

type actionSession struct {
	client           *client.Client
	me               *self.Module
	world            *world.Module
	entities         *entities.Module
	players          *playerlist.Module
	inventory        *inventory.Module
	physics          *physics.Module
	path             *pathfinding.Module
	combat           *combat.Module
	ctx              context.Context
	cancel           context.CancelFunc
	mu               sync.Mutex
	positionKnown    bool
	inventoryKnown   bool
	vehicle          int32
	passengerVersion uint64
	deadEntities     map[int32]bool
	slotVersion      [inventory.TotalSlots]uint64
	ticks            atomic.Uint64
	attacks          atomic.Int64
	navigationResult atomic.Int32 // 0 pending, 1 reached, -1 failed
	blockAck         int32
	containerWindow  int32
	containerReady   bool
}

type actionJob struct {
	status           actionStatus // owner goroutine only; copies are published under manager.mu
	ctx              context.Context
	cancel           context.CancelFunc
	session          *actionSession
	goal             actionPoint
	origin           actionPoint
	target           int32
	targetUUID       [16]byte
	targetBound      bool
	player           string
	seconds          int
	initialized      bool
	navStarted       bool
	lastGoal         actionPoint
	lastRepath       uint64
	attackStart      int64
	sent             bool
	sentAt           time.Time
	passengerVersion uint64
	build            *buildPlan
	interact         *interactionPlan
	use              *itemUsePlan
}

type actionManager struct {
	store     *playerStore
	mu        sync.Mutex // lifecycle, published state only; never held across module calls
	controlMu sync.Mutex // serialize external submit/stop requests
	execMu    sync.Mutex // synchronous action-owner barrier, never taken by lifecycle callbacks
	session   *actionSession
	current   *actionJob
	published *actionStatus
	history   []actionStatus
	nextID    uint64
	closed    bool
}

func newActionManager(store *playerStore) *actionManager { return &actionManager{store: store} }
func (m *actionManager) admin(actor string) bool {
	return m.store != nil && m.store.hasRank(actor, "admin")
}

func (m *actionManager) register(c *client.Client) {
	me, w, ents, inv, p, col := self.From(c), world.From(c), entities.From(c), inventory.From(c), physics.From(c), collisions.From(c)
	if me == nil || w == nil || ents == nil || inv == nil || p == nil || col == nil {
		return
	}
	nav := pathfinding.From(c)
	if nav == nil {
		nav = pathfinding.New()
		c.Register(nav)
	}
	fighter := combat.From(c)
	if fighter == nil {
		fighter = combat.New()
		c.Register(fighter)
	}
	bind := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.closed {
			return
		}
		if m.session != nil {
			m.session.cancel()
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.session = &actionSession{client: c, me: me, world: w, entities: ents, players: playerlist.From(c), inventory: inv, physics: p, path: nav, combat: fighter, ctx: ctx, cancel: cancel}
	}
	reset := func() {
		m.mu.Lock()
		if m.session != nil && m.session.client == c {
			m.session.cancel()
			m.session = nil
		}
		m.mu.Unlock()
	}
	c.OnPlay(bind)
	c.OnDisconnect(reset)
	c.OnTransfer(reset)
	me.OnRespawn(bind)
	me.OnDeath(reset)
	// Registered before Wire appends navigation/combat callbacks: this owner
	// checks cancellation/target visibility before either module acts this tick.
	p.OnTick(func() { m.tick(c) })
	nav.OnNavigationComplete(func(reached bool) {
		m.mu.Lock()
		s := m.session
		m.mu.Unlock()
		if s != nil && s.client == c {
			if reached {
				s.navigationResult.Store(1)
			} else {
				s.navigationResult.Store(-1)
			}
		}
	})
	fighter.OnAttack(func(_ int32) {
		m.mu.Lock()
		s := m.session
		m.mu.Unlock()
		if s != nil && s.client == c {
			s.attacks.Add(1)
		}
	})
	c.RegisterHandler(func(_ *client.Client, pkt *jp.WirePacket) {
		m.mu.Lock()
		s := m.session
		m.mu.Unlock()
		if s == nil || s.client != c || s.ctx.Err() != nil {
			return
		}
		s.observe(pkt)
	})
}

func (s *actionSession) observe(pkt *jp.WirePacket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch pkt.PacketID {
	case packet_ids.S2CPlayerPositionID:
		var d packets.S2CPlayerPosition
		if pkt.ReadInto(&d) == nil {
			s.positionKnown = true
		}
	case packet_ids.S2COpenScreenID:
		var d packets.S2COpenScreen
		if pkt.ReadInto(&d) == nil {
			s.containerWindow, s.containerReady = int32(d.WindowId), false
		}
	case packet_ids.S2CContainerCloseID:
		var d packets.S2CContainerClose
		if pkt.ReadInto(&d) == nil && int32(d.WindowId) == s.containerWindow {
			s.containerWindow, s.containerReady = 0, false
		}
	case packet_ids.S2CBlockChangedAckID:
		var d packets.S2CBlockChangedAck
		if pkt.ReadInto(&d) == nil {
			s.blockAck = int32(d.SequenceId)
		}
	case packet_ids.S2CContainerSetContentID:
		var d packets.S2CContainerSetContent
		if pkt.ReadInto(&d) == nil {
			if d.WindowId == 0 {
				s.inventoryKnown = true
				for i := range s.slotVersion {
					s.slotVersion[i]++
				}
			} else if int32(d.WindowId) == s.containerWindow {
				s.containerReady = true
			}
		}
	case packet_ids.S2CSetPlayerInventoryID:
		var d packets.S2CSetPlayerInventory
		if pkt.ReadInto(&d) == nil {
			slot := int(d.Slot)
			if slot >= 0 && slot < 9 {
				slot += inventory.SlotHotbarStart
			}
			if slot >= inventory.SlotMainStart && slot < inventory.SlotHotbarEnd {
				s.slotVersion[slot]++
			}
		}
	case packet_ids.S2CEntityEventID:
		var d packets.S2CEntityEvent
		if pkt.ReadInto(&d) == nil && d.EntityStatus == 3 {
			if s.deadEntities == nil || len(s.deadEntities) >= 128 {
				s.deadEntities = make(map[int32]bool)
			}
			s.deadEntities[int32(d.EntityId)] = true
		}
	case packet_ids.S2CContainerSetSlotID:
		var d packets.S2CContainerSetSlot
		if pkt.ReadInto(&d) == nil && (d.WindowId == 0 || d.WindowId == -2) && d.Slot >= 0 && int(d.Slot) < len(s.slotVersion) {
			s.slotVersion[int(d.Slot)]++
		}
	case packet_ids.S2CSetPassengersID:
		var d packets.S2CSetPassengers
		if pkt.ReadInto(&d) != nil {
			return
		}
		if s.vehicle == int32(d.EntityId) {
			s.vehicle = 0
			s.passengerVersion++
		}
		for _, id := range d.Passengers {
			if int32(id) == s.me.EntityID() {
				s.vehicle = int32(d.EntityId)
				s.passengerVersion++
			}
		}
	}
}

func (m *actionManager) close() {
	m.mu.Lock()
	m.closed = true
	if m.session != nil {
		m.session.cancel()
	}
	m.mu.Unlock()
	_ = m.stopCurrent(context.Background(), "action manager closed")
}

// stopCurrent is the synchronous motion barrier for mutating command tools.
// It must not be called from a packet/lifecycle callback; those cancel sessions.
func (m *actionManager) stopCurrent(ctx context.Context, reason string) error {
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	j, s := m.current, m.session
	m.mu.Unlock()
	if j != nil {
		j.releaseItemUse()
		j.cancel()
	}
	m.execMu.Lock()
	if j != nil {
		m.finish(j, "cancelled", reason)
	} else if s != nil {
		s.stopMotion()
	}
	m.execMu.Unlock()
	if j != nil {
		return j.waitItemUseRelease(ctx)
	}
	return nil
}

func (s *actionSession) stopMotion() {
	s.path.Stop()
	s.combat.StopAttacking()
	s.physics.SetInput(0, 0, false)
	s.me.SetSprinting(false)
	s.me.SetSneaking(false)
}

func (m *actionManager) finish(j *actionJob, state, detail string) {
	j.releaseItemUse()
	j.cancel()
	j.session.stopMotion()
	j.status.State = state
	j.status.Detail = detail
	j.status.Attacks = j.session.attacks.Load() - j.attackStart
	now := time.Now()
	j.status.Finished = &now
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.current != j {
		return
	}
	m.history = append(m.history, j.status)
	if len(m.history) > actionHistoryLimit {
		copy(m.history, m.history[len(m.history)-actionHistoryLimit:])
		m.history = m.history[:actionHistoryLimit]
	}
	m.current = nil
	m.published = nil
}

func (m *actionManager) submit(ctx context.Context, actor, kind string, j *actionJob) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if kind == "build" && !m.admin(actor) {
		return nil, fmt.Errorf("construction requires admin rank")
	}
	maxSeconds := 120
	if kind == "use_item" {
		maxSeconds = 130 // The hold is <=120s; allow bounded start/release observation.
	}
	if kind == "move" {
		maxSeconds = maxMoveSeconds
		if j.seconds == 0 {
			j.seconds = 600
		}
	}
	if j.seconds == 0 {
		j.seconds = 60
	}
	if j.seconds < 1 || j.seconds > maxSeconds {
		return nil, fmt.Errorf("seconds must be 1..%d", maxSeconds)
	}
	m.controlMu.Lock()
	defer m.controlMu.Unlock()
	m.mu.Lock()
	s, old := m.session, m.current
	if m.closed || s == nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("actions unavailable: disconnected or physics/gravity modules disabled")
	}
	if old != nil && !strings.EqualFold(old.status.Owner, actor) && !m.admin(actor) {
		m.mu.Unlock()
		return nil, fmt.Errorf("action %d belongs to %s; use stop_action for an emergency stop", old.status.ID, old.status.Owner)
	}
	m.mu.Unlock()
	if !s.physics.Available() {
		return nil, fmt.Errorf("actions unavailable: physics/gravity disabled, no server position, or inactive physics session")
	}
	s.mu.Lock()
	ready, vehicle := s.positionKnown, s.vehicle
	s.mu.Unlock()
	if !ready || s.me.IsDead() {
		return nil, fmt.Errorf("actions unavailable until a living server position is known")
	}
	if vehicle != 0 && kind != "dismount" {
		return nil, fmt.Errorf("dismount before starting another action")
	}
	m.execMu.Lock()
	defer m.execMu.Unlock()
	if old != nil {
		m.finish(old, "cancelled", "replaced by owner or administrator")
	}
	m.mu.Lock()
	if m.session != s || s.ctx.Err() != nil || ctx.Err() != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("session changed while starting action")
	}
	m.nextID++
	j.session = s
	j.ctx, j.cancel = context.WithTimeout(s.ctx, time.Duration(j.seconds)*time.Second)
	j.status = actionStatus{ID: m.nextID, Kind: kind, Owner: actor, State: "queued", Detail: "accepted; not yet completed", Started: time.Now()}
	j.attackStart = s.attacks.Load()
	m.current = j
	snapshot := j.status
	m.published = &snapshot
	m.mu.Unlock()
	go m.watch(j)
	return snapshot, nil
}

func (m *actionManager) watch(j *actionJob) {
	timer := time.NewTicker(200 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case <-j.ctx.Done():
		case <-timer.C:
			if j.session.physics.Available() {
				continue
			}
		}
		m.execMu.Lock()
		m.mu.Lock()
		active := m.current == j
		m.mu.Unlock()
		if active && j.ctx.Err() == nil && j.status.Kind == "dismount" && j.sent {
			done, err := j.mountTick()
			if done {
				m.finish(j, "succeeded", j.status.Detail)
			} else if err != nil {
				m.finish(j, "failed", err.Error())
			} else {
				m.execMu.Unlock()
				continue
			}
			m.execMu.Unlock()
			return
		}
		if active {
			state, detail := "cancelled", "session ended or action cancelled"
			if j.ctx.Err() == context.DeadlineExceeded {
				state, detail = "failed", "action duration limit reached; partial effects were not rolled back"
			}
			if j.ctx.Err() == nil {
				state, detail = "failed", "physics became unavailable; action stopped"
			}
			m.finish(j, state, detail)
		}
		m.execMu.Unlock()
		return
	}
}

func (m *actionManager) tick(c *client.Client) {
	m.execMu.Lock()
	defer m.execMu.Unlock()
	m.mu.Lock()
	j, s := m.current, m.session
	m.mu.Unlock()
	if s == nil || s.client != c {
		return
	}
	s.ticks.Add(1)
	if j == nil || j.session != s {
		return
	}
	if j.ctx.Err() != nil || s.me.IsDead() {
		m.finish(j, "cancelled", "cancelled, dead, or session ended; partial effects were not rolled back")
		return
	}
	if j.status.Kind == "build" && !m.admin(j.status.Owner) {
		m.finish(j, "cancelled", "construction admin authority was revoked; partial effects were not rolled back")
		return
	}
	if !s.physics.Available() && !(j.status.Kind == "dismount" && j.sent) {
		m.finish(j, "failed", "physics session became unavailable")
		return
	}
	if !j.initialized {
		j.origin = actionPosition(s.me)
		j.initialized = true
		j.status.State = "running"
	}
	if !finitePosition(j.origin.X, j.origin.Y, j.origin.Z) || j.status.Kind != "move" && actionPosition(s.me).distance(j.origin) > j.radius() {
		m.finish(j, "failed", "action radius exceeded")
		return
	}
	var done bool
	var err error
	switch j.status.Kind {
	case "move":
		done, err = j.moveTick()
	case "follow":
		done, err = j.followTick(false)
	case "fight":
		done, err = j.followTick(true)
	case "mount", "dismount":
		done, err = j.mountTick()
	case "interact_block", "interact_entity":
		done, err = j.interactTick()
	case "go_to_block":
		done, err = j.goToBlockTick()
	case "use_item":
		done, err = j.useItemTick()
	case "build":
		done, err = j.buildTick()
	}
	if j.ctx.Err() != nil {
		m.finish(j, "cancelled", "cancelled; partial effects were not rolled back")
		return
	}
	if err != nil {
		m.finish(j, "failed", err.Error()+"; partial effects were not rolled back")
		return
	}
	if done {
		state := "succeeded"
		if j.interact != nil && !j.interact.confirmed || j.use != nil && !j.use.confirmed {
			state = "submitted"
		}
		m.finish(j, state, j.status.Detail)
		return
	}
	j.status.Attacks = s.attacks.Load() - j.attackStart
	snapshot := j.status
	m.mu.Lock()
	if m.current == j {
		m.published = &snapshot
	}
	m.mu.Unlock()
}

func (j *actionJob) send(pkt jp.Packet) error {
	return j.session.physics.QueuePacketContext(j.ctx, pkt)
}
func (j *actionJob) checkPoint(p actionPoint) error {
	if !finitePosition(p.X, p.Y, p.Z) || p.Y < -2048 || p.Y > 2047 {
		return fmt.Errorf("target must be finite and within world coordinates")
	}
	if j.status.Kind == "move" {
		return nil
	}
	if p.distance(j.origin) > j.radius() {
		return fmt.Errorf("target must be within %g blocks of action start", j.radius())
	}
	for dy := -1; dy <= 2; dy++ {
		if _, ok := j.session.world.GetLoadedBlock(int(math.Floor(p.X)), int(math.Floor(p.Y))+dy, int(math.Floor(p.Z))); !ok {
			return fmt.Errorf("target terrain is unloaded")
		}
	}
	return nil
}
func (j *actionJob) navigate(p actionPoint) error {
	if err := j.checkPoint(p); err != nil {
		return err
	}
	if err := j.ctx.Err(); err != nil {
		return err
	}
	j.session.path.Stop()
	j.session.navigationResult.Store(0)
	if err := j.session.path.NavigateToContext(j.ctx, p.X, p.Y, p.Z); err != nil {
		return err
	}
	if err := j.ctx.Err(); err != nil {
		j.session.path.Stop()
		return err
	}
	j.navStarted = true
	j.lastGoal = p
	j.lastRepath = j.session.ticks.Load()
	return nil
}
func (j *actionJob) moveTick() (bool, error) {
	if err := j.checkPoint(j.goal); err != nil {
		return false, err
	}
	if !j.navStarted {
		return false, j.navigate(j.goal)
	}
	if j.session.path.IsNavigating() {
		j.status.Detail = "navigating with collision-aware physics"
		return false, nil
	}
	if j.session.navigationResult.Load() != 1 || actionPosition(j.session.me).distance(j.goal) > 1 {
		return false, fmt.Errorf("destination unreachable or navigation stopped before arrival")
	}
	j.status.Detail = "arrived using walking physics; no teleport command used"
	return true, nil
}
func (j *actionJob) targetEntity() (*entities.Entity, error) {
	s := j.session
	if j.player != "" && j.target == 0 {
		if s.players == nil {
			return nil, fmt.Errorf("player list unavailable")
		}
		player := s.players.GetPlayerByName(j.player)
		if player == nil {
			return nil, fmt.Errorf("player is not in the visible player list")
		}
		e := s.entities.GetEntityByUUID(player.UUID)
		if e == nil {
			return nil, fmt.Errorf("player entity is not loaded")
		}
		j.target = e.ID
	}
	e := s.entities.GetEntity(j.target)
	if e == nil {
		return nil, fmt.Errorf("target entity unloaded or removed; disappearance is not proof of a kill")
	}
	if j.targetBound && e.UUID != j.targetUUID {
		return nil, fmt.Errorf("target entity identity changed after unload")
	}
	j.targetBound = true
	j.targetUUID = e.UUID
	if err := j.checkPoint(actionPoint{e.X, e.Y, e.Z}); err != nil {
		return nil, err
	}
	return e, nil
}
func (j *actionJob) followTick(fight bool) (bool, error) {
	s := j.session
	e, err := j.targetEntity()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	dead := s.deadEntities[e.ID]
	s.mu.Unlock()
	if fight && dead {
		j.status.Detail = "target death confirmed by server entity event"
		return true, nil
	}
	p := actionPoint{e.X, e.Y, e.Z}
	distance := actionPosition(s.me).distance(p)
	eye := actionPosition(s.me)
	eye.Y += self.EyeHeight
	visible := j.loadedRay(eye, actionPoint{e.X, e.Y + e.EyeHeight, e.Z}) && s.entities.CanSee(e.ID)
	inRange := distance <= 2.5 && visible
	if fight {
		inRange = s.combat.IsWithinReach(e.ID) && visible
	}
	if inRange {
		s.path.Stop()
		j.navStarted = false
		if fight {
			s.combat.StartAttackingContext(j.ctx, e.ID)
			j.status.Detail = "fighting with held-item cooldown; attacks submitted are not confirmed damage"
		} else {
			j.status.Detail = "following loaded target; holding distance"
		}
		return false, nil
	}
	s.combat.StopAttacking()
	if j.navStarted && !s.path.IsNavigating() && s.navigationResult.Load() < 0 {
		return false, fmt.Errorf("target is unreachable")
	}
	if !j.navStarted || (s.ticks.Load()-j.lastRepath >= 20 && p.distance(j.lastGoal) > 1.5) || !s.path.IsNavigating() {
		// Stand beside an entity rather than treating its occupied cell as a goal.
		goal, err := j.approach(p, 2.4)
		if err != nil {
			return false, err
		}
		if err = j.navigate(goal); err != nil {
			return false, err
		}
	}
	j.status.Detail = "navigating toward loaded target"
	return false, nil
}

func (j *actionJob) approach(target actionPoint, reach float64) (actionPoint, error) {
	s := j.session
	col := collisions.From(s.client)
	sx, sy, sz, ok := pathfinding.FindReachablePosition(col, j.origin.X, j.origin.Y, j.origin.Z, int(math.Floor(target.X)), int(math.Floor(target.Y)), int(math.Floor(target.Z)), reach)
	if !ok {
		return actionPoint{}, fmt.Errorf("no supported, visible approach position within reach")
	}
	y, ok := col.SupportSurfaceY(sx, sy, sz, collisions.PlayerWidth)
	if !ok {
		return actionPoint{}, fmt.Errorf("approach lacks support")
	}
	goal := actionPoint{float64(sx) + 0.5, y, float64(sz) + 0.5}
	if err := j.checkPoint(goal); err != nil {
		return actionPoint{}, err
	}
	return goal, nil
}

func (j *actionJob) mountTick() (bool, error) {
	s := j.session
	s.mu.Lock()
	vehicle, version, inventoryKnown := s.vehicle, s.passengerVersion, s.inventoryKnown
	s.mu.Unlock()
	if j.sent {
		confirmed := version > j.passengerVersion && ((j.status.Kind == "mount" && vehicle == j.target) || (j.status.Kind == "dismount" && vehicle == 0))
		if confirmed {
			s.me.SetSneaking(false)
			j.status.Detail = "passenger state confirmed by server"
			return true, nil
		}
		if j.status.Kind == "mount" {
			if _, err := j.targetEntity(); err != nil {
				return false, err
			}
		}
		if time.Since(j.sentAt) > 5*time.Second {
			return false, fmt.Errorf("server did not confirm passenger state within 5 seconds")
		}
		return false, nil
	}
	if j.status.Kind == "dismount" {
		if vehicle == 0 {
			return false, fmt.Errorf("bot is not known to be mounted")
		}
		s.stopMotion()
		s.me.SetSneaking(true)
		if err := j.send(&packets.C2SPlayerInput{Flags: 32}); err != nil {
			return false, err
		}
	} else {
		e, err := j.targetEntity()
		if err != nil {
			return false, err
		}
		if !mountableType(e.TypeName) {
			return false, fmt.Errorf("mount supports boats and minecarts only; target is %s", e.TypeName)
		}
		if actionPosition(s.me).distance(actionPoint{e.X, e.Y, e.Z}) > 3 || !s.entities.CanSee(e.ID) {
			if j.navStarted && !s.path.IsNavigating() && s.navigationResult.Load() < 0 {
				return false, fmt.Errorf("mount is unreachable")
			}
			if !s.path.IsNavigating() {
				goal, err := j.approach(actionPoint{e.X, e.Y, e.Z}, 2.5)
				if err != nil {
					return false, err
				}
				if err = j.navigate(goal); err != nil {
					return false, err
				}
			}
			return false, nil
		}
		if !inventoryKnown {
			return false, fmt.Errorf("mount requires a server-confirmed inventory")
		}
		s.stopMotion()
		// Empty hand prevents held-item use (feeding, placing, or consuming).
		empty := -1
		for i, item := range s.inventory.GetHotbar() {
			if item.IsEmpty() {
				empty = i
				break
			}
		}
		hand := ns.VarInt(0)
		if empty >= 0 {
			if err := s.inventory.SelectHotbar(empty, j.send); err != nil {
				return false, err
			}
		} else if s.inventory.GetOffhand().IsEmpty() {
			hand = 1
		} else {
			return false, fmt.Errorf("mount requires an empty hotbar slot or offhand")
		}
		if err := j.send(&packets.C2SInteract{EntityId: ns.VarInt(e.ID), Hand: hand, Location: ns.LpVec3{X: 0, Y: e.Height / 2, Z: 0}}); err != nil {
			return false, err
		}
	}
	j.sent = true
	j.sentAt = time.Now()
	j.passengerVersion = version
	return false, nil
}
func mountableType(name string) bool {
	return strings.HasSuffix(name, "_boat") || strings.HasSuffix(name, "_raft") || name == "minecraft:minecart"
}

func (m *actionManager) tools() []toolSpec {
	number := func() map[string]any { return map[string]any{"type": "number"} }
	duration := map[string]any{"type": "integer", "minimum": 1, "maximum": 120}
	targetProps := func() map[string]any {
		return map[string]any{"entity_id": map[string]any{"type": "integer"}, "player": map[string]any{"type": "string", "maxLength": 16}, "seconds": duration}
	}
	return []toolSpec{
		makeTool("move_to", "Start a walking navigation job to absolute coordinates, including beyond render distance. Explores loaded terrain toward the goal and replans as chunks arrive or terrain changes; unknown chunks are never assumed to be air. May open wooden doors; no teleporting, mining or block placement. Default 600 seconds, maximum 3600; action_status reports actual completion.", map[string]any{"x": number(), "y": number(), "z": number(), "seconds": map[string]any{"type": "integer", "minimum": 1, "maximum": maxMoveSeconds}}, []string{"x", "y", "z"}, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			var a struct {
				X       *float64 `json:"x"`
				Y       *float64 `json:"y"`
				Z       *float64 `json:"z"`
				Seconds int      `json:"seconds"`
			}
			if err := decodeArgs(raw, &a); err != nil {
				return nil, err
			}
			if a.X == nil || a.Y == nil || a.Z == nil {
				return nil, fmt.Errorf("x,y,z are required")
			}
			return m.submit(ctx, actor, "move", &actionJob{goal: actionPoint{*a.X, *a.Y, *a.Z}, seconds: a.Seconds})
		}),
		makeTool("follow_entity", "Follow one loaded entity or visible player algorithmically for at most 120 seconds/32-block radius. Target unload terminates the job.", targetProps(), nil, m.targetTool("follow")),
		makeTool("fight_entity", "Autonomously fight one loaded mob or player, selected by entity_id or visible player name, with navigation, line of sight, reach and held-item attack cooldown. Server PvP rules still apply. Attacks are not confirmed damage; removal is not proof of a kill.", targetProps(), nil, m.targetTool("fight")),
		makeTool("mount_entity", "Mount a loaded boat/raft or rideable minecart, with navigation, empty hand and server passenger confirmation.", targetProps(), nil, m.targetTool("mount")),
		makeTool("dismount", "Dismount using real shift input; requires server passenger confirmation.", nil, nil, func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			return m.submit(ctx, actor, "dismount", &actionJob{seconds: 10})
		}),
		makeTool("stop_action", "Emergency stop available to everyone. Cancels the current job, stops movement/combat, and releases held item use (releasing a charged bow can fire). Already confirmed effects are not rolled back.", nil, nil, func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
			if err := decodeArgs(raw, &struct{}{}); err != nil {
				return nil, err
			}
			if err := m.stopCurrent(ctx, "emergency stop requested"); err != nil {
				return nil, err
			}
			return map[string]any{"status": "stopped", "rollback": false}, nil
		}),
		makeTool("action_status", "Read the current physical job and last12 outcomes. Optional wait_seconds waits up to30s for the current job to finish without extra model polling; use it to complete dependent steps such as opening then transferring items. Server text is untrusted. Submitted interactions/attacks/item use are not confirmed effects.", map[string]any{"wait_seconds": map[string]any{"type": "integer", "minimum": 0, "maximum": 30}}, nil, func(ctx context.Context, _ string, raw json.RawMessage) (any, error) {
			var args struct {
				WaitSeconds int `json:"wait_seconds"`
			}
			if err := decodeArgs(raw, &args); err != nil {
				return nil, err
			}
			if args.WaitSeconds < 0 || args.WaitSeconds > 30 {
				return nil, fmt.Errorf("wait_seconds must be 0..30")
			}
			if args.WaitSeconds > 0 {
				m.mu.Lock()
				j := m.current
				m.mu.Unlock()
				if j != nil {
					timer := time.NewTimer(time.Duration(args.WaitSeconds) * time.Second)
					defer timer.Stop()
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-j.ctx.Done():
					case <-timer.C:
					}
					// Finish publishes under execMu after cancelling the job context.
					m.execMu.Lock()
					defer m.execMu.Unlock()
				}
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			return map[string]any{"current": m.published, "history": append([]actionStatus(nil), m.history...)}, nil
		}),
		m.buildTool(),
		m.interactBlockTool(),
		m.interactEntityTool(),
		m.goToBlockTool(),
		m.useItemTool(),
	}
}
func (m *actionManager) targetTool(kind string) func(context.Context, string, json.RawMessage) (any, error) {
	return func(ctx context.Context, actor string, raw json.RawMessage) (any, error) {
		var a struct {
			EntityID *int32 `json:"entity_id"`
			Player   string `json:"player"`
			Seconds  int    `json:"seconds"`
		}
		if err := decodeArgs(raw, &a); err != nil {
			return nil, err
		}
		if (a.EntityID == nil) == (a.Player == "") {
			return nil, fmt.Errorf("supply exactly one of entity_id or player")
		}
		if len(a.Player) > 16 {
			return nil, fmt.Errorf("player name too long")
		}
		j := &actionJob{player: a.Player, seconds: a.Seconds}
		if a.EntityID != nil {
			j.target = *a.EntityID
		}
		return m.submit(ctx, actor, kind, j)
	}
}
