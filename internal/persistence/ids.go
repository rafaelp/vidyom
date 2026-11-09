package persistence

import (
	"context"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
)

// nextSequence allocates the next number of a prefix inside the caller's write. It
// is what keeps two concurrent allocations from colliding and an ID from being
// reused (R5, D46): the row only grows and survives deletion of whatever used it.
func nextSequence(ctx context.Context, q querier, prefix id.Prefix) (id.Friendly, error) {
	var n uint64
	err := q.QueryRowContext(ctx,
		`INSERT INTO sequences (prefix, last) VALUES (?, 1)
		 ON CONFLICT (prefix) DO UPDATE SET last = last + 1
		 RETURNING last`, string(prefix)).Scan(&n)
	if err != nil {
		return "", mapErr(err)
	}
	return id.Format(prefix, n), nil
}

// refColumn resolves a reference to the column and value that identify a row. An
// unparsable reference yields ok=false: to a caller it is the same as an ID that
// does not exist in this tenant (I105).
func refColumn(ref id.Ref, wantPrefixes ...id.Prefix) (column, value string, ok bool) {
	s := string(ref)
	if id.IsFriendly(s) {
		if len(wantPrefixes) > 0 {
			prefix := id.Friendly(s).Prefix()
			match := false
			for _, want := range wantPrefixes {
				if prefix == want {
					match = true
					break
				}
			}
			if !match {
				return "", "", false
			}
		}
		return "friendly_id", s, true
	}
	if id.IsULID(s) {
		return "ulid", strings.ToUpper(s), true
	}
	return "", "", false
}

func invalidArg(format string, args ...any) error {
	return errs.Newf(errs.CodeInvalidArgument, format, args...)
}
