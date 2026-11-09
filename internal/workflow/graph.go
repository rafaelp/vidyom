package workflow

import (
	"fmt"
	"sort"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/wfdef"
)

// Graph is the DAG of a workflow (§6.1, D33).
type Graph struct {
	// Order is a topological order, stable with respect to the definition.
	Order []string
	Preds map[string][]string
	Succs map[string][]string
	// Index gives a node's position in the definition.
	Index map[string]int

	ancestors map[string]map[string]bool
}

// BuildGraph builds the graph. It reports duplicate IDs, connections naming
// unknown nodes and cycles as errors, all at once.
func BuildGraph(def *wfdef.Definition) (*Graph, []error) {
	var problems []error
	g := &Graph{
		Preds: map[string][]string{},
		Succs: map[string][]string{},
		Index: map[string]int{},
	}
	for i, n := range def.Nodes {
		if _, dup := g.Index[n.ID]; dup {
			problems = append(problems, fmt.Errorf("duplicate node ID %q", n.ID))
			continue
		}
		g.Index[n.ID] = i
	}
	from := make([]string, 0, len(def.Connections))
	for k := range def.Connections {
		from = append(from, k)
	}
	sort.Strings(from)
	for _, src := range from {
		if _, ok := g.Index[src]; !ok {
			problems = append(problems, fmt.Errorf("connections names unknown node %q", src))
			continue
		}
		for _, dst := range def.Connections[src] {
			if _, ok := g.Index[dst]; !ok {
				problems = append(problems, fmt.Errorf("connections from %q names unknown node %q", src, dst))
				continue
			}
			g.Succs[src] = append(g.Succs[src], dst)
			g.Preds[dst] = append(g.Preds[dst], src)
		}
	}
	// Kahn's algorithm, always picking the earliest node in definition order.
	indeg := make(map[string]int, len(g.Index))
	for id := range g.Index {
		indeg[id] = len(g.Preds[id])
	}
	var ready []string
	for id, d := range indeg {
		if d == 0 {
			ready = append(ready, id)
		}
	}
	byIndex := func(s []string) {
		sort.Slice(s, func(i, j int) bool { return g.Index[s[i]] < g.Index[s[j]] })
	}
	for len(ready) > 0 {
		byIndex(ready)
		id := ready[0]
		ready = ready[1:]
		g.Order = append(g.Order, id)
		for _, s := range g.Succs[id] {
			indeg[s]--
			if indeg[s] == 0 {
				ready = append(ready, s)
			}
		}
	}
	if len(g.Order) != len(g.Index) {
		var stuck []string
		for id, d := range indeg {
			if d > 0 {
				stuck = append(stuck, id)
			}
		}
		byIndex(stuck)
		problems = append(problems, fmt.Errorf("connections form a cycle through %s", strings.Join(stuck, ", ")))
		return g, problems
	}
	g.ancestors = make(map[string]map[string]bool, len(g.Order))
	for _, id := range g.Order {
		set := map[string]bool{}
		for _, p := range g.Preds[id] {
			set[p] = true
			for a := range g.ancestors[p] {
				set[a] = true
			}
		}
		g.ancestors[id] = set
	}
	return g, problems
}

// IsAncestor reports whether a precedes b on some path (R24).
func (g *Graph) IsAncestor(a, b string) bool { return g.ancestors[b][a] }

// Dominates reports whether every path from a root to b passes through a (D53).
func (g *Graph) Dominates(a, b string) bool {
	if a == b {
		return true
	}
	if !g.IsAncestor(a, b) {
		return false
	}
	seen := map[string]bool{a: true}
	var stack []string
	for _, id := range g.Order {
		if len(g.Preds[id]) == 0 && id != a {
			stack = append(stack, id)
			seen[id] = true
		}
	}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == b {
			return false
		}
		for _, s := range g.Succs[id] {
			if !seen[s] {
				seen[s] = true
				stack = append(stack, s)
			}
		}
	}
	return true
}
