package reboot

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// BuildTiers groups the targeted hosts into sequential execution tiers using
// Kahn's algorithm over their ordering constraints, then thins each tier by the
// exclusions its members declare.
//
// Every host in tier N is guaranteed to have all of its targeted predecessors
// in tiers 1..N-1, so a tier can be rebooted in parallel — and no tier contains
// two hosts that said they must never go down together. Constraints pointing at
// hosts outside the target set are ignored, which is what makes a narrow run
// over part of a topology possible.
func BuildTiers(hosts Hosts, targets []string) ([][]string, error) {
	targeted := map[string]bool{}
	for _, name := range targets {
		targeted[name] = true
	}

	// Remaining unsatisfied predecessors per targeted host. Hosting implies an
	// ordering, so a guest waits for the hypervisor it runs on without having
	// to also declare that it does.
	pending := map[string]map[string]bool{}
	for name := range targeted {
		deps := map[string]bool{}
		for _, before := range hosts[name].predecessors() {
			if targeted[before] && before != name {
				deps[before] = true
			}
		}
		pending[name] = deps
	}

	var tiers [][]string
	for len(pending) > 0 {
		var ready []string
		for name, deps := range pending {
			if len(deps) == 0 {
				ready = append(ready, name)
			}
		}
		if len(ready) == 0 {
			return nil, fmt.Errorf("cyclic ordering between hosts: %s",
				strings.Join(slices.Sorted(maps.Keys(pending)), ", "))
		}
		slices.Sort(ready)

		tier := admit(hosts, ready)
		tiers = append(tiers, tier)

		for _, name := range tier {
			delete(pending, name)
		}
		for _, deps := range pending {
			for _, name := range tier {
				delete(deps, name)
			}
		}
	}
	return tiers, nil
}

// admit selects the hosts of a ready set that may go down together, taking them
// in name order and holding back any that excludes one already admitted.
//
// Exclusions thin a tier rather than reordering anything. A host held back has
// no constraint of its own to satisfy — it is simply still ready next round,
// which is where it lands. Doing it inside Kahn's loop rather than by splitting
// finished tiers afterwards is what keeps the result correct: a host released
// only once all its predecessors are done cannot be pulled earlier by a split
// upstream of it, so delaying one delays everything behind it automatically.
//
// Progress is guaranteed because the first host of a non-empty ready set is
// always admitted: Validate has already rejected a host that excludes itself.
func admit(hosts Hosts, ready []string) []string {
	var tier []string
	for _, name := range ready {
		if !slices.ContainsFunc(tier, func(other string) bool { return hosts.Excludes(name, other) }) {
			tier = append(tier, name)
		}
	}
	return tier
}

// PrintTree writes the targeted hosts as a tree showing reboot order, parents
// first and nested dependents last, followed by any pair the plan will keep
// apart.
func PrintTree(out io.Writer, hosts Hosts, targets []string) {
	targeted := map[string]bool{}
	for _, name := range targets {
		targeted[name] = true
	}

	children := map[string][]string{}
	var roots []string
	for _, name := range sortedTargets(targets) {
		isRoot := true
		for _, before := range hosts[name].predecessors() {
			if targeted[before] && before != name {
				children[before] = append(children[before], name)
				isRoot = false
			}
		}
		if isRoot {
			roots = append(roots, name)
		}
	}

	visited := map[string]bool{}
	var walk func(name, parent, prefix string, last bool)
	walk = func(name, parent, prefix string, last bool) {
		connector := "├── "
		if last {
			connector = "└── "
		}
		if visited[name] {
			report(out, "%s%s%s (already listed)\n", prefix, connector, name)
			return
		}
		visited[name] = true
		// A host can hang under one parent it runs on and another it is merely
		// ordered after, and the nesting alone cannot say which edge put it
		// there. The annotation marks the branch where the reboot is carried,
		// which is the one that will not cost a reboot of its own.
		label := name
		if hosts[name].RunsOn == parent && parent != "" {
			label = name + " (runs on " + parent + ")"
		}
		report(out, "%s%s%s\n", prefix, connector, label)

		kids := children[name]
		if len(kids) == 0 {
			return
		}
		nested := prefix + "│   "
		if last {
			nested = prefix + "    "
		}
		for i, kid := range kids {
			walk(kid, name, nested, i == len(kids)-1)
		}
	}
	for i, root := range roots {
		walk(root, "", "", i == len(roots)-1)
	}

	printExclusions(out, hosts, targets)
}

// printExclusions lists the targeted pairs that will not reboot together.
//
// The tree cannot show them: an exclusion is not an edge and produces no
// nesting, yet it changes what the run does, since the two land in different
// tiers. Left unsaid, a plan that correctly separated an HA pair would read
// exactly like one that had forgotten the pair existed.
func printExclusions(out io.Writer, hosts Hosts, targets []string) {
	names := sortedTargets(targets)
	var pairs []string
	for i, a := range names {
		for _, b := range names[i+1:] {
			if hosts.Excludes(a, b) {
				pairs = append(pairs, a+" / "+b)
			}
		}
	}
	if len(pairs) == 0 {
		return
	}
	report(out, "Never rebooted together: %s\n", strings.Join(pairs, ", "))
}

// sortedTargets returns a sorted copy of the target names.
func sortedTargets(targets []string) []string {
	return slices.Sorted(slices.Values(targets))
}
