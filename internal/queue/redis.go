package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/contract/node"
)

// pointerField is the only field of a stream entry.
const pointerField = "pointer"

// enqueueScript adds a pointer together with its inflight record, atomically, so
// no crash can leave one without the other. It is a no-op for a known task.
var enqueueScript = redis.NewScript(`
if redis.call('HEXISTS', KEYS[2], ARGV[1]) == 1 then
  return 0
end
local id = redis.call('XADD', KEYS[1], '*', ARGV[2], ARGV[3])
redis.call('HSET', KEYS[2], ARGV[1], id)
return 1
`)

// ackScript acknowledges an entry, trims it and forgets the task. An old entry of a
// task that was enqueued again does not erase the newer inflight record.
var ackScript = redis.NewScript(`
redis.call('XACK', KEYS[1], ARGV[1], ARGV[2])
redis.call('XDEL', KEYS[1], ARGV[2])
local current = redis.call('HGET', KEYS[2], ARGV[3])
if current == ARGV[2] or current == false then
  redis.call('HDEL', KEYS[2], ARGV[3])
end
return 1
`)

// Redis implements jobqueue.Queue.
type Redis struct {
	client redis.UniversalClient
}

var _ jobqueue.Queue = (*Redis)(nil)

// NewRedis returns the queue over an open client. It takes ownership: Close closes
// the client.
func NewRedis(client redis.UniversalClient) jobqueue.Queue { return &Redis{client: client} }

func unavailable(err error, what string) error {
	return errs.Wrap(errs.CodeUnavailable, err, "the job queue is unavailable: "+what)
}

func isNoGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "NOGROUP")
}

// Enqueue implements jobqueue.Queue.
func (q *Redis) Enqueue(ctx context.Context, p jobqueue.Pointer) error {
	if p.TaskID == "" || p.Pool == "" {
		return errs.New(errs.CodeInvalidArgument, "a pointer needs a task and a pool")
	}
	if p.EnqueuedAt.IsZero() {
		p.EnqueuedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("encode pointer: %w", err)
	}
	keys := []string{jobqueue.StreamKey(p.Pool), jobqueue.InflightKey}
	if err := enqueueScript.Run(ctx, q.client, keys, p.TaskID, pointerField, payload).Err(); err != nil {
		return unavailable(err, "enqueue")
	}
	return nil
}

// Inflight implements jobqueue.Queue.
func (q *Redis) Inflight(ctx context.Context, taskID string) (bool, error) {
	ok, err := q.client.HExists(ctx, jobqueue.InflightKey, taskID).Result()
	if err != nil {
		return false, unavailable(err, "inflight")
	}
	return ok, nil
}

func (q *Redis) createGroup(ctx context.Context, pool node.Pool) error {
	// From the start of the stream, so entries added before the group existed are
	// delivered, which matters after a restart of Redis.
	err := q.client.XGroupCreateMkStream(ctx, jobqueue.StreamKey(pool), jobqueue.GroupName(pool), "0").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return unavailable(err, "create group")
	}
	return nil
}

// Read implements jobqueue.Queue. The group is created on first use, and again
// after Redis lost its data.
func (q *Redis) Read(ctx context.Context, opts jobqueue.ReadOptions) ([]jobqueue.Delivery, error) {
	count := int64(opts.Count)
	if count <= 0 {
		count = 1
	}
	block := opts.Block
	if block <= 0 {
		block = -1 // no BLOCK argument
	}
	args := &redis.XReadGroupArgs{
		Group:    jobqueue.GroupName(opts.Pool),
		Consumer: opts.Consumer,
		Streams:  []string{jobqueue.StreamKey(opts.Pool), ">"},
		Count:    count,
		Block:    block,
	}
	for attempt := 0; ; attempt++ {
		streams, err := q.client.XReadGroup(ctx, args).Result()
		if err == nil {
			var out []jobqueue.Delivery
			for _, s := range streams {
				out = append(out, q.decode(ctx, opts.Pool, s.Messages)...)
			}
			return out, nil
		}
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		if isNoGroup(err) && attempt == 0 {
			if err := q.createGroup(ctx, opts.Pool); err != nil {
				return nil, err
			}
			continue
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable(err, "read")
	}
}

// decode parses entries. A malformed entry is acknowledged and dropped, because no
// consumer could ever act on it.
func (q *Redis) decode(ctx context.Context, pool node.Pool, messages []redis.XMessage) []jobqueue.Delivery {
	out := make([]jobqueue.Delivery, 0, len(messages))
	for _, m := range messages {
		raw, _ := m.Values[pointerField].(string)
		var p jobqueue.Pointer
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.TaskID == "" {
			_ = q.client.XAck(ctx, jobqueue.StreamKey(pool), jobqueue.GroupName(pool), m.ID).Err()
			_ = q.client.XDel(ctx, jobqueue.StreamKey(pool), m.ID).Err()
			continue
		}
		out = append(out, jobqueue.Delivery{Pointer: p, StreamID: m.ID})
	}
	return out
}

// ClaimStale implements jobqueue.Queue.
func (q *Redis) ClaimStale(ctx context.Context, pool node.Pool, consumer string, minIdle time.Duration, count int) ([]jobqueue.Delivery, error) {
	if count <= 0 {
		count = 16
	}
	messages, _, err := q.client.XAutoClaim(ctx, &redis.XAutoClaimArgs{
		Stream:   jobqueue.StreamKey(pool),
		Group:    jobqueue.GroupName(pool),
		Consumer: consumer,
		MinIdle:  minIdle,
		Start:    "0-0",
		Count:    int64(count),
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) || isNoGroup(err) {
			return nil, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, unavailable(err, "claim")
	}
	return q.decode(ctx, pool, messages), nil
}

// Ack implements jobqueue.Queue.
func (q *Redis) Ack(ctx context.Context, pool node.Pool, streamID, taskID string) error {
	keys := []string{jobqueue.StreamKey(pool), jobqueue.InflightKey}
	if err := ackScript.Run(ctx, q.client, keys, jobqueue.GroupName(pool), streamID, taskID).Err(); err != nil {
		return unavailable(err, "ack")
	}
	return nil
}

// Depth implements jobqueue.Queue.
func (q *Redis) Depth(ctx context.Context, pool node.Pool) (jobqueue.Depth, error) {
	length, err := q.client.XLen(ctx, jobqueue.StreamKey(pool)).Result()
	if err != nil {
		return jobqueue.Depth{}, unavailable(err, "depth")
	}
	pending, err := q.client.XPending(ctx, jobqueue.StreamKey(pool), jobqueue.GroupName(pool)).Result()
	if err != nil {
		if isNoGroup(err) || errors.Is(err, redis.Nil) {
			return jobqueue.Depth{Length: length}, nil
		}
		return jobqueue.Depth{}, unavailable(err, "depth")
	}
	return jobqueue.Depth{Length: length, Pending: pending.Count}, nil
}

// Ping implements jobqueue.Queue.
func (q *Redis) Ping(ctx context.Context) error {
	if err := q.client.Ping(ctx).Err(); err != nil {
		return unavailable(err, "ping")
	}
	return nil
}

// Close implements jobqueue.Queue.
func (q *Redis) Close() error { return q.client.Close() }
