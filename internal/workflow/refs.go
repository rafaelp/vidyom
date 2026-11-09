package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// RefKind is the namespace of a reference.
type RefKind string

const (
	RefInputs RefKind = "inputs"
	RefNodes  RefKind = "nodes"
	RefEvent  RefKind = "event"
	RefJob    RefKind = "job"
)

// JobFields lists what ${job.<field>} may name (I141).
var JobFields = map[string]bool{
	"id": true, "ulid": true, "workflow": true, "workflow_version": true,
	"tenant_id": true, "trace_id": true,
}

// Ref is one parsed ${...} reference. Node is set for RefNodes. Path is what
// follows the namespace: the input name and beyond for inputs, what follows
// ".output" for nodes.
type Ref struct {
	Kind RefKind
	Node string
	Path []string
	Raw  string
}

// Part is a piece of a template: a literal, or a reference.
type Part struct {
	Literal string
	Ref     *Ref
}

var (
	refSegment = `[A-Za-z0-9_-]+`
	refInputs  = regexp.MustCompile(`^inputs((?:\.` + refSegment + `)+)$`)
	refNodes   = regexp.MustCompile(`^nodes\.(` + refSegment + `)\.output((?:\.` + refSegment + `)*)$`)
	refEvent   = regexp.MustCompile(`^event((?:\.` + refSegment + `)+)$`)
	refJob     = regexp.MustCompile(`^job\.(` + refSegment + `)$`)
)

func parseRef(inner string) (*Ref, error) {
	split := func(p string) []string {
		if p == "" {
			return nil
		}
		return strings.Split(strings.TrimPrefix(p, "."), ".")
	}
	raw := "${" + inner + "}"
	switch {
	case refInputs.MatchString(inner):
		return &Ref{Kind: RefInputs, Path: split(refInputs.FindStringSubmatch(inner)[1]), Raw: raw}, nil
	case refNodes.MatchString(inner):
		m := refNodes.FindStringSubmatch(inner)
		return &Ref{Kind: RefNodes, Node: m[1], Path: split(m[2]), Raw: raw}, nil
	case refEvent.MatchString(inner):
		return &Ref{Kind: RefEvent, Path: split(refEvent.FindStringSubmatch(inner)[1]), Raw: raw}, nil
	case refJob.MatchString(inner):
		m := refJob.FindStringSubmatch(inner)
		if !JobFields[m[1]] {
			return nil, fmt.Errorf("unknown job field in %s", raw)
		}
		return &Ref{Kind: RefJob, Path: []string{m[1]}, Raw: raw}, nil
	default:
		return nil, fmt.Errorf("invalid reference %s: expected inputs.<name>, nodes.<id>.output[.<path>], event.<path> or job.<field>", raw)
	}
}

// ParseTemplate splits a string into literals and references. "$${" writes a
// literal "${" (R24).
func ParseTemplate(s string) ([]Part, error) {
	if !strings.Contains(s, "${") {
		return []Part{{Literal: s}}, nil
	}
	var parts []Part
	var lit strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			lit.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unterminated reference in %q", s)
			}
			ref, err := parseRef(s[i+2 : i+end])
			if err != nil {
				return nil, err
			}
			if lit.Len() > 0 {
				parts = append(parts, Part{Literal: lit.String()})
				lit.Reset()
			}
			parts = append(parts, Part{Ref: ref})
			i += end + 1
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	if lit.Len() > 0 || len(parts) == 0 {
		parts = append(parts, Part{Literal: lit.String()})
	}
	return parts, nil
}

// HasRef reports whether s holds at least one reference. A string that fails to
// parse counts, so that static checks do not also report it as a type mismatch.
func HasRef(s string) bool {
	if !strings.Contains(s, "${") {
		return false
	}
	parts, err := ParseTemplate(s)
	if err != nil {
		return true
	}
	for _, p := range parts {
		if p.Ref != nil {
			return true
		}
	}
	return false
}

