package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// parseTimecode reads HH:MM:SS[.fff], MM:SS[.fff] or plain seconds into seconds.
func parseTimecode(s string) (float64, error) {
	bad := fmt.Errorf("invalid time %q: expected HH:MM:SS, MM:SS or seconds", s)
	if s == "" {
		return 0, bad
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, bad
	}
	var total float64
	for i, part := range parts {
		isLast := i == len(parts)-1
		if part == "" {
			return 0, bad
		}
		if !isLast && strings.Contains(part, ".") {
			return 0, bad
		}
		value, err := strconv.ParseFloat(part, 64)
		if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, bad
		}
		if len(parts) > 1 && i > 0 && value >= 60 {
			return 0, bad
		}
		total = total*60 + value
	}
	return total, nil
}

// parseSince turns "24h" or "7d" into an RFC 3339 instant before now; RFC 3339
// passes through (I171).
func parseSince(s string, now time.Time) (string, error) {
	if s == "" {
		return "", nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Format(time.RFC3339), nil
	}
	d, err := parseDuration(s)
	if err != nil || d < 0 {
		return "", fmt.Errorf("invalid --since %q: expected a duration like 24h or 7d, or an RFC 3339 time", s)
	}
	return now.Add(-d).UTC().Format(time.RFC3339), nil
}

// parseDuration is time.ParseDuration plus a day suffix.
func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil {
			return 0, fmt.Errorf("invalid duration %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

// parseTTLSeconds reads a positive duration into whole seconds, rounding up.
func parseTTLSeconds(flag, s string) (int, error) {
	if s == "" {
		return 0, nil
	}
	d, err := parseDuration(s)
	if err != nil || d <= 0 {
		// The value is not repeated: on secret commands a misplaced value could be one.
		return 0, usagef("invalid --%s: expected a positive duration like 15m or 2h", flag)
	}
	return int(math.Ceil(d.Seconds())), nil
}

// currencyExponent is the ISO 4217 minor unit exponent of a currency (I175).
func currencyExponent(currency string) int {
	switch currency {
	case "BIF", "CLP", "DJF", "GNF", "ISK", "JPY", "KMF", "KRW", "PYG", "RWF", "UGX", "UYI", "VND", "VUV", "XAF", "XOF", "XPF":
		return 0
	case "BHD", "IQD", "JOD", "KWD", "LYD", "OMR", "TND":
		return 3
	default:
		return 2
	}
}

// parseMoney converts a decimal amount in major units to minor units, without
// floating point. "500" USD is 50000.
func parseMoney(amount, currency string) (int64, error) {
	exponent := currencyExponent(currency)
	whole, fraction, hasFraction := strings.Cut(strings.TrimSpace(amount), ".")
	bad := usagef("invalid amount %q: expected a non-negative decimal like 500 or 12.50", amount)
	if whole == "" || !isDigits(whole) || (hasFraction && (fraction == "" || !isDigits(fraction))) {
		return 0, bad
	}
	if len(fraction) > exponent {
		return 0, usagef("%s has %d decimal places, and %q has %d", currency, exponent, amount, len(fraction))
	}
	fraction += strings.Repeat("0", exponent-len(fraction))
	minor, err := strconv.ParseInt(whole+fraction, 10, 64)
	if err != nil {
		return 0, bad
	}
	return minor, nil
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

// formatMoney renders minor units in major units: 50000 USD is "500.00 USD".
func formatMoney(minor int64, currency string) string {
	exponent := currencyExponent(currency)
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	digits := strconv.FormatInt(minor, 10)
	if exponent == 0 {
		return strings.TrimSpace(sign + digits + " " + currency)
	}
	for len(digits) <= exponent {
		digits = "0" + digits
	}
	cut := len(digits) - exponent
	return strings.TrimSpace(sign + digits[:cut] + "." + digits[cut:] + " " + currency)
}

// parseInputs reads --input key=value flags, coerced by the workflow's declared
// inputs (I173). Undeclared inputs stay strings; the server validates them.
func parseInputs(values []string, specs map[string]wfdef.InputSpec) (map[string]any, error) {
	out := make(map[string]any, len(values))
	for _, raw := range values {
		key, value, ok := strings.Cut(raw, "=")
		if !ok || key == "" {
			return nil, usagef("invalid --input %q: expected key=value", raw)
		}
		coerced, err := coerceInput(key, value, specs[key])
		if err != nil {
			return nil, err
		}
		out[key] = coerced
	}
	return out, nil
}

func coerceInput(key, value string, spec wfdef.InputSpec) (any, error) {
	switch {
	case spec.Type == wfdef.InputFile, spec.Type == "" && key == "file":
		// The four forms of R20. A bare absolute path is a server path, for
		// compatibility with the examples. scratch:// and relative paths are refused.
		uri, err := domain.ParseUserInputURI(value)
		if err != nil {
			return nil, usagef("invalid --input %s: %v", key, err)
		}
		return uri.String(), nil
	case spec.Type == wfdef.InputNumber:
		var number json.Number
		if err := json.Unmarshal([]byte(value), &number); err != nil || number.String() != value {
			return nil, usagef("invalid --input %s: expected a number, got %q", key, value)
		}
		return number, nil
	case spec.Type == wfdef.InputBool:
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return nil, usagef("invalid --input %s: expected true or false, got %q", key, value)
		}
		return parsed, nil
	case spec.Type == wfdef.InputArray, spec.Type == wfdef.InputObject:
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err != nil {
			return nil, usagef("invalid --input %s: expected JSON", key)
		}
		switch parsed.(type) {
		case []any:
			if spec.Type != wfdef.InputArray {
				return nil, usagef("invalid --input %s: expected a JSON object", key)
			}
		case map[string]any:
			if spec.Type != wfdef.InputObject {
				return nil, usagef("invalid --input %s: expected a JSON array", key)
			}
		default:
			return nil, usagef("invalid --input %s: expected a JSON %s", key, spec.Type)
		}
		return parsed, nil
	default:
		return value, nil
	}
}

// validateRef checks a friendly ID or ULID argument before it reaches a path.
func validateRef(label, value string) error {
	if err := id.Ref(value).Validate(); err != nil {
		return usagef("invalid %s %q: expected an ID like ORI-001 or a ULID", label, value)
	}
	return nil
}

// splitList splits a comma-separated flag value, dropping empty entries.
func splitList(values []string) []string {
	var out []string
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out = append(out, item)
			}
		}
	}
	return out
}
