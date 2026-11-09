package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

func TestParseTimecode(t *testing.T) {
	good := map[string]float64{
		"00:10:48": 648, "00:18:00": 1080, "10:48": 648, "648": 648, "648.5": 648.5,
		"01:00:00.250": 3600.25, "0": 0, "100:00:00": 360000,
	}
	for input, want := range good {
		got, err := parseTimecode(input)
		if err != nil || got != want {
			t.Errorf("%q: got %v, %v; want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "abc", "-1", "1:2:3:4", "00:61:00", "00:00:61", "1.5:00", ":10", "10:", "NaN", "Inf", "00:00:1e3"} {
		if _, err := parseTimecode(input); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestFormatTimecodeRoundTrips(t *testing.T) {
	for input, want := range map[float64]string{0: "00:00:00", 648: "00:10:48", 3600.25: "01:00:00.250", 0.1: "00:00:00.100"} {
		if got := formatTimecode(input); got != want {
			t.Errorf("%v: got %s, want %s", input, got, want)
		}
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"":                     "",
		"24h":                  "2026-11-08T12:00:00Z",
		"7d":                   "2026-11-02T12:00:00Z",
		"90m":                  "2026-11-09T10:30:00Z",
		"2026-11-01T00:00:00Z": "2026-11-01T00:00:00Z",
	}
	for input, want := range cases {
		got, err := parseSince(input, now)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"yesterday", "-1h", "1x"} {
		if _, err := parseSince(input, now); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestParseTTLSeconds(t *testing.T) {
	for input, want := range map[string]int{"15m": 900, "2h": 7200, "1d": 86400, "1500ms": 2, "": 0} {
		got, err := parseTTLSeconds("ttl", input)
		if err != nil || got != want {
			t.Errorf("%q: got %d, %v; want %d", input, got, err, want)
		}
	}
	for _, input := range []string{"soon", "-1h", "0s"} {
		if _, err := parseTTLSeconds("ttl", input); err == nil {
			t.Errorf("%q: expected an error", input)
		}
	}
}

func TestMoney(t *testing.T) {
	good := []struct {
		amount, currency string
		minor            int64
		formatted        string
	}{
		{"500", "USD", 50000, "500.00 USD"},
		{"12.5", "USD", 1250, "12.50 USD"},
		{"12.50", "BRL", 1250, "12.50 BRL"},
		{"0", "USD", 0, "0.00 USD"},
		{"0.05", "USD", 5, "0.05 USD"},
		{"500", "JPY", 500, "500 JPY"},
		{"1.234", "KWD", 1234, "1.234 KWD"},
	}
	for _, tc := range good {
		minor, err := parseMoney(tc.amount, tc.currency)
		if err != nil || minor != tc.minor {
			t.Errorf("%s %s: got %d, %v; want %d", tc.amount, tc.currency, minor, err, tc.minor)
		}
		if got := formatMoney(tc.minor, tc.currency); got != tc.formatted {
			t.Errorf("format %d %s: got %q, want %q", tc.minor, tc.currency, got, tc.formatted)
		}
	}
	for _, tc := range [][2]string{{"-1", "USD"}, {"1.234", "USD"}, {"1.5", "JPY"}, {"abc", "USD"}, {"", "USD"}, {"1.", "USD"}, {".5", "USD"}, {"1e3", "USD"}, {"99999999999999999999", "USD"}} {
		if _, err := parseMoney(tc[0], tc[1]); err == nil {
			t.Errorf("%v: expected an error", tc)
		}
	}
	if got := formatMoney(-150, "USD"); got != "-1.50 USD" {
		t.Errorf("negative: %q", got)
	}
}

func TestCoerceNumberKeepsTheLiteral(t *testing.T) {
	value, err := coerceInput("n", "1e5", wfdef.InputSpec{Type: wfdef.InputNumber})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(value)
	if err != nil || string(raw) != "1e5" {
		t.Fatalf("raw = %s, %v", raw, err)
	}
	for _, bad := range []string{"0x10", "Inf", "NaN", " 3", "1,5", ""} {
		if _, err := coerceInput("n", bad, wfdef.InputSpec{Type: wfdef.InputNumber}); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestParseFileReference(t *testing.T) {
	good := map[string]string{
		"/data/class.mp4":                  "server:/data/class.mp4",
		"server:/data/class.mp4":           "server:/data/class.mp4",
		`C:\data\class.mp4`:                `server:C:\data\class.mp4`, // a Windows server, from any client
		"upload:UPL-001":                   "upload:UPL-001",
		"storage://master/originals/x.mp4": "storage://master/originals/x.mp4",
		"import:youtube-main/abc":          "import:youtube-main/abc",
	}
	for input, want := range good {
		got, err := parseFileReference(input)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "class.mp4", "./class.mp4", "server:class.mp4", "upload:ORI-001", "storage://nowhere/x", "scratch://01JC0000000000000000000000/n/f", "import:nothing"} {
		if got, err := parseFileReference(input); err == nil {
			t.Errorf("%q: expected an error, got %q", input, got)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList([]string{"a,b", " c ", ""})
	if strings.Join(got, "|") != "a|b|c" {
		t.Fatalf("got = %v", got)
	}
	if splitList(nil) != nil {
		t.Fatal("empty input must stay nil")
	}
}

func TestClean(t *testing.T) {
	if got := clean("a\x1b[31mb\tc\nd\x07"); got != "a[31mb c d" {
		t.Fatalf("got %q", got)
	}
	if got := cleanBlock("a\nb\tc\x1b"); got != "a\nb\tc" {
		t.Fatalf("got %q", got)
	}
}

func TestFormatBytes(t *testing.T) {
	for input, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1024: "1.0 KiB", 1 << 30: "1.0 GiB", 5 << 40: "5.0 TiB"} {
		if got := formatBytes(input); got != want {
			t.Errorf("%d: got %q, want %q", input, got, want)
		}
	}
}

func TestServerNames(t *testing.T) {
	for _, name := range []string{"production", "ops-host", "a", "host.example_1"} {
		if err := validateServerName(name); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for _, name := range []string{"", "bad name", "../x", "-x", "a/b", strings.Repeat("x", 64)} {
		if err := validateServerName(name); err == nil {
			t.Errorf("%q: expected an error", name)
		}
	}
}
