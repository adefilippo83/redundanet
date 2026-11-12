package collector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/adefilippo83/redundanet/agent/internal/hub"
	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/manifest"
	"github.com/adefilippo83/redundanet/agent/internal/wire"
)

var third = key(14)

// sent is one frame the collector handed the hub for a device.
type sent struct {
	to  identity.Key
	typ byte
	v   any
}

// schedulerWorld is a collector with a signing key, three storage servers
// listed, and the hub's channel writer captured.
type schedulerWorld struct {
	*world
	signer *identity.DeviceKey
	m      *manifest.Manifest
	mu     sync.Mutex
	sent   []sent
}

func newSchedulerWorld(t *testing.T, hubs []manifest.Hub, fetch func(ctx context.Context, url string) ([]byte, error)) *schedulerWorld {
	t.Helper()
	signer, err := identity.NewDeviceKey()
	if err != nil {
		t.Fatal(err)
	}
	for i := range hubs {
		if hubs[i].Name == "hub" {
			hubs[i].Key = signer.ID()
		}
	}
	five := 5
	m := &manifest.Manifest{Revision: 7, Network: manifest.Network{Name: "demo", RepairAfterHours: 72, Encoding: manifest.Encoding{K: 2, N: 4}}, Hubs: hubs,
		Members: []manifest.Member{{Name: "alice", Key: alice, ContributionGB: 100, Status: "active"}, {Name: "bob", Key: bob, AllocationGB: &five, Status: "active"}},
		Storage: []manifest.StorageDevice{{Member: "alice", Device: falcon, Name: "falcon"}, {Member: "alice", Device: spare, Name: "spare"}, {Member: "bob", Device: third, Name: "third"}}}
	w := &world{now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	sw := &schedulerWorld{world: w, signer: signer, m: m}
	c, err := New(Config{Device: signer.ID(), Signer: signer, Manifest: func() *manifest.Manifest { return m }, Fetch: fetch,
		Started: w.now.Add(-time.Hour), Now: func() time.Time { w.mu.Lock(); defer w.mu.Unlock(); return w.now }})
	if err != nil {
		t.Fatal(err)
	}
	c.SetLive(func() hub.Status { w.mu.Lock(); defer w.mu.Unlock(); return w.live })
	c.SetSend(func(to identity.Key, typ byte, v any) error {
		sw.mu.Lock()
		defer sw.mu.Unlock()
		sw.sent = append(sw.sent, sent{to, typ, v})
		return nil
	})
	w.c = c
	t.Cleanup(func() { _ = c.Close() })
	return sw
}

// online puts storage servers on a live channel with room above the floor.
func (sw *schedulerWorld) online(free uint64, devs ...identity.Key) {
	var st hub.Status
	for _, d := range devs {
		member := alice
		if d == third {
			member = bob
		}
		st.Clients = append(st.Clients, hub.ClientStatus{Device: d.String(), Member: member.String()})
		sw.c.Record(identity.Peer{Device: d, Member: member}, "udp", wire.Census{Roles: []string{identity.RoleStorage}, Worker: true,
			Storage: &wire.CensusStorage{FreeBytes: free, TotalBytes: 200 << 30, State: "ok", Floor: 10 << 30}})
	}
	sw.setLive(st)
}

func (sw *schedulerWorld) tasks() []*wire.RepairTask {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	var out []*wire.RepairTask
	for _, s := range sw.sent {
		if s.typ == wire.FrameTask {
			out = append(out, s.v.(*wire.RepairTask))
		}
	}
	return out
}

func (sw *schedulerWorld) hints() []sent {
	sw.mu.Lock()
	defer sw.mu.Unlock()
	var out []sent
	for _, s := range sw.sent {
		if s.typ == wire.FrameRelease {
			out = append(out, s)
		}
	}
	return out
}

func TestSchedulerIssuesSignedTasks(t *testing.T) {
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	sw.online(100<<30, falcon, spare, third)
	// 1 is placed over the three servers; 2 lacks number 3.
	sw.report(falcon, 1, share(1, 0, alice), share(1, 1, alice), share(2, 0, alice), share(2, 1, alice))
	sw.report(spare, 1, share(1, 2, alice), share(2, 2, alice))
	sw.report(third, 1, share(1, 3, alice))
	ctx := context.Background()
	sw.c.Schedule(ctx)
	if got := sw.tasks(); len(got) != 0 {
		t.Fatalf("a task during the quiescence window: %+v", got[0])
	}
	if st := sw.c.Status(); st.Grid.Scheduler != "hub" || st.Grid.RepairRunning != 0 {
		t.Fatalf("status: %+v", st.Grid)
	}
	sw.advance(25 * time.Hour)
	sw.online(100<<30, falcon, spare, third)
	sw.c.Schedule(ctx)
	got := sw.tasks()
	if len(got) != 1 {
		t.Fatalf("tasks after the window: %d", len(got))
	}
	tk := got[0]
	if err := tk.Verify("demo", sw.now); err != nil {
		t.Fatalf("the task does not verify: %v", err)
	}
	// Number 3 goes to the server holding nothing of the object; the
	// worker is a volunteer that is not a target; the holders are the hint.
	if tk.Hub != sw.signer.ID() || tk.StorageIndex[0] != 2 || tk.Owner != alice || len(tk.Shares) != 1 || tk.Shares[0].Number != 3 || tk.Shares[0].Server != third ||
		tk.Worker != falcon || len(tk.Holders) != 2 || !tk.Expires.After(sw.now.Add(59*time.Minute)) || tk.ID == 0 {
		t.Fatalf("task: %+v", tk)
	}
	sw.mu.Lock()
	to := sw.sent[0].to
	sw.mu.Unlock()
	if to != falcon {
		t.Fatalf("sent to %s", to.Short())
	}
	st := sw.c.Status()
	if st.Grid.RepairRunning != 1 || st.Grid.Repair != 1 {
		t.Fatalf("grid with a task running: %+v", st.Grid)
	}
	// Running: no second task for the same object.
	sw.c.Schedule(ctx)
	if len(sw.tasks()) != 1 {
		t.Fatal("a second task for an object under repair")
	}
	// A report from a device the task does not name is ignored; the
	// worker's report closes it.
	sw.c.RecordTaskReport(identity.Peer{Device: spare, Member: alice}, &wire.TaskReport{ID: tk.ID, Done: []uint8{3}})
	if st := sw.c.Status(); st.Grid.RepairRunning != 1 {
		t.Fatalf("a stranger's report closed the task: %+v", st.Grid)
	}
	sw.c.RecordTaskReport(identity.Peer{Device: falcon, Member: alice}, &wire.TaskReport{ID: tk.ID, Done: []uint8{3}})
	if st := sw.c.Status(); st.Grid.RepairRunning != 0 || st.Grid.RepairDone != 1 {
		t.Fatalf("after the report: %+v", st.Grid)
	}
	// The census then shows the share; the object is placed, nothing due.
	sw.c.RecordShares(identity.Peer{Device: third, Member: bob}, &wire.ShareCensus{Since: 1, Generation: 2, Added: []wire.ShareEntry{share(2, 3, alice)}})
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.Repair != 0 || st.Grid.Placed != 2 || len(sw.tasks()) != 1 {
		t.Fatalf("after the census: %+v, %d tasks", st.Grid, len(sw.tasks()))
	}
	// The page says it.
	st = sw.c.Status()
	page, err := sw.c.render(st)
	if err != nil || !strings.Contains(string(page), "repaired in 24 h") || !strings.Contains(string(page), "scheduler") {
		t.Fatalf("page: %v\n%s", err, page)
	}
}

func TestSchedulerExpiresAndRetries(t *testing.T) {
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	sw.online(100<<30, falcon, spare, third)
	sw.report(falcon, 1, share(3, 0, alice), share(3, 1, alice))
	sw.advance(25 * time.Hour)
	sw.online(100<<30, falcon, spare, third)
	ctx := context.Background()
	sw.c.Schedule(ctx)
	got := sw.tasks()
	if len(got) != 1 || len(got[0].Shares) != 2 {
		t.Fatalf("tasks: %+v", got)
	}
	// Two missing numbers land on the two empty servers, one each.
	if a, b := got[0].Shares[0].Server, got[0].Shares[1].Server; a == b || a == falcon || b == falcon {
		t.Fatalf("placement: %+v", got[0].Shares)
	}
	// The worker never finishes: the task expires, and the object waits
	// before it is tried again.
	sw.advance(3 * time.Hour)
	sw.online(100<<30, falcon, spare, third)
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.RepairRunning != 0 || st.Grid.RepairFailed != 1 || len(sw.tasks()) != 1 {
		t.Fatalf("after expiry: %+v, %d tasks", st.Grid, len(sw.tasks()))
	}
	sw.advance(11 * time.Minute)
	sw.online(100<<30, falcon, spare, third)
	sw.c.Schedule(ctx)
	if len(sw.tasks()) != 2 {
		t.Fatalf("not retried after the wait: %d tasks", len(sw.tasks()))
	}
	// A worker's failure closes the task the same way.
	sw.c.RecordTaskReport(identity.Peer{Device: falcon, Member: alice}, &wire.TaskReport{ID: sw.tasks()[1].ID, Failed: "no holder could give the descriptor"})
	if st := sw.c.Status(); st.Grid.RepairRunning != 0 || st.Grid.RepairFailed != 2 {
		t.Fatalf("after a failure: %+v", st.Grid)
	}
}

func TestSchedulerBlocksWithoutRoomOrServers(t *testing.T) {
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	sw.online(20<<30, falcon, spare, third) // 10 GB above the floor each
	big := share(4, 0, alice)
	big.Size = 50 << 30
	big2 := big
	big2.Number = 1
	sw.report(falcon, 1, big, big2)
	sw.advance(25 * time.Hour)
	sw.online(20<<30, falcon, spare, third)
	ctx := context.Background()
	sw.c.Schedule(ctx)
	st := sw.c.Status()
	if len(sw.tasks()) != 0 || st.Grid.RepairBlocked != 1 || st.Grid.RepairShort != 2*(50<<30) {
		t.Fatalf("blocked for room: %d tasks, %+v", len(sw.tasks()), st.Grid)
	}
	page, _ := sw.c.render(st)
	if !strings.Contains(string(page), "1 blocked: 100 GB short above the floors") {
		t.Fatalf("page:\n%s", page)
	}
	// Room appears: the task goes out.
	sw.online(100<<30, falcon, spare, third)
	sw.c.Schedule(ctx)
	if len(sw.tasks()) != 1 {
		t.Fatalf("after room appeared: %d tasks", len(sw.tasks()))
	}

	// Fewer than k servers alive: never doubled up on one, so blocked.
	sw2 := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	sw2.online(100<<30, falcon)
	sw2.report(falcon, 1, share(5, 0, alice), share(5, 1, alice))
	sw2.advance(25 * time.Hour)
	sw2.online(100<<30, falcon)
	sw2.c.Schedule(ctx)
	if st := sw2.c.Status(); len(sw2.tasks()) != 0 || st.Grid.RepairBlocked != 1 || st.Grid.RepairShort != 0 {
		t.Fatalf("one server alive: %d tasks, %+v", len(sw2.tasks()), st.Grid)
	}
}

func TestSchedulerRaisesSpreadAndHintsSurplus(t *testing.T) {
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	sw.online(100<<30, falcon, spare, third)
	// 6: every number on falcon, target spread 3. 7: placed over the
	// three servers with number 0 held twice; spare, holding as many as
	// falcon, is the later key and gives it up.
	sw.report(falcon, 1, share(6, 0, alice), share(6, 1, alice), share(6, 2, alice), share(6, 3, alice), share(7, 0, alice), share(7, 1, alice))
	sw.report(spare, 1, share(7, 0, alice), share(7, 2, alice))
	sw.report(third, 1, share(7, 3, alice))
	sw.advance(25 * time.Hour)
	sw.online(100<<30, falcon, spare, third)
	ctx := context.Background()
	sw.c.Schedule(ctx)
	got := sw.tasks()
	if len(got) != 1 || got[0].StorageIndex[0] != 6 || len(got[0].Shares) != 2 || got[0].Worker != falcon {
		t.Fatalf("spread task: %+v", got)
	}
	targets := map[identity.Key]bool{}
	for _, s := range got[0].Shares {
		targets[s.Server] = true
	}
	if !targets[spare] || !targets[third] {
		t.Fatalf("spread copies: %+v", got[0].Shares)
	}
	hints := sw.hints()
	if len(hints) != 1 || hints[0].to != spare {
		t.Fatalf("hints: %+v", hints)
	}
	h := hints[0].v.(*wire.ReleaseHint)
	if h.StorageIndex[0] != 7 || h.Number != 0 || len(h.Holders) != 2 || h.Holders[0] != falcon || h.Holders[1] != third || h.DescriptorHash[0] != 7 {
		t.Fatalf("hint: %+v", h)
	}
	// Not hinted again within the hour.
	sw.c.Schedule(ctx)
	if len(sw.hints()) != 1 {
		t.Fatal("hinted twice")
	}
}

func TestOneSchedulerAtATime(t *testing.T) {
	var mu sync.Mutex
	otherUp := true
	var now time.Time
	fetch := func(ctx context.Context, url string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasSuffix(url, "/status.json") {
			return nil, errors.New("wrong url " + url)
		}
		if !otherUp {
			return nil, errors.New("connection refused")
		}
		return json.Marshal(map[string]any{"generated_at": now.Add(-2 * time.Minute)})
	}
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "first", Key: key(21), Status: "https://first.example/v3/"}, {Name: "hub"}}, fetch)
	now = sw.now
	sw.online(100<<30, falcon, spare, third)
	sw.report(falcon, 1, share(8, 0, alice), share(8, 1, alice))
	sw.advance(25 * time.Hour)
	mu.Lock()
	now = sw.now
	mu.Unlock()
	sw.online(100<<30, falcon, spare, third)
	ctx := context.Background()
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.Scheduler != "first" || len(sw.tasks()) != 0 {
		t.Fatalf("with the first hub up: %q, %d tasks", st.Grid.Scheduler, len(sw.tasks()))
	}
	// The first hub goes quiet: this one takes over.
	mu.Lock()
	otherUp = false
	mu.Unlock()
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.Scheduler != "hub" || len(sw.tasks()) != 1 {
		t.Fatalf("with the first hub gone: %q, %d tasks", st.Grid.Scheduler, len(sw.tasks()))
	}
	// Without a signing key a hub never schedules.
	w := newWorld(t, "", "")
	w.c.Schedule(ctx)
	if st := w.c.Status(); st.Grid.Scheduler != "" {
		t.Fatalf("unsigned hub schedules: %q", st.Grid.Scheduler)
	}
}

