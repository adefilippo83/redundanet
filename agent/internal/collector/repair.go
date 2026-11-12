package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

// The repair scheduler of RFC 0001's "Health" section. Every tick it
// classifies the placement table, turns each object due for repair into a
// signed task for a storage node that volunteers as a worker, hands it
// over the hub channel, and closes the task when the share census shows
// the object placed, when the worker reports, or when the task expires.
// Only one hub schedules at a time: the first in manifest order whose
// status page is fresh. Tasks live in the placement table's SQLite file,
// so a hub restart neither loses nor doubles them.

const (
	scheduleEvery     = time.Minute
	taskBaseExpiry    = time.Hour
	taskBytesPerSec   = 1 << 20 // the uplink a task's expiry allows for
	tasksPerTick      = 20
	tasksPerWorker    = 2
	retryAfterFailure = 10 * time.Minute
	hintEvery         = time.Hour
	defaultFloor      = 10 << 30 // an older node's floor, when its heartbeat does not say
)

// repairState is what the scheduler keeps between ticks.
type repairState struct {
	scheduler string // the hub scheduling repair as this hub sees it; "" before the first election
	hinted    map[[16]byte]time.Time
}

// RecordTaskReport closes a running task on its worker's word; the hub
// calls it. A report from a device the task does not name is ignored.
func (c *Collector) RecordTaskReport(peer identity.Peer, r *wire.TaskReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var worker []byte
	var blob []byte
	err := c.pl.db.QueryRow(`SELECT worker, task FROM tasks WHERE id = ? AND state = 'running'`, r.ID).Scan(&worker, &blob)
	if err != nil {
		return
	}
	var w identity.Key
	copy(w[:], worker)
	if w != peer.Device {
		return
	}
	var t wire.RepairTask
	if err := identity.UnmarshalLenient(blob, &t); err != nil {
		return
	}
	done := map[uint8]bool{}
	for _, n := range r.Done {
		done[n] = true
	}
	all := true
	for _, s := range t.Shares {
		all = all && done[s.Number]
	}
	state, outcome := "done", fmt.Sprintf("the worker stored %d shares", len(r.Done))
	if !all || r.Failed != "" {
		state, outcome = "failed", r.Failed
		if outcome == "" {
			outcome = fmt.Sprintf("the worker stored %d of %d shares", len(r.Done), len(t.Shares))
		}
	}
	_, _ = c.pl.db.Exec(`UPDATE tasks SET state = ?, outcome = ?, ended = ? WHERE id = ?`, state, outcome, c.cfg.Now().Unix(), r.ID)
	c.log.Info("repair task reported", "task", r.ID, "worker", peer.Device.Short(), "state", state, "outcome", outcome)
}

