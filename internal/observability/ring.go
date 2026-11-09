package observability

import (
	"strconv"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
)

// recordRing keeps recent records in memory whatever is configured, so GET /logs has
// an answer even with no sink at all (§10.1). Sequence numbers make its pages stable.
type recordRing struct {
	mu   sync.RWMutex
	buf  []connector.LogRecord
	next int64 // sequence of the next record
}

func newRecordRing(size int) *recordRing {
	return &recordRing{buf: make([]connector.LogRecord, size)}
}

func (r *recordRing) add(rec connector.LogRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf[int(r.next%int64(len(r.buf)))] = rec
	r.next++
}

// query returns matching records, newest first, older than the cursor's sequence.
func (r *recordRing) query(q connector.LogQuery, minRank int) connector.LogPage {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 1000)
	r.mu.RLock()
	defer r.mu.RUnlock()
	start := r.next - 1
	if q.Cursor != "" {
		if c, err := strconv.ParseInt(q.Cursor, 10, 64); err == nil {
			start = c - 1
		} else {
			return connector.LogPage{}
		}
	}
	oldest := max(r.next-int64(len(r.buf)), 0)
	page := connector.LogPage{}
	for seq := start; seq >= oldest; seq-- {
		rec := &r.buf[int(seq%int64(len(r.buf)))]
		if !matchesQuery(rec, &q, minRank) {
			continue
		}
		if len(page.Records) == limit {
			page.NextCursor = strconv.FormatInt(seq+1, 10)
			break
		}
		page.Records = append(page.Records, *rec)
	}
	return page
}

// since returns matching records at or after a time, oldest first, for a subscriber
// asking for recent history.
func (r *recordRing) since(q connector.LogQuery, minRank int, from time.Time) []connector.LogRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	oldest := max(r.next-int64(len(r.buf)), 0)
	var out []connector.LogRecord
	for seq := oldest; seq < r.next; seq++ {
		rec := &r.buf[int(seq%int64(len(r.buf)))]
		if !rec.Timestamp.Before(from) && matchesQuery(rec, &q, minRank) {
			out = append(out, *rec)
		}
	}
	return out
}

func levelRank(level string) int {
	switch level {
	case "debug":
		return 1
	case "info", "":
		return 2
	case "warn":
		return 3
	case "error":
		return 4
	}
	return 2
}

func matchesQuery(rec *connector.LogRecord, q *connector.LogQuery, minRank int) bool {
	if q.TenantID != "" && rec.TenantID != q.TenantID {
		return false
	}
	if q.JobID != "" && rec.JobID != q.JobID {
		return false
	}
	if minRank > 0 && levelRank(rec.Level) < minRank {
		return false
	}
	if !q.Since.IsZero() && rec.Timestamp.Before(q.Since) {
		return false
	}
	return true
}