// A task is handed over on the worker's channel. When that channel began
// after the task was issued, or there is none, the task is closed at the
// next tick and its object is offered again after the usual wait, not
// after the task's hour.
func TestATaskItsWorkerCannotHaveIsClosedEarly(t *testing.T) {
	sw := newSchedulerWorld(t, []manifest.Hub{{Name: "hub"}}, nil)
	ctx := context.Background()
	// connected puts the three servers on the hub, the worker on a channel
	// that began when workerSince says.
	connected := func(workerSince time.Time, withWorker bool) {
		sw.online(100<<30, falcon, spare, third)
		var st hub.Status
		for _, d := range []identity.Key{falcon, spare, third} {
			cl := hub.ClientStatus{Device: d.String(), Member: alice.String(), Since: sw.now.Add(-48 * time.Hour)}
			if d == falcon {
				if !withWorker {
					continue
				}
				cl.Since = workerSince
			}
			st.Clients = append(st.Clients, cl)
		}
		sw.setLive(st)
	}
	sw.online(100<<30, falcon, spare, third)
	sw.report(falcon, 1, share(2, 0, alice), share(2, 1, alice))
	sw.report(spare, 1, share(2, 2, alice))
	sw.advance(25 * time.Hour)
	longAgo := sw.now.Add(-48 * time.Hour)
	connected(longAgo, true)
	sw.c.Schedule(ctx)
	if got := sw.tasks(); len(got) != 1 || got[0].Worker != falcon {
		t.Fatalf("tasks after the window: %+v", got)
	}

	// The worker stays on the channel it had: the task runs on.
	sw.advance(time.Minute)
	connected(longAgo, true)
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.RepairRunning != 1 || st.Grid.RepairFailed != 0 {
		t.Fatalf("a task whose worker stayed connected: %+v", st.Grid)
	}

	// The worker's channel is a new one, begun after the task was issued.
	sw.advance(time.Minute)
	connected(sw.now.Add(-30*time.Second), true)
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.RepairRunning != 0 || st.Grid.RepairFailed != 1 {
		t.Fatalf("a task issued before its worker's channel began: %+v", st.Grid)
	}
	if len(sw.tasks()) != 1 {
		t.Fatal("the object was offered again at once")
	}
	// A late report for the closed task changes nothing.
	sw.c.RecordTaskReport(identity.Peer{Device: falcon, Member: alice}, &wire.TaskReport{ID: sw.tasks()[0].ID, Done: []uint8{3}})
	if st := sw.c.Status(); st.Grid.RepairDone != 0 || st.Grid.RepairFailed != 1 {
		t.Fatalf("a report for a closed task: %+v", st.Grid)
	}

	// After the wait the object is offered again.
	sw.advance(retryAfterFailure + time.Minute)
	connected(sw.now.Add(-retryAfterFailure), true)
	sw.c.Schedule(ctx)
	if got := sw.tasks(); len(got) != 2 {
		t.Fatalf("%d tasks after the wait", len(got))
	}

	// A worker with no channel at all cannot have its task either.
	sw.advance(time.Minute)
	connected(time.Time{}, false)
	sw.c.Schedule(ctx)
	if st := sw.c.Status(); st.Grid.RepairRunning != 0 || st.Grid.RepairFailed != 2 {
		t.Fatalf("a task whose worker has no channel: %+v", st.Grid)
	}
}