// Schedule runs one tick: election, bookkeeping, new tasks, surplus hints.
func (c *Collector) Schedule(ctx context.Context) {
	now := c.cfg.Now()
	m := c.cfg.Manifest()
	scheduler := c.electScheduler(ctx, m, now)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rep.scheduler = scheduler
	if m == nil {
		return
	}
	if _, err := c.pl.db.Exec(`UPDATE tasks SET state = 'expired', outcome = 'the worker did not finish in time', ended = ? WHERE state = 'running' AND expires <= ?`, now.Unix(), now.Unix()); err != nil {
		c.log.Warn("tasks not expired", "err", err)
	}
	c.closeLostTasks(now)
	alive, servers := c.aliveServers(now, m)
	enc, conv := encodingOfManifest(m)
	objects, _, err := c.pl.compute(now, alive, servers, enc, conv)
	if err != nil {
		c.log.Warn("placement not computed", "err", err)
		return
	}
	running, err := c.runningTasks()
	if err != nil {
		c.log.Warn("tasks not read", "err", err)
		return
	}
	bySI := map[[16]byte]Object{}
	for _, o := range objects {
		bySI[o.StorageIndex] = o
	}
	for _, t := range running {
		o, ok := bySI[t.StorageIndex]
		if !ok || o.Placed || o.Tombstoned {
			_, _ = c.pl.db.Exec(`UPDATE tasks SET state = 'done', outcome = 'the census shows the object placed', ended = ? WHERE id = ?`, now.Unix(), t.ID)
		}
	}
	if scheduler != c.hubName(m) {
		return
	}
	in := c.planInput(now, m, objects, servers)
	out := plan(in)
	issued := 0
	for _, p := range out.tasks {
		if issued == tasksPerTick {
			break
		}
		t := &wire.RepairTask{Network: m.Network.Name, StorageIndex: p.obj.StorageIndex, Owner: p.obj.Owner, DescriptorHash: p.obj.Hash,
			Worker: p.worker, Shares: p.shares, Issued: now, Expires: now.Add(taskBaseExpiry + time.Duration(p.bytes/taskBytesPerSec)*time.Second)}
		for dev := range p.obj.Holders {
			t.Holders = append(t.Holders, dev)
		}
		sort.Slice(t.Holders, func(i, j int) bool { return t.Holders[i].Less(t.Holders[j]) })
		res, err := c.pl.db.Exec(`INSERT INTO tasks (si, worker, issued, expires, bytes, task, state) VALUES (?, ?, ?, ?, ?, ?, 'running')`,
			t.StorageIndex[:], t.Worker[:], t.Issued.Unix(), t.Expires.Unix(), p.bytes, []byte{})
		if err != nil {
			c.log.Warn("task not recorded", "err", err)
			continue
		}
		id, _ := res.LastInsertId()
		t.ID = uint64(id)
		if err := t.Sign(c.cfg.Signer); err != nil {
			c.log.Warn("task not signed", "err", err)
			_, _ = c.pl.db.Exec(`DELETE FROM tasks WHERE id = ?`, id)
			continue
		}
		blob, _ := identity.Marshal(t)
		if _, err := c.pl.db.Exec(`UPDATE tasks SET task = ? WHERE id = ?`, blob, id); err != nil {
			c.log.Warn("task not recorded", "err", err)
			continue
		}
		if c.send == nil {
			_, _ = c.pl.db.Exec(`UPDATE tasks SET state = 'failed', outcome = 'no channel to the worker', ended = ? WHERE id = ?`, now.Unix(), id)
			continue
		}
		if err := c.send(t.Worker, wire.FrameTask, t); err != nil {
			_, _ = c.pl.db.Exec(`UPDATE tasks SET state = 'failed', outcome = ?, ended = ? WHERE id = ?`, "not sent: "+err.Error(), now.Unix(), id)
			c.log.Info("repair task not sent", "task", id, "worker", t.Worker.Short(), "err", err)
			continue
		}
		issued++
		c.log.Info("repair task issued", "task", id, "si", short(objectID(t.StorageIndex)), "worker", t.Worker.Short(), "shares", len(t.Shares), "expires", t.Expires.UTC().Format(time.RFC3339))
	}
	c.hintSurplus(now, objects, in)
}

func objectID(si [16]byte) string {
	var b strings.Builder
	for _, x := range si[:4] {
		fmt.Fprintf(&b, "%02x", x)
	}
	return b.String()
}

// lostTaskOutcome is recorded for a task closed by closeLostTasks.
const lostTaskOutcome = "the worker's connection dropped before it reported"

// closeLostTasks closes the running tasks whose worker cannot have them
// any more as far as this hub can tell: a task is handed over on the
// worker's channel, so a worker with no channel, or with one that began
// after the task was issued, most likely never got it. Waiting for the
// expiry would leave the object alone for an hour; closed, it is offered
// again after retryAfterFailure. A worker that did get the task and
// finishes it across a reconnect stores a share another worker may store
// too, which the surplus release takes care of; its report then finds no
// running task and is ignored. The caller holds c.mu.
func (c *Collector) closeLostTasks(now time.Time) {
	live := c.cfg.Live
	if live == nil {
		return
	}
	running, err := c.runningTasks()
	if err != nil || len(running) == 0 {
		return
	}
	since := map[string]time.Time{}
	for _, cl := range live().Clients {
		since[cl.Device] = cl.Since
	}
	for _, t := range running {
		began, connected := since[t.Worker.String()]
		// Whole seconds, as the table keeps them: a channel that began in
		// the second the task was issued may well have carried it.
		if connected && began.Unix() <= t.Issued.Unix() {
			continue
		}
		if _, err := c.pl.db.Exec(`UPDATE tasks SET state = 'expired', outcome = ?, ended = ? WHERE id = ? AND state = 'running'`, lostTaskOutcome, now.Unix(), t.ID); err != nil {
			c.log.Warn("lost task not closed", "task", t.ID, "err", err)
			continue
		}
		c.log.Info("repair task closed: "+lostTaskOutcome, "task", t.ID, "worker", t.Worker.Short(), "connected", connected)
	}
}

