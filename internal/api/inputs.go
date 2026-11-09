package apiserver

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// R20 enforcement at submission (I131). This is the first of two layers: the
// worker's OpenInput checks ingestRoots again when it opens the file.

// normalizeRef parses a client's file reference and returns its canonical form:
// server: with symlinks resolved inside ingestRoots, upload: with the ULID that
// names the spool file. scratch:// is internal and refused.
func (s *server) normalizeRef(c *call, raw string) (string, error) {
	u, err := domain.ParseUserInputURI(raw)
	if err != nil {
		return "", errs.Wrap(errs.CodeInvalidArgument, err, err.Error())
	}
	switch u.Kind {
	case domain.URIServer:
		if !s.lexicallyInsideRoots(u.Path) {
			// Before touching the file system, so that "not found" and "forbidden"
			// cannot be used to probe paths outside the roots.
			return "", errs.New(errs.CodeForbidden, "server path is outside the configured ingest roots")
		}
		resolved, err := domain.ResolveServerPath(s.d.IngestRoots, u.Path)
		if err != nil {
			return "", err
		}
		return "server:" + resolved, nil
	case domain.URIUpload:
		if s.d.Uploads == nil {
			return "", errs.New(errs.CodeNotImplemented, "uploads are not wired in this daemon")
		}
		upload, err := s.d.Uploads.Get(c.ctx, c.scope, id.Ref(u.UploadRef))
		if err != nil {
			return "", err
		}
		if upload.Status != domain.UploadComplete {
			return "", errs.Newf(errs.CodeFailedPrecondition, "upload %s is %s, not complete and verified", upload.ID, upload.Status)
		}
		return "upload:" + string(upload.ULID), nil
	default:
		return u.String(), nil
	}
}

func (s *server) lexicallyInsideRoots(path string) bool {
	clean := filepath.Clean(path)
	for _, root := range s.d.IngestRoots {
		for _, candidate := range rootForms(root) {
			rel, err := filepath.Rel(candidate, clean)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// rootForms returns a root as configured and with its symlinks resolved, because a
// client may name either (/tmp and /private/tmp on macOS).
func rootForms(root string) []string {
	forms := []string{filepath.Clean(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != forms[0] {
		forms = append(forms, resolved)
	}
	return forms
}

// isFileRefLike recognizes references in values not declared as files. A string
// like "server: hello" in a prompt is left alone.
func isFileRefLike(v string) bool {
	if strings.HasPrefix(v, "scratch://") {
		return true
	}
	if rest, ok := strings.CutPrefix(v, "server:"); ok {
		return filepath.IsAbs(rest)
	}
	if rest, ok := strings.CutPrefix(v, "upload:"); ok {
		return id.IsFriendly(rest) || id.IsULID(rest)
	}
	return false
}

// scan rewrites references found anywhere inside a decoded JSON value.
func (s *server) scan(c *call, v any) (any, error) {
	switch t := v.(type) {
	case string:
		if isFileRefLike(t) {
			return s.normalizeRef(c, t)
		}
		return t, nil
	case []any:
		for i := range t {
			next, err := s.scan(c, t[i])
			if err != nil {
				return nil, err
			}
			t[i] = next
		}
		return t, nil
	case map[string]any:
		for k := range t {
			next, err := s.scan(c, t[k])
			if err != nil {
				return nil, err
			}
			t[k] = next
		}
		return t, nil
	default:
		return v, nil
	}
}

func decodeJSONObject(raw json.RawMessage, what string) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // large integers must survive the round trip
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, errs.Newf(errs.CodeInvalidArgument, "%s must be a JSON object", what)
	}
	return out, nil
}

// checkObject rewrites the references in a JSON object: isFile names the top-level
// keys that must hold one, and every other value is scanned for what looks like one.
func (s *server) checkObject(c *call, raw json.RawMessage, what string, isFile func(key string) bool) (json.RawMessage, error) {
	object, err := decodeJSONObject(raw, what)
	if err != nil || object == nil {
		return raw, err
	}
	for key, value := range object {
		if isFile(key) {
			text, ok := value.(string)
			if !ok {
				return nil, errs.Newf(errs.CodeInvalidArgument, "input %q must be a file reference", key)
			}
			canonical, err := s.normalizeRef(c, text)
			if err != nil {
				return nil, err
			}
			object[key] = canonical
			continue
		}
		next, err := s.scan(c, value)
		if err != nil {
			return nil, err
		}
		object[key] = next
	}
	return json.Marshal(object)
}

// checkInputs enforces R20 on a run's inputs against the workflow's declared types.
func (s *server) checkInputs(c *call, specs map[string]wfdef.InputSpec, raw json.RawMessage) (json.RawMessage, error) {
	return s.checkObject(c, raw, "inputs", func(key string) bool {
		spec, declared := specs[key]
		return declared && spec.Type == wfdef.InputFile
	})
}

// checkNodeParameters enforces R20 on an ad-hoc node's parameters. In the node
// catalog, `source` is the file reference.
func (s *server) checkNodeParameters(c *call, raw json.RawMessage) (json.RawMessage, error) {
	return s.checkObject(c, raw, "parameters", func(key string) bool { return key == "source" })
}

// checkInlineDefinition validates an inline workflow's literal references: node
// parameters and input defaults. It only checks; the submitted bytes stay what they
// are, because the job's snapshot hash covers them (§6.5).
func (s *server) checkInlineDefinition(c *call, def *wfdef.Definition) error {
	for _, node := range def.Nodes {
		if _, err := s.checkObject(c, node.Parameters, "node parameters", func(string) bool { return false }); err != nil {
			return err
		}
	}
	for name, input := range def.Inputs {
		if len(input.Default) == 0 {
			continue
		}
		if input.Type == wfdef.InputFile {
			var text string
			if err := json.Unmarshal(input.Default, &text); err != nil {
				return errs.Newf(errs.CodeInvalidArgument, "the default of input %q must be a file reference", name)
			}
			if _, err := s.normalizeRef(c, text); err != nil {
				return err
			}
			continue
		}
		var value any
		if err := json.Unmarshal(input.Default, &value); err != nil {
			return errs.Newf(errs.CodeInvalidArgument, "the default of input %q is not valid JSON", name)
		}
		if _, err := s.scan(c, value); err != nil {
			return err
		}
	}
	return nil
}
