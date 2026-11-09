package workflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"cel.dev/cel-go/cel"
	celast "cel.dev/cel-go/common/ast"
	"cel.dev/cel-go/common/types"
)

// Limits of condition evaluation (I23).
const (
	CELCostLimit   = 100_000
	CELEvalTimeout = 100 * time.Millisecond
	celMaxSource   = 4096
)

// Condition is a compiled CEL condition (D2). It is safe for concurrent use.
type Condition struct {
	Source string
	prg    cel.Program
	// Nodes, Inputs, UsesEvent and UsesJob are what the expression reads, so the
	// validator can apply R24 to conditions too.
	Nodes     []string
	Inputs    []string
	UsesEvent bool
	UsesJob   bool
	// DynamicNodes is set when a node is indexed with a computed key, which makes
	// the ancestor check impossible.
	DynamicNodes bool
}

func newEnv(names ...string) (*cel.Env, error) {
	opts := []cel.EnvOption{cel.CrossTypeNumericComparisons(true)}
	for _, n := range names {
		opts = append(opts, cel.Variable(n, cel.DynType))
	}
	return cel.NewEnv(opts...)
}

var (
	conditionEnv = sync.OnceValues(func() (*cel.Env, error) { return newEnv("inputs", "nodes", "event", "job") })
	filterEnv    = sync.OnceValues(func() (*cel.Env, error) { return newEnv("event") })
	celCache     sync.Map // kind + "\x00" + source -> *Condition
)

// CompileCondition compiles a flow.if condition: it sees inputs, nodes, event and
// job (I23).
func CompileCondition(source string) (*Condition, error) {
	return compile("cond", source, conditionEnv)
}

// CompileFilter compiles an event trigger filter: it sees only event (§8.2).
func CompileFilter(source string) (*Condition, error) {
	return compile("filter", source, filterEnv)
}

func compile(kind, source string, envFn func() (*cel.Env, error)) (*Condition, error) {
	key := kind + "\x00" + source
	if c, ok := celCache.Load(key); ok {
		return c.(*Condition), nil
	}
	if source == "" {
		return nil, errors.New("empty CEL expression")
	}
	if len(source) > celMaxSource {
		return nil, fmt.Errorf("CEL expression longer than %d bytes", celMaxSource)
	}
	env, err := envFn()
	if err != nil {
		return nil, err
	}
	ast, issues := env.Compile(source)
	if issues != nil && issues.Err() != nil {
		return nil, issues.Err()
	}
	if out := ast.OutputType(); out != nil && !out.IsAssignableType(cel.BoolType) && out != cel.DynType {
		return nil, fmt.Errorf("CEL expression must be boolean, not %s", out)
	}
	prg, err := env.Program(ast,
		cel.CostLimit(CELCostLimit),
		cel.InterruptCheckFrequency(64),
		cel.EvalOptions(cel.OptTrackCost),
	)
	if err != nil {
		return nil, err
	}
	c := &Condition{Source: source, prg: prg}
	inspect(ast, c)
	celCache.Store(key, c)
	return c, nil
}

func inspect(ast *cel.Ast, c *Condition) {
	nodes := map[string]bool{}
	inputs := map[string]bool{}
	literalString := func(e celast.Expr) (string, bool) {
		if e.Kind() != celast.LiteralKind {
			return "", false
		}
		s, ok := e.AsLiteral().(types.String)
		return string(s), ok
	}
	isIdent := func(e celast.Expr, name string) bool {
		return e.Kind() == celast.IdentKind && e.AsIdent() == name
	}
	celast.PreOrderVisit(ast.NativeRep().Expr(), celast.NewExprVisitor(func(e celast.Expr) {
		switch e.Kind() {
		case celast.IdentKind:
			switch e.AsIdent() {
			case "event":
				c.UsesEvent = true
			case "job":
				c.UsesJob = true
			}
		case celast.SelectKind:
			sel := e.AsSelect()
			if isIdent(sel.Operand(), "nodes") {
				nodes[sel.FieldName()] = true
			}
			if isIdent(sel.Operand(), "inputs") {
				inputs[sel.FieldName()] = true
			}
		case celast.CallKind:
			call := e.AsCall()
			if call.FunctionName() == "_[_]" && len(call.Args()) == 2 {
				if isIdent(call.Args()[0], "nodes") {
					if s, ok := literalString(call.Args()[1]); ok {
						nodes[s] = true
					} else {
						c.DynamicNodes = true
					}
				}
				if isIdent(call.Args()[0], "inputs") {
					if s, ok := literalString(call.Args()[1]); ok {
						inputs[s] = true
					}
				}
			}
		}
	}))
	c.Nodes = sortedKeys(nodes)
	c.Inputs = sortedKeys(inputs)
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Eval evaluates the condition against a job's scope. Evaluation errors, including
// exceeding the cost limit, fail the node (I23).
func (c *Condition) Eval(ctx context.Context, s Scope) (bool, error) {
	nodes := make(map[string]any, len(s.Nodes))
	for id, n := range s.Nodes {
		nodes[id] = map[string]any{"output": normalizeForCEL(n.Output), "status": n.Status, "error_code": n.ErrorCode}
	}
	vars := map[string]any{
		"inputs": orEmpty(normalizeForCEL(s.Inputs)),
		"nodes":  nodes,
		"event":  orEmpty(normalizeForCEL(s.Event)),
		"job":    orEmpty(normalizeForCEL(s.Job)),
	}
	return c.eval(ctx, vars)
}

// EvalEvent evaluates a trigger filter against an event as decoded JSON.
func (c *Condition) EvalEvent(ctx context.Context, event map[string]any) (bool, error) {
	return c.eval(ctx, map[string]any{"event": orEmpty(normalizeForCEL(event))})
}

func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok && m == nil {
		return map[string]any{}
	}
	return v
}

func (c *Condition) eval(ctx context.Context, vars map[string]any) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, CELEvalTimeout)
	defer cancel()
	out, _, err := c.prg.ContextEval(ctx, vars)
	if err != nil {
		return false, fmt.Errorf("evaluate condition: %w", err)
	}
	b, ok := out.Value().(bool)
	if !ok {
		return false, fmt.Errorf("condition did not produce a boolean, got %T", out.Value())
	}
	return b, nil
}

// normalizeForCEL turns json.Number into int64 or float64, which CEL understands.
func normalizeForCEL(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalizeForCEL(e)
		}
		return out
	case map[string]any:
		if x == nil {
			return nil
		}
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeForCEL(e)
		}
		return out
	default:
		return v
	}
}
