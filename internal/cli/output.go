package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

// emit writes a result: its JSON form with --json, or the human form.
func (a *app) emit(value any, human func(w io.Writer)) error {
	if a.jsonOutput {
		return a.writeJSON(value)
	}
	human(a.stdout())
	return nil
}

func (a *app) writeJSON(value any) error {
	encoder := json.NewEncoder(a.stdout())
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write the result")
	}
	return nil
}

// writeLine writes one NDJSON line, for streams.
func (a *app) writeLine(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot encode the event")
	}
	if _, err := a.stdout().Write(append(raw, '\n')); err != nil {
		return errs.Wrap(errs.CodeInternal, err, "cannot write the event")
	}
	return nil
}

// note writes progress or a remark to stderr. Stdout stays parseable.
func (a *app) note(format string, args ...any) {
	if a.jsonOutput {
		return
	}
	fmt.Fprintf(a.stderr(), format+"\n", args...)
}

// warn writes to stderr in every mode.
func (a *app) warn(format string, args ...any) {
	fmt.Fprintf(a.stderr(), "warning: "+format+"\n", args...)
}

func (a *app) printError(coded *errs.Error, hint string) {
	if a.jsonOutput {
		public := errs.Public(coded)
		encoder := json.NewEncoder(a.stderr())
		encoder.SetEscapeHTML(false)
		if hint != "" {
			public = public.With("hint", hint)
		}
		_ = encoder.Encode(errs.Body{Error: public})
		return
	}
	message := coded.Message
	if cause := errors.Unwrap(coded); cause != nil {
		// A local cause helps here, and only a server's errors arrive without one. The
		// contract's keyring errors already start with the message.
		if detail := cause.Error(); strings.Contains(detail, message) {
			message = detail
		} else {
			message += ": " + detail
		}
	}
	fmt.Fprintf(a.stderr(), "error: %s\n", clean(message))
	if hint != "" {
		fmt.Fprintf(a.stderr(), "hint: %s\n", hint)
	}
	if coded.RequestID != "" {
		fmt.Fprintf(a.stderr(), "request id: %s\n", clean(coded.RequestID))
	}
}

// hintFor says what to do next for the errors a person can act on.
func (a *app) hintFor(coded *errs.Error) string {
	switch coded.Code {
	case errs.CodeTenantRequired:
		return "select one with 'vidyom tenant use <tenant>', or pass --tenant"
	case errs.CodeUnauthenticated:
		if strings.Contains(coded.Message, security.ErrKeyringUnavailable.Error()) {
			return "" // pairing would fail the same way; the message says what is missing
		}
		if name, _, _, err := a.resolveServer(); err == nil {
			return fmt.Sprintf("pair this client with 'vidyom server pair %s --code <one-time-code>'; an admin creates the code with 'vidyom device pairing-code create'", name)
		}
		return "pair this client with 'vidyom server pair <server> --code <one-time-code>'"
	case errs.CodeSealed:
		return "the daemon's vault is sealed; the operator must provide the master key"
	case errs.CodeUnavailable:
		if _, ok := coded.Details["reason"]; ok {
			return ""
		}
		return "check that the daemon is running and that 'vidyom server list' points at it"
	case errs.CodeBudgetExceeded:
		return "an admin can review it with 'vidyom budget show' and allow it with 'vidyom budget override'"
	case errs.CodeNotFound:
		// From the server, this also means another tenant's ID (§12.1).
		if coded.RequestID == "" {
			return ""
		}
		if name, entry, _, err := a.resolveServer(); err == nil {
			if tenant, err := a.currentTenant(entry); err == nil && tenant != "" {
				return fmt.Sprintf("looked in tenant %s on %s", tenant, name)
			}
		}
		return ""
	default:
		return ""
	}
}

// clean strips control characters from server-provided text, so a title or a log
// line cannot drive the terminal. Tabs and newlines would break tables and are
// replaced by spaces.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n' || r == '\r':
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
}

// cleanBlock is clean for multi-line text, keeping line breaks.
func cleanBlock(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, s)
}

// table writes rows under a header, with server-provided cells cleaned.
func table(w io.Writer, header []string, rows [][]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(header, "\t"))
	for _, row := range rows {
		cells := make([]string, len(row))
		for i, cell := range row {
			cells[i] = clean(cell)
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	_ = tw.Flush()
}

// fields writes aligned "name: value" lines, skipping empty values.
func fields(w io.Writer, pairs ...[2]string) {
	tw := tabwriter.NewWriter(w, 0, 4, 1, ' ', 0)
	for _, p := range pairs {
		if p[1] == "" {
			continue
		}
		fmt.Fprintf(tw, "%s:\t%s\n", p[0], clean(p[1]))
	}
	_ = tw.Flush()
}

func kv(name, value string) [2]string { return [2]string{name, value} }

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatTimePtr(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatTime(*t)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// formatTimecode renders seconds as HH:MM:SS with milliseconds when present.
func formatTimecode(seconds float64) string {
	total := time.Duration(seconds * float64(time.Second)).Round(time.Millisecond)
	hours := total / time.Hour
	total -= hours * time.Hour
	minutes := total / time.Minute
	total -= minutes * time.Minute
	secs := total / time.Second
	total -= secs * time.Second
	if total > 0 {
		return fmt.Sprintf("%02d:%02d:%02d.%03d", hours, minutes, secs, total/time.Millisecond)
	}
	return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, secs)
}

// prettyJSON indents raw JSON for human output, cleaned of control characters.
func prettyJSON(raw []byte) string {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return clean(string(raw))
	}
	out, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return clean(string(raw))
	}
	return cleanBlock(string(out))
}
