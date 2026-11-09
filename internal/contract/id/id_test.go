package id

import (
	"sync"
	"testing"
)

func TestFormatAndParse(t *testing.T) {
	cases := []struct {
		prefix Prefix
		n      uint64
		want   string
	}{
		{PrefixOriginal, 1, "ORI-001"},
		{PrefixClip, 42, "CLIP-042"},
		{PrefixClient, 999, "CLIENT-999"},
		{PrefixOriginal, 1000, "ORI-1000"},
		{PrefixUpload, 123456, "UPL-123456"},
	}
	for _, c := range cases {
		got := Format(c.prefix, c.n)
		if string(got) != c.want {
			t.Errorf("Format(%s, %d) = %s, want %s", c.prefix, c.n, got, c.want)
		}
		parsed, err := ParseFriendly(c.want)
		if err != nil {
			t.Errorf("ParseFriendly(%s): %v", c.want, err)
		}
		if parsed.Prefix() != c.prefix || parsed.Number() != c.n {
			t.Errorf("round trip of %s gave %s, %d", c.want, parsed.Prefix(), parsed.Number())
		}
	}
}

func TestParseFriendlyRejects(t *testing.T) {
	for _, s := range []string{"", "ORI", "ORI-", "ORI-01", "ORI-000", "ORI-0100", "COR-001", "ori-001", "ORI-001x", "LECT-001", "ORI--01"} {
		if _, err := ParseFriendly(s); err == nil {
			t.Errorf("ParseFriendly(%q) should fail", s)
		}
		if IsFriendly(s) {
			t.Errorf("IsFriendly(%q) should be false", s)
		}
	}
}

func TestEveryPrefixParses(t *testing.T) {
	for _, p := range Prefixes {
		if _, err := ParseFriendly(string(Format(p, 7))); err != nil {
			t.Errorf("prefix %s: %v", p, err)
		}
	}
}

func TestULID(t *testing.T) {
	u := New()
	parsed, err := ParseULID(string(u))
	if err != nil {
		t.Fatal(err)
	}
	if parsed != u {
		t.Fatalf("round trip %s != %s", parsed, u)
	}
	if u.Time().IsZero() {
		t.Fatal("ULID must encode a time")
	}
	if !IsULID(string(u)) || IsFriendly(string(u)) {
		t.Fatal("classification is wrong")
	}
	if _, err := ParseULID("not-a-ulid"); err == nil {
		t.Fatal("bad ULID accepted")
	}
}

func TestULIDConcurrentUnique(t *testing.T) {
	const n = 2000
	var mu sync.Mutex
	seen := make(map[ULID]bool, n)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range n / 8 {
				u := New()
				mu.Lock()
				if seen[u] {
					t.Errorf("duplicate ULID %s", u)
				}
				seen[u] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
}

func TestRefValidate(t *testing.T) {
	if err := Ref("ORI-001").Validate(); err != nil {
		t.Error(err)
	}
	if err := Ref(New()).Validate(); err != nil {
		t.Error(err)
	}
	if err := Ref("../etc/passwd").Validate(); err == nil {
		t.Error("path-like ref accepted")
	}
}
