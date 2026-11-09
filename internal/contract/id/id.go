// Package id defines Vidyom's identifiers: an immutable, globally unique ULID
// technical key, and per-tenant friendly IDs used as human aliases (§2.3, D1,
// D46, D55). The API accepts and returns both.
package id

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// ULID is a canonical 26-character ULID string. It is the immutable technical
// key of any record and is globally unique.
type ULID string

// New returns a fresh ULID. It is safe for concurrent use and monotonic within
// the same millisecond.
func New() ULID { return ULID(ulid.Make().String()) }

// ParseULID validates s and returns it in canonical upper case.
func ParseULID(s string) (ULID, error) {
	u, err := ulid.ParseStrict(strings.ToUpper(s))
	if err != nil {
		return "", fmt.Errorf("invalid ULID %q: %w", s, err)
	}
	return ULID(u.String()), nil
}

// Time returns the timestamp encoded in the ULID, or the zero time when the
// value is not a valid ULID.
func (u ULID) Time() time.Time {
	parsed, err := ulid.ParseStrict(string(u))
	if err != nil {
		return time.Time{}
	}
	return ulid.Time(parsed.Time()).UTC()
}

func (u ULID) String() string { return string(u) }

// Prefix is a friendly ID prefix, without the trailing dash.
type Prefix string

// The prefixes of §2.3 (R5, D1, D55).
const (
	PrefixOriginal    Prefix = "ORI"
	PrefixClip        Prefix = "CLIP"
	PrefixEdited      Prefix = "EDT"
	PrefixJob         Prefix = "JOB"
	PrefixApproval    Prefix = "APR"
	PrefixPublication Prefix = "PUB"
	PrefixClient      Prefix = "CLIENT"
	PrefixUpload      Prefix = "UPL"
)

// Prefixes lists every valid prefix.
var Prefixes = []Prefix{
	PrefixOriginal, PrefixClip, PrefixEdited, PrefixJob,
	PrefixApproval, PrefixPublication, PrefixClient, PrefixUpload,
}

// Friendly is a human alias such as ORI-001 or CLIP-1000. Numbering is
// sequential per prefix and per tenant, except CLIENT which is system-wide
// (I14). A friendly ID is never reused.
type Friendly string

var friendlyPattern = regexp.MustCompile(`^(ORI|CLIP|EDT|JOB|APR|PUB|CLIENT|UPL)-([0-9]{3,})$`)

// Format builds a friendly ID. Numbers start at 1 and use at least 3 digits,
// with no upper limit.
func Format(p Prefix, n uint64) Friendly {
	return Friendly(fmt.Sprintf("%s-%03d", p, n))
}

// ParseFriendly validates s. A number with more than 3 digits may not have
// leading zeros, and 0 is not a valid number.
func ParseFriendly(s string) (Friendly, error) {
	if _, _, err := splitFriendly(s); err != nil {
		return "", err
	}
	return Friendly(s), nil
}

func splitFriendly(s string) (Prefix, uint64, error) {
	m := friendlyPattern.FindStringSubmatch(s)
	if m == nil {
		return "", 0, fmt.Errorf("invalid friendly ID %q", s)
	}
	digits := m[2]
	if len(digits) > 3 && digits[0] == '0' {
		return "", 0, fmt.Errorf("invalid friendly ID %q: leading zero", s)
	}
	n, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || n == 0 {
		return "", 0, fmt.Errorf("invalid friendly ID %q", s)
	}
	return Prefix(m[1]), n, nil
}

// Prefix returns the ID's prefix, or "" when the value is invalid.
func (f Friendly) Prefix() Prefix {
	p, _, err := splitFriendly(string(f))
	if err != nil {
		return ""
	}
	return p
}

// Number returns the ID's number, or 0 when the value is invalid.
func (f Friendly) Number() uint64 {
	_, n, err := splitFriendly(string(f))
	if err != nil {
		return 0
	}
	return n
}

func (f Friendly) String() string { return string(f) }

// IsFriendly reports whether s is a valid friendly ID.
func IsFriendly(s string) bool {
	_, _, err := splitFriendly(s)
	return err == nil
}

// IsULID reports whether s is a valid ULID.
func IsULID(s string) bool {
	_, err := ParseULID(s)
	return err == nil
}

// Ref is what an API path or a workflow parameter carries: a friendly ID or a
// ULID. Repositories resolve it within a tenant.
type Ref string

// Validate reports whether the reference has one of the two valid forms.
func (r Ref) Validate() error {
	if IsFriendly(string(r)) || IsULID(string(r)) {
		return nil
	}
	return fmt.Errorf("invalid ID %q: expected a friendly ID or a ULID", string(r))
}

func (r Ref) String() string { return string(r) }