// DecodeJSON decodes keeping numbers exact, as json.Number.
func DecodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// EncodeJSON encodes without escaping HTML, so < and > survive in text.
func EncodeJSON(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// CollectRefs parses every string inside a decoded JSON value. skip names object
// keys at the top level that are not templates, such as a flow.if condition.
func CollectRefs(v any, skip map[string]bool, visit func(Ref)) error {
	var firstErr error
	var walk func(v any, top bool)
	walk = func(v any, top bool) {
		switch x := v.(type) {
		case string:
			parts, err := ParseTemplate(x)
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			for _, p := range parts {
				if p.Ref != nil {
					visit(*p.Ref)
				}
			}
		case []any:
			for _, e := range x {
				walk(e, false)
			}
		case map[string]any:
			for k, e := range x {
				if top && skip[k] {
					continue
				}
				walk(e, false)
			}
		}
	}
	walk(v, true)
	return firstErr
}

// NodeData is what a resolver knows about one node.
type NodeData struct {
	// Output is the decoded output; nil when the node has none (absent, I141).
	Output any
	// Status is the node's state, visible to CEL as nodes.<id>.status.
	Status string
}

// Scope is the data a job offers to references and conditions. None of it holds
// secrets.
type Scope struct {
	Inputs map[string]any
	Nodes  map[string]NodeData
	Event  map[string]any // nil when the job was not started by an event
	Job    map[string]any
}

// ErrAbsent says a reference names nothing (I141).
var ErrAbsent = errors.New("absent")

func walkPath(v any, path []string) (any, bool) {
	for _, seg := range path {
		switch x := v.(type) {
		case map[string]any:
			next, ok := x[seg]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(x) {
				return nil, false
			}
			v = x[idx]
		default:
			return nil, false
		}
	}
	return v, v != nil
}

// Lookup returns the value of a reference, or false when it is absent.
func (s Scope) Lookup(r Ref) (any, bool) {
	switch r.Kind {
	case RefInputs:
		return walkPath(s.Inputs, r.Path)
	case RefNodes:
		n, ok := s.Nodes[r.Node]
		if !ok {
			return nil, false
		}
		return walkPath(n.Output, r.Path)
	case RefEvent:
		return walkPath(s.Event, r.Path)
	case RefJob:
		return walkPath(s.Job, r.Path)
	}
	return nil, false
}

func scalarText(v any, raw string) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case json.Number:
		return x.String(), nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	default:
		return "", fmt.Errorf("reference %s is not a scalar and cannot be embedded in text", raw)
	}
}

// resolveString resolves one template. A string that is exactly one reference
// keeps the referenced type; absent reports the reference named nothing.
func resolveString(s string, scope Scope) (value any, absent bool, err error) {
	if !strings.Contains(s, "${") {
		return s, false, nil
	}
	parts, err := ParseTemplate(s)
	if err != nil {
		return nil, false, err
	}
	if len(parts) == 1 && parts[0].Ref != nil {
		v, ok := scope.Lookup(*parts[0].Ref)
		if !ok {
			return nil, true, nil
		}
		return v, false, nil
	}
	var b strings.Builder
	for _, p := range parts {
		if p.Ref == nil {
			b.WriteString(p.Literal)
			continue
		}
		v, ok := scope.Lookup(*p.Ref)
		if !ok {
			return nil, false, fmt.Errorf("reference %s embedded in text has no value", p.Ref.Raw)
		}
		text, err := scalarText(v, p.Ref.Raw)
		if err != nil {
			return nil, false, err
		}
		b.WriteString(text)
	}
	return b.String(), false, nil
}

func resolveValue(v any, scope Scope, skip map[string]bool, top bool) (any, bool, error) {
	switch x := v.(type) {
	case string:
		return resolveString(x, scope)
	case []any:
		out := make([]any, 0, len(x))
		for _, e := range x {
			r, absent, err := resolveValue(e, scope, nil, false)
			if err != nil {
				return nil, false, err
			}
			if absent {
				if s, ok := e.(string); ok {
					return nil, false, fmt.Errorf("array element %s has no value", s)
				}
				return nil, false, errors.New("array element has no value")
			}
			out = append(out, r)
		}
		return out, false, nil
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			if top && skip[k] {
				out[k] = e
				continue
			}
			r, absent, err := resolveValue(e, scope, nil, false)
			if err != nil {
				return nil, false, fmt.Errorf("%s: %w", k, err)
			}
			if !absent {
				out[k] = r
			}
		}
		return out, false, nil
	default:
		return v, false, nil
	}
}

// ResolveParameters replaces references in a node's parameters (§6.2). Object
// keys holding only an absent reference are removed (I141). skip names top-level
// keys left as written, such as a flow.if condition, which is CEL (D2).
func ResolveParameters(raw json.RawMessage, scope Scope, skip map[string]bool) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	v, err := DecodeJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode parameters: %w", err)
	}
	r, absent, err := resolveValue(v, scope, skip, true)
	if err != nil {
		return nil, err
	}
	if absent {
		return json.RawMessage(`{}`), nil
	}
	return EncodeJSON(r)
}

// ResolveOutput resolves one `outputs` entry. ok is false when it is absent.
func ResolveOutput(template string, scope Scope) (any, bool, error) {
	v, absent, err := resolveString(template, scope)
	if err != nil {
		return nil, false, err
	}
	return v, !absent, nil
}
