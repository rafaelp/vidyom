package workflow

import (
	"encoding/json"
	"sort"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// BindOptions controls input binding.
type BindOptions struct {
	// File maps a declared file input's value to its canonical form, or refuses it.
	// Nil accepts the value as is.
	File func(name, value string) (string, error)
	// AllowMissing leaves required inputs out instead of failing, for --dry-run on a
	// workflow given no inputs (§11.3).
	AllowMissing bool
}

// BindInputs applies defaults, required flags and declared types (§6.2) and returns
// the job's inputs. Undeclared inputs are refused (input isolation, §6.2).
func BindInputs(def *wfdef.Definition, raw json.RawMessage, opts BindOptions) (map[string]any, error) {
	given := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		v, err := DecodeJSON(raw)
		if err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "inputs are not valid JSON")
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, errs.New(errs.CodeInvalidArgument, "inputs must be a JSON object")
		}
		given = m
	}
	names := make([]string, 0, len(given))
	for k := range given {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		if _, ok := def.Inputs[k]; !ok {
			return nil, errs.Newf(errs.CodeInvalidArgument, "workflow %s does not declare input %q", def.ID, k)
		}
	}
	out := make(map[string]any, len(def.Inputs))
	for _, name := range sortedSpecKeys(def.Inputs) {
		spec := def.Inputs[name]
		val, ok := given[name]
		if !ok || val == nil {
			if len(spec.Default) > 0 {
				d, err := DecodeJSON(spec.Default)
				if err != nil {
					return nil, errs.Wrap(errs.CodeInternal, err, "invalid input default")
				}
				out[name] = d
				continue
			}
			if spec.Required && !opts.AllowMissing {
				return nil, errs.Newf(errs.CodeInvalidArgument, "input %q is required", name)
			}
			continue
		}
		if err := checkInputType(spec.Type, val); err != nil {
			return nil, errs.Newf(errs.CodeInvalidArgument, "input %q: %v", name, err)
		}
		if spec.Type == wfdef.InputFile && opts.File != nil {
			canonical, err := opts.File(name, val.(string))
			if err != nil {
				return nil, err
			}
			val = canonical
		}
		out[name] = val
	}
	return out, nil
}
