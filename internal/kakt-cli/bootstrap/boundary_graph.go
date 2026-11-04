package bootstrap

import (
	"fmt"
	"sort"
)

// The boundaries form a directed dependency graph, and it must be acyclic.
//
// Ordering alone is not enough to check. Three deadlocks in one day had the same
// shape -- A needs something from B, and B is gated behind a gate that waits on
// A -- and each was found only as a fifteen-minute timeout on a live cluster:
//
//	03 hub-operator ── needs SpokeMachineIdentity CRD ──> 04 spoke-identity-operator
//	04              ── needs the database roles       ──> 03 hub-operator
//
// A comparison of boundary numbers catches the two-node case and misses every
// longer one (03 → 05 → 04 → 03). So the graph is built from the capabilities
// boundaries declare and checked for cycles properly, which catches a cycle of
// any length and keeps working as boundaries are added.
//
// The limit is worth stating: this checks DECLARED dependencies. A component
// that needs something at runtime without the boundary declaring it -- which is
// exactly how the CRD deadlock above escaped -- is invisible here. That is an
// argument for declaring such dependencies, not for trusting the check to find
// what nobody wrote down.

// boundaryEdge is "producer must complete before consumer starts".
type boundaryEdge struct {
	from, to int
	via      capability
}

// dependencyEdges derives the graph from what boundaries yield and require.
//
// A capability nothing declares as its own is produced OUTSIDE the boundary
// sequence -- the database roles are created by the hub-operator after phase 11f,
// not by any boundary -- and contributes no edge. Requiring one is still
// legitimate; there is simply no boundary to order against.
func dependencyEdges(cs []boundaryContract) []boundaryEdge {
	producer := map[capability]int{}
	for _, c := range cs {
		for _, chk := range c.readiness.checks {
			if chk.capability != "" {
				producer[chk.capability] = c.number
			}
		}
	}

	var edges []boundaryEdge
	for _, c := range cs {
		for _, req := range c.requires.checks {
			if req.capability == "" {
				continue
			}
			if p, ok := producer[req.capability]; ok {
				edges = append(edges, boundaryEdge{from: p, to: c.number, via: req.capability})
			}
		}
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		return edges[i].to < edges[j].to
	})
	return edges
}

// validateBoundaryGraph refuses a dependency graph that cannot be satisfied.
//
// Two failures, and they are different:
//
//   - a CYCLE, which no ordering can satisfy;
//   - an edge pointing BACKWARDS or at itself, which a topological order could
//     satisfy but this platform cannot, because boundaries run in numeric order.
func validateBoundaryGraph(cs []boundaryContract) error {
	edges := dependencyEdges(cs)

	if cycle := findCycle(edges); cycle != nil {
		return fmt.Errorf("the boundaries depend on each other in a cycle that no "+
			"ordering can satisfy: %s", renderCycle(cycle, edges))
	}

	for _, e := range edges {
		if e.from >= e.to {
			return fmt.Errorf("boundary %d requires %q, which boundary %d produces. "+
				"Boundaries run in order, so %d has not run when %d starts",
				e.to, e.via, e.from, e.from, e.to)
		}
	}
	return nil
}

// findCycle returns the boundaries on a cycle, or nil. Iterative-free DFS with
// the standard white/grey/black colouring.
func findCycle(edges []boundaryEdge) []int {
	adj := map[int][]int{}
	nodes := map[int]bool{}
	for _, e := range edges {
		adj[e.from] = append(adj[e.from], e.to)
		nodes[e.from], nodes[e.to] = true, true
	}

	const (
		white = 0
		grey  = 1
		black = 2
	)
	colour := map[int]int{}
	var stack []int
	var found []int

	var visit func(int) bool
	visit = func(n int) bool {
		colour[n] = grey
		stack = append(stack, n)
		for _, m := range adj[n] {
			switch colour[m] {
			case grey:
				// Cycle: take the stack from m onwards.
				for i, x := range stack {
					if x == m {
						found = append([]int{}, stack[i:]...)
						break
					}
				}
				return true
			case white:
				if visit(m) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		colour[n] = black
		return false
	}

	ordered := make([]int, 0, len(nodes))
	for n := range nodes {
		ordered = append(ordered, n)
	}
	sort.Ints(ordered)
	for _, n := range ordered {
		if colour[n] == white && visit(n) {
			return found
		}
	}
	return nil
}

// renderCycle names the capability on each hop, so the message says what the
// boundaries actually want from each other rather than only that they loop.
func renderCycle(cycle []int, edges []boundaryEdge) string {
	via := func(from, to int) capability {
		for _, e := range edges {
			if e.from == from && e.to == to {
				return e.via
			}
		}
		return ""
	}
	out := ""
	for i, n := range cycle {
		next := cycle[(i+1)%len(cycle)]
		out += fmt.Sprintf("boundary %d ──(%s)──> ", n, via(n, next))
	}
	return out + fmt.Sprintf("boundary %d", cycle[0])
}