// electScheduler is the one-scheduler rule: the first hub in manifest
// order whose status page was generated within the down threshold
// schedules; this hub does when every hub before it is gone, or when it
// is first. Without a signing key this hub never schedules.
func (c *Collector) electScheduler(ctx context.Context, m *manifest.Manifest, now time.Time) string {
	if m == nil || c.cfg.Signer == nil {
		return ""
	}
	me := -1
	for i, h := range m.Hubs {
		if h.Key == c.cfg.Device {
			me = i
		}
	}
	if me < 0 {
		return ""
	}
	for _, h := range m.Hubs[:me] {
		if h.Status == "" {
			continue
		}
		url := strings.TrimSuffix(h.Status, "/") + "/status.json"
		fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		data, err := c.cfg.Fetch(fctx, url)
		cancel()
		if err != nil {
			continue
		}
		var st struct {
			GeneratedAt time.Time `json:"generated_at"`
		}
		if json.Unmarshal(data, &st) != nil || st.GeneratedAt.IsZero() {
			continue
		}
		if age := now.Sub(st.GeneratedAt); age >= -downAfter && age <= downAfter {
			return h.Name
		}
	}
	return m.Hubs[me].Name
}

func (c *Collector) hubName(m *manifest.Manifest) string {
	if h := m.HubByKey(c.cfg.Device); h != nil {
		return h.Name
	}
	return ""
}

// runningTask is a task in flight as the table records it.
type runningTask struct {
	ID           uint64
	StorageIndex [16]byte
	Worker       identity.Key
	Issued       time.Time
	Shares       []wire.RepairShare
	Bytes        int64
}

