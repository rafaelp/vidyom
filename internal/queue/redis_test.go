package queue

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
	"github.com/rafaelp/vidyom/internal/queue/redistest"
)

func pointer(n int) jobqueue.Pointer {
	return jobqueue.Pointer{
		TaskID:   jobqueue.TaskID(id.New(), "node", n),
		TenantID: "acme",
		Pool:     node.PoolMedia,
		NodeType: "video.cut",
	}
}

func read(t *testing.T, q jobqueue.Queue, consumer string) []jobqueue.Delivery {
	t.Helper()
	got, err := q.Read(context.Background(), jobqueue.ReadOptions{Pool: node.PoolMedia, Consumer: consumer, Count: 10})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// behavior runs the contract on any Redis.
func behavior(t *testing.T, client redis.UniversalClient, flush func()) {
	t.Helper()
	ctx := context.Background()
	q := &Redis{client: client}
	if err := q.Ping(ctx); err != nil {
		t.Fatal(err)
	}

	// Enqueue is atomic with its inflight record and a no-op on a known task.
	p := pointer(1)
	if err := q.Enqueue(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, p); err != nil {
		t.Fatal(err)
	}
	if ok, err := q.Inflight(ctx, p.TaskID); err != nil || !ok {
		t.Fatalf("inflight: %v %v", ok, err)
	}
	got := read(t, q, "w1")
	if len(got) != 1 || got[0].Pointer.TaskID != p.TaskID || got[0].Pointer.NodeType != "video.cut" {
		t.Fatalf("read %+v", got)
	}
	if got[0].Pointer.EnqueuedAt.IsZero() {
		t.Fatal("enqueue time missing")
	}
	if again := read(t, q, "w2"); len(again) != 0 {
		t.Fatalf("a read entry must not reach another consumer: %+v", again)
	}

	// Depth counts the backlog and what is unacknowledged.
	d, err := q.Depth(ctx, node.PoolMedia)
	if err != nil || d.Pending != 1 || d.Length != 1 {
		t.Fatalf("depth %+v %v", d, err)
	}

	// A dead consumer's entry can be claimed once it is idle long enough.
	claimed, err := q.ClaimStale(ctx, node.PoolMedia, "w2", time.Hour, 10)
	if err != nil || len(claimed) != 0 {
		t.Fatalf("not stale yet: %+v %v", claimed, err)
	}
	time.Sleep(5 * time.Millisecond)
	claimed, err = q.ClaimStale(ctx, node.PoolMedia, "w2", time.Millisecond, 10)
	if err != nil || len(claimed) != 1 || claimed[0].Pointer.TaskID != p.TaskID {
		t.Fatalf("claim %+v %v", claimed, err)
	}

	// Ack forgets the task, so it can be enqueued again by reconciliation.
	if err := q.Ack(ctx, node.PoolMedia, claimed[0].StreamID, p.TaskID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := q.Inflight(ctx, p.TaskID); ok {
		t.Fatal("ack must clear inflight")
	}
	d, err = q.Depth(ctx, node.PoolMedia)
	if err != nil || d.Pending != 0 || d.Length != 0 {
		t.Fatalf("depth after ack %+v %v", d, err)
	}

	// An old entry's ack does not erase a newer inflight record.
	again := pointer(2)
	if err := q.Enqueue(ctx, again); err != nil {
		t.Fatal(err)
	}
	first := read(t, q, "w1")
	if err := client.HSet(ctx, jobqueue.InflightKey, again.TaskID, "newer").Err(); err != nil {
		t.Fatal(err)
	}
	if err := q.Ack(ctx, node.PoolMedia, first[0].StreamID, again.TaskID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := q.Inflight(ctx, again.TaskID); !ok {
		t.Fatal("the newer inflight record must survive")
	}
	if err := client.HDel(ctx, jobqueue.InflightKey, again.TaskID).Err(); err != nil {
		t.Fatal(err)
	}

	// Redis emptied: the group is recreated, and work flows again.
	lost := pointer(3)
	if err := q.Enqueue(ctx, lost); err != nil {
		t.Fatal(err)
	}
	flush()
	if ok, _ := q.Inflight(ctx, lost.TaskID); ok {
		t.Fatal("the flush must lose the record")
	}
	if got := read(t, q, "w1"); len(got) != 0 {
		t.Fatalf("after flush: %+v", got)
	}
	if err := q.Enqueue(ctx, lost); err != nil {
		t.Fatal(err)
	}
	if got := read(t, q, "w1"); len(got) != 1 {
		t.Fatalf("after re-enqueue: %+v", got)
	}

	// Entries added before the group exists are delivered.
	flush()
	early := pointer(4)
	if err := q.Enqueue(ctx, early); err != nil {
		t.Fatal(err)
	}
	if got := read(t, q, "w1"); len(got) != 1 || got[0].Pointer.TaskID != early.TaskID {
		t.Fatalf("early entry: %+v", got)
	}

	// A malformed entry is dropped, not delivered forever.
	if err := client.XAdd(ctx, &redis.XAddArgs{Stream: jobqueue.StreamKey(node.PoolMedia), Values: map[string]any{pointerField: "not json"}}).Err(); err != nil {
		t.Fatal(err)
	}
	if got := read(t, q, "w1"); len(got) != 0 {
		t.Fatalf("malformed: %+v", got)
	}

	// Depth and claim on a pool with no group at all.
	if d, err := q.Depth(ctx, node.PoolAI); err != nil || d.Length != 0 {
		t.Fatalf("empty depth %+v %v", d, err)
	}
	if got, err := q.ClaimStale(ctx, node.PoolAI, "w", time.Millisecond, 1); err != nil || len(got) != 0 {
		t.Fatalf("empty claim %+v %v", got, err)
	}
	if err := q.Enqueue(ctx, jobqueue.Pointer{}); err == nil {
		t.Fatal("an empty pointer must be refused")
	}
}

func TestQueueOnMiniredis(t *testing.T) {
	m := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: m.Addr()})
	defer client.Close()
	behavior(t, client, m.FlushAll)
}

func TestQueueOnRealRedis(t *testing.T) {
	s := redistest.Start(t)
	client := s.Client()
	defer client.Close()
	behavior(t, client, func() {
		if err := client.FlushAll(context.Background()).Err(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestBlockingReadOnRealRedis(t *testing.T) {
	s := redistest.Start(t)
	client := s.Client()
	defer client.Close()
	q := NewRedis(s.Client())
	defer q.Close()
	ctx := context.Background()

	start := time.Now()
	got, err := q.Read(ctx, jobqueue.ReadOptions{Pool: node.PoolMedia, Consumer: "w", Count: 1, Block: 100 * time.Millisecond})
	if err != nil || len(got) != 0 {
		t.Fatalf("empty read: %+v %v", got, err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Fatalf("read did not block: %s", time.Since(start))
	}
	p := pointer(1)
	go func() {
		time.Sleep(50 * time.Millisecond)
		_ = q.Enqueue(ctx, p)
	}()
	got, err = q.Read(ctx, jobqueue.ReadOptions{Pool: node.PoolMedia, Consumer: "w", Count: 1, Block: 5 * time.Second})
	if err != nil || len(got) != 1 || got[0].Pointer.TaskID != p.TaskID {
		t.Fatalf("blocking read: %+v %v", got, err)
	}
}

// TestAOFSurvivesCrash proves what D4 asks of AOF on a real server: a SIGKILL keeps
// the stream, the group, the unacknowledged entry and the inflight record.
func TestAOFSurvivesCrash(t *testing.T) {
	s := redistest.Start(t)
	ctx := context.Background()
	q := NewRedis(s.Client())
	defer q.Close()

	acked, pending, queued := pointer(1), pointer(2), pointer(3)
	for _, p := range []jobqueue.Pointer{acked, pending} {
		if err := q.Enqueue(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	got := read(t, q, "dead-worker")
	if len(got) != 2 {
		t.Fatalf("read %+v", got)
	}
	if err := q.Ack(ctx, node.PoolMedia, got[0].StreamID, acked.TaskID); err != nil {
		t.Fatal(err)
	}
	if err := q.Enqueue(ctx, queued); err != nil {
		t.Fatal(err)
	}

	s.Kill()
	if err := q.Ping(ctx); err == nil {
		t.Fatal("redis should be down")
	}
	s.Restart()

	if ok, err := q.Inflight(ctx, acked.TaskID); err != nil || ok {
		t.Fatalf("acked task came back: %v %v", ok, err)
	}
	if ok, err := q.Inflight(ctx, pending.TaskID); err != nil || !ok {
		t.Fatalf("pending task lost: %v %v", ok, err)
	}
	fresh := read(t, q, "w2")
	if len(fresh) != 1 || fresh[0].Pointer.TaskID != queued.TaskID {
		t.Fatalf("queued entry: %+v", fresh)
	}
	time.Sleep(5 * time.Millisecond)
	claimed, err := q.ClaimStale(ctx, node.PoolMedia, "w2", time.Millisecond, 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, c := range claimed {
		ids[c.Pointer.TaskID] = true
	}
	if !ids[pending.TaskID] {
		t.Fatalf("the dead worker's entry must be claimable: %+v", claimed)
	}
}

// TestRestartWithoutData is the data-loss half of D4 at the queue level.
func TestRestartWithoutData(t *testing.T) {
	s := redistest.Start(t)
	ctx := context.Background()
	q := NewRedis(s.Client())
	defer q.Close()
	p := pointer(1)
	if err := q.Enqueue(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, q, "w"); len(got) != 1 {
		t.Fatalf("read %+v", got)
	}
	s.Kill()
	s.WipeData()
	s.Restart()
	if ok, err := q.Inflight(ctx, p.TaskID); err != nil || ok {
		t.Fatalf("expected the record to be gone: %v %v", ok, err)
	}
	if got := read(t, q, "w"); len(got) != 0 {
		t.Fatalf("expected no entry: %+v", got)
	}
	if err := q.Enqueue(ctx, p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, q, "w"); len(got) != 1 {
		t.Fatalf("expected the re-enqueued entry: %+v", got)
	}
}
