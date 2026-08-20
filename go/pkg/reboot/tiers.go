package reboot

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// BuildTiers groups the targeted hosts into sequential execution tiers using
// Kahn's algorithm over their After constraints.
//
// Every host in tier N is guaranteed to have all of its targeted predecessors
// in tiers 1..N-1, so a tier can be rebooted in parallel. Constraints pointing
// at hosts outside the target set are ignored, which is what makes a narrow run
// over part of a topology possible.
func BuildTiers(hosts Hosts, targets []string) ([][]string, error) {
	targeted := map[string]bool{}
	for _, name := range targets {
		targeted[name] = true
	}

	// Remaining unsatisfied predecessors per targeted host.
	pending := map[string]map[string]bool{}
	for name := range targeted {
		deps := map[string]bool{}
		for _, after := range hosts[name].After {
			if targeted[after] && after != name {
				deps[after] = true
			}
		}
		pending[name] = deps
	}

	var tiers [][]string
	for len(pending) > 0 {
		var tier []string
		for name, deps := range pending {
			if len(deps) == 0 {
				tier = append(tier, name)
			}
		}
		if len(tier) == 0 {
			return nil, fmt.Errorf("cyclic ordering between hosts: %s",
				strings.Join(slices.Sorted(maps.Keys(pending)), ", "))
		}
		slices.Sort(tier)
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

// PrintTree writes the targeted hosts as a tree showing reboot order, parents
// first and nested dependents last.
func PrintTree(out io.Writer, hosts Hosts, targets []string) {
	targeted := map[string]bool{}
	for _, name := range targets {
		targeted[name] = true
	}

	children := map[string][]string{}
	var roots []string
	for _, name := range sortedTargets(targets) {
		isRoot := true
		for _, after := range hosts[name].After {
			if targeted[after] && after != name {
				children[after] = append(children[after], name)
				isRoot = false
			}
		}
		if isRoot {
			roots = append(roots, name)
		}
	}

	visited := map[string]bool{}
	var walk func(name, prefix string, last bool)
	walk = func(name, prefix string, last bool) {
		connector := "├── "
		if last {
			connector = "└── "
		}
		if visited[name] {
			report(out, "%s%s%s (already listed)\n", prefix, connector, name)
			return
		}
		visited[name] = true
		report(out, "%s%s%s\n", prefix, connector, name)

		kids := children[name]
		if len(kids) == 0 {
			return
		}
		nested := prefix + "│   "
		if last {
			nested = prefix + "    "
		}
		for i, kid := range kids {
			walk(kid, nested, i == len(kids)-1)
		}
	}
	for i, root := range roots {
		walk(root, "", i == len(roots)-1)
	}
}

// sortedTargets returns a sorted copy of the target names.
func sortedTargets(targets []string) []string {
	return slices.Sorted(slices.Values(targets))
}