func (c *Collector) runningTasks() ([]runningTask, error) {
	rows, err := c.pl.db.Query(`SELECT id, si, worker, issued, bytes, task FROM tasks WHERE state = 'running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []runningTask
	for rows.Next() {
		var t runningTask
		var si, worker, blob []byte
		var issued int64
		if err := rows.Scan(&t.ID, &si, &worker, &issued, &t.Bytes, &blob); err != nil {
			return nil, err
		}
		t.Issued = time.Unix(issued, 0)
		copy(t.StorageIndex[:], si)
		copy(t.Worker[:], worker)
		var task wire.RepairTask
		if len(blob) > 0 && identity.UnmarshalLenient(blob, &task) == nil {
			t.Shares = task.Shares
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// taskCounts is the page's view of the tasks table.
func (c *Collector) taskCounts(now time.Time) (running, done, failed int) {
	since := now.Add(-24 * time.Hour).Unix()
	_ = c.pl.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE state = 'running'`).Scan(&running)
	_ = c.pl.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE state = 'done' AND ended >= ?`, since).Scan(&done)
	_ = c.pl.db.QueryRow(`SELECT COUNT(*) FROM tasks WHERE state IN ('failed', 'expired') AND ended >= ?`, since).Scan(&failed)
	return
}

// ---- the planner, pure ----

// target is a live storage server with room above its floor.
type target struct {
	dev  identity.Key
	room int64
}

type planInput struct {
	now       time.Time
	objects   []Object
	servers   int // storage servers not dead, the doubling rule's denominator
	targets   map[identity.Key]*target
	workers   []identity.Key // live, volunteering, in key order
	running   map[[16]byte]bool
	perWorker map[identity.Key]int
	notBefore map[[16]byte]time.Time // objects whose last task failed recently
}

type plannedTask struct {
	obj    Object
	shares []wire.RepairShare
	worker identity.Key
	bytes  int64
}

type planOutput struct {
	tasks   []plannedTask
	blocked int   // objects due with no server to place on
	short   int64 // bytes the servers lack above their floors for them
}

// planInput gathers what the planner needs from the collector's state;
// called with the lock.
func (c *Collector) planInput(now time.Time, m *manifest.Manifest, objects []Object, servers int) planInput {
	in := planInput{now: now, objects: objects, servers: servers, targets: map[identity.Key]*target{}, running: map[[16]byte]bool{}, perWorker: map[identity.Key]int{}, notBefore: map[[16]byte]time.Time{}}
	live := map[identity.Key]bool{}
	if c.cfg.Live != nil {
		for _, cl := range c.cfg.Live().Clients {
			if k, err := identity.ParseKey(cl.Device); err == nil {
				live[k] = true
			}
		}
	}
	for _, s := range m.Storage {
		e, ok := c.entries[s.Device]
		if !ok || !live[s.Device] || e.Census.Storage == nil || e.Census.Storage.State != "ok" {
			continue
		}
		floor := int64(e.Census.Storage.Floor)
		if floor == 0 {
			floor = defaultFloor
		}
		in.targets[s.Device] = &target{dev: s.Device, room: int64(e.Census.Storage.FreeBytes) - floor}
		if e.Census.Worker {
			in.workers = append(in.workers, s.Device)
		}
	}
	sort.Slice(in.workers, func(i, j int) bool { return in.workers[i].Less(in.workers[j]) })
	running, _ := c.runningTasks()
	for _, t := range running {
		in.running[t.StorageIndex] = true
		in.perWorker[t.Worker]++
		for _, s := range t.Shares {
			if tg := in.targets[s.Server]; tg != nil {
				tg.room -= t.Bytes / int64(max(len(t.Shares), 1))
			}
		}
	}
	rows, err := c.pl.db.Query(`SELECT si, MAX(ended) FROM tasks WHERE state IN ('failed', 'expired') AND ended >= ? GROUP BY si`, now.Add(-retryAfterFailure).Unix())
	if err == nil {
		for rows.Next() {
			var si []byte
			var ended int64
			if rows.Scan(&si, &ended) == nil {
				var key [16]byte
				copy(key[:], si)
				in.notBefore[key] = time.Unix(ended, 0).Add(retryAfterFailure)
			}
		}
		rows.Close()
	}
	return in
}

// plan decides, for every object due, which share numbers go where and
// who does the work. Rules: a missing number lands on a live server with
// room that does not hold it, servers holding nothing of the object
// first, at most ceil(n / servers not dead) per server; then the spread
// is raised to the target by copying a number from the fullest holder to
// an empty server, and the surplus rule retires the copy later; never a
// plan that leaves the object on fewer than k servers. The worker is a
// volunteering live node, one not among the targets when there is one,
// with fewer than tasksPerWorker tasks running.
func plan(in planInput) planOutput {
	var out planOutput
	for _, o := range in.objects {
		if o.Need != "repair" || o.Tombstoned || !o.Readable || in.running[o.StorageIndex] {
			continue
		}
		if nb, ok := in.notBefore[o.StorageIndex]; ok && in.now.Before(nb) {
			continue
		}
		room := map[identity.Key]int64{}
		count := map[identity.Key]int{}
		holds := map[identity.Key]map[uint8]bool{}
		for dev, t := range in.targets {
			room[dev] = t.room
		}
		for dev, shares := range o.Holders {
			count[dev] = len(shares)
			holds[dev] = map[uint8]bool{}
			for _, n := range shares {
				holds[dev][n] = true
			}
		}
		maxPer := (o.N + max(in.servers, 1) - 1) / max(in.servers, 1)
		var shares []wire.RepairShare
		blocked, short := false, int64(0)
		// pick chooses the server for a number; lackRoom says a server
		// would have done but for its floor.
		pick := func(n uint8, emptyOnly bool) (best identity.Key, found, lackRoom bool) {
			for dev := range in.targets {
				if holds[dev][n] || count[dev] >= maxPer || (emptyOnly && count[dev] > 0) {
					continue
				}
				if room[dev] < o.Size {
					lackRoom = true
					continue
				}
				if !found || better(dev, count, room, best) {
					best, found = dev, true
				}
			}
			return best, found, lackRoom
		}
		take := func(n uint8, dev identity.Key) {
			shares = append(shares, wire.RepairShare{Number: n, Server: dev})
			room[dev] -= o.Size
			count[dev]++
			if holds[dev] == nil {
				holds[dev] = map[uint8]bool{}
			}
			holds[dev][n] = true
		}
		for _, n := range o.Missing {
			dev, ok, lack := pick(n, true)
			if !ok {
				var lack2 bool
				dev, ok, lack2 = pick(n, false)
				lack = lack || lack2
			}
			if !ok {
				blocked = true
				if lack {
					short += o.Size
				}
				continue
			}
			take(n, dev)
		}
		// Spread: copy from the fullest holder to an empty server, once
		// every number has a home.
		for !blocked && len(holds) < o.Target {
			var from identity.Key
			for dev := range holds {
				if count[dev] > count[from] || (count[dev] == count[from] && from.Less(dev)) {
					from = dev
				}
			}
			var n uint8
			ok := false
			for x := 0; x < o.N; x++ {
				if holds[from][uint8(x)] && !duplicated(uint8(x), holds) {
					n, ok = uint8(x), true
					break
				}
			}
			if !ok {
				break
			}
			dev, found, lack := pick(n, true)
			if !found {
				blocked = true
				if lack {
					short += o.Size
				}
				break
			}
			take(n, dev)
		}
		if len(holds) < o.K {
			blocked = true // never on fewer than k servers
			shares = nil
		}
		if len(shares) == 0 {
			if blocked {
				out.blocked++
				out.short += short
			}
			continue
		}
		worker, ok := chooseWorker(in, shares)
		if !ok {
			continue // every worker is busy; next tick
		}
		in.perWorker[worker]++
		for _, s := range shares {
			if t := in.targets[s.Server]; t != nil {
				t.room -= o.Size
			}
		}
		out.tasks = append(out.tasks, plannedTask{obj: o, shares: shares, worker: worker, bytes: o.Size * int64(len(shares))})
	}
	return out
}

func better(dev identity.Key, count map[identity.Key]int, room map[identity.Key]int64, best identity.Key) bool {
	switch {
	case count[dev] != count[best]:
		return count[dev] < count[best]
	case room[dev] != room[best]:
		return room[dev] > room[best]
	}
	return dev.Less(best)
}

func duplicated(n uint8, holds map[identity.Key]map[uint8]bool) bool {
	k := 0
	for _, h := range holds {
		if h[n] {
			k++
		}
	}
	return k > 1
}

func chooseWorker(in planInput, shares []wire.RepairShare) (identity.Key, bool) {
	isTarget := map[identity.Key]bool{}
	for _, s := range shares {
		isTarget[s.Server] = true
	}
	var best identity.Key
	found := false
	for _, w := range in.workers {
		if in.perWorker[w] >= tasksPerWorker {
			continue
		}
		if !found || (isTarget[best] && !isTarget[w]) || (isTarget[best] == isTarget[w] && in.perWorker[w] < in.perWorker[best]) {
			best, found = w, true
		}
	}
	return best, found
}

// hintSurplus tells one holder per object with surplus which copy to give
// up: the holder with the most shares of the object, as long as the spread
// stays at the target. The holder confirms with the others before it
// removes anything. Called with the lock.
func (c *Collector) hintSurplus(now time.Time, objects []Object, in planInput) {
	if c.send == nil {
		return
	}
	for _, o := range objects {
		if o.Surplus == 0 || !o.Placed || in.running[o.StorageIndex] {
			continue
		}
		if t, ok := c.rep.hinted[o.StorageIndex]; ok && now.Sub(t) < hintEvery {
			continue
		}
		holders := map[uint8][]identity.Key{}
		for dev, shares := range o.Holders {
			for _, n := range shares {
				holders[n] = append(holders[n], dev)
			}
		}
		var hint *wire.ReleaseHint
		var to identity.Key
		for n := 0; n < o.N && hint == nil; n++ {
			devs := holders[uint8(n)]
			if len(devs) < 2 {
				continue
			}
			var pickDev identity.Key
			found := false
			for _, dev := range devs {
				if _, live := in.targets[dev]; !live {
					continue
				}
				if len(o.Holders[dev]) == 1 && o.Spread-1 < o.Target {
					continue
				}
				if !found || len(o.Holders[dev]) > len(o.Holders[pickDev]) || (len(o.Holders[dev]) == len(o.Holders[pickDev]) && pickDev.Less(dev)) {
					pickDev, found = dev, true
				}
			}
			if !found {
				continue
			}
			h := &wire.ReleaseHint{StorageIndex: o.StorageIndex, Number: uint8(n), DescriptorHash: o.Hash}
			for dev := range o.Holders {
				if dev != pickDev {
					h.Holders = append(h.Holders, dev)
				}
			}
			sort.Slice(h.Holders, func(i, j int) bool { return h.Holders[i].Less(h.Holders[j]) })
			hint, to = h, pickDev
		}
		if hint == nil {
			continue
		}
		c.rep.hinted[o.StorageIndex] = now
		if err := c.send(to, wire.FrameRelease, hint); err == nil {
			c.log.Info("surplus release hinted", "si", objectID(o.StorageIndex), "n", hint.Number, "holder", to.Short())
		}
	}
}
