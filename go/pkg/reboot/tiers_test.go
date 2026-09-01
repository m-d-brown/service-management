package reboot

import (
	"bytes"
	"strings"
	"testing"
)

// topology is the fixture most ordering tests work against: one hypervisor,
// two guests behind it, and a service behind one of those.
func topology() Hosts {
	return Hosts{
		"hv1":  {Name: "hv1"},
		"vm-a": {Name: "vm-a", After: []string{"hv1"}},
		"vm-b": {Name: "vm-b", After: []string{"hv1"}},
		"web":  {Name: "web", After: []string{"vm-a"}},
	}
}

// joinTiers renders tiers as "a,b | c" for compact comparison.
func joinTiers(tiers [][]string) string {
	rendered := make([]string, 0, len(tiers))
	for _, tier := range tiers {
		rendered = append(rendered, strings.Join(tier, ","))
	}
	return strings.Join(rendered, " | ")
}

func TestBuildTiers(t *testing.T) {
	tests := []struct {
		name    string
		hosts   Hosts
		targets []string
		want    string
	}{
		{
			name:    "parents first, dependents last",
			hosts:   topology(),
			targets: []string{"web", "vm-a", "vm-b", "hv1"},
			want:    "hv1 | vm-a,vm-b | web",
		},
		{
			// Constraints pointing outside the target set are ignored, which is
			// what makes a narrow run over part of a topology possible.
			name:    "untargeted dependency is ignored",
			hosts:   topology(),
			targets: []string{"vm-a", "web"},
			want:    "vm-a | web",
		},
		{
			name:    "independent hosts share one tier",
			hosts:   topology(),
			targets: []string{"vm-a", "vm-b"},
			want:    "vm-a,vm-b",
		},
		{
			name:    "single host",
			hosts:   topology(),
			targets: []string{"hv1"},
			want:    "hv1",
		},
		{
			name: "diamond collapses to three tiers",
			hosts: Hosts{
				"root":  {Name: "root"},
				"left":  {Name: "left", After: []string{"root"}},
				"right": {Name: "right", After: []string{"root"}},
				"leaf":  {Name: "leaf", After: []string{"left", "right"}},
			},
			targets: []string{"leaf", "left", "right", "root"},
			want:    "root | left,right | leaf",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tiers, err := BuildTiers(tt.hosts, tt.targets)
			if err != nil {
				t.Fatal(err)
			}
			if got := joinTiers(tiers); got != tt.want {
				t.Errorf("BuildTiers() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuildTiersDetectsCycle(t *testing.T) {
	hosts := Hosts{
		"a": {Name: "a", After: []string{"c"}},
		"b": {Name: "b", After: []string{"a"}},
		"c": {Name: "c", After: []string{"b"}},
	}
	// A cycle has no valid ordering at all; reporting it beats hanging or
	// silently dropping hosts from the run.
	_, err := BuildTiers(hosts, []string{"a", "b", "c"})
	if err == nil {
		t.Fatal("BuildTiers() succeeded on a cycle, want error")
	}
	if !strings.Contains(err.Error(), "cyclic ordering") {
		t.Errorf("error = %q, want it to mention a cyclic ordering", err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error = %q, want it to name host %q", err, name)
		}
	}
}

func TestBuildTiersEmptyTargets(t *testing.T) {
	tiers, err := BuildTiers(topology(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tiers) != 0 {
		t.Errorf("BuildTiers() = %v, want no tiers", tiers)
	}
}

func TestPrintTree(t *testing.T) {
	var out bytes.Buffer
	PrintTree(&out, topology(), []string{"hv1", "vm-a", "vm-b", "web"})

	want := strings.Join([]string{
		"└── hv1",
		"    ├── vm-a",
		"    │   └── web",
		"    └── vm-b",
		"",
	}, "\n")
	if out.String() != want {
		t.Errorf("PrintTree() =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPrintTreeMultipleRoots(t *testing.T) {
	hosts := Hosts{
		"hv1":  {Name: "hv1"},
		"hv2":  {Name: "hv2"},
		"vm-a": {Name: "vm-a", After: []string{"hv1"}},
	}
	var out bytes.Buffer
	PrintTree(&out, hosts, []string{"hv1", "hv2", "vm-a"})

	want := strings.Join([]string{
		"├── hv1",
		"│   └── vm-a",
		"└── hv2",
		"",
	}, "\n")
	if out.String() != want {
		t.Errorf("PrintTree() =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPrintTreeMarksRepeatedHost(t *testing.T) {
	// A host behind two parents appears under both, but is only expanded once
	// so a wide topology cannot print its subtree over and over.
	hosts := Hosts{
		"hv1":  {Name: "hv1"},
		"hv2":  {Name: "hv2"},
		"vm-a": {Name: "vm-a", After: []string{"hv1", "hv2"}},
	}
	var out bytes.Buffer
	PrintTree(&out, hosts, []string{"hv1", "hv2", "vm-a"})

	if got := strings.Count(out.String(), "vm-a"); got != 2 {
		t.Errorf("vm-a appears %d times, want 2", got)
	}
	if !strings.Contains(out.String(), "vm-a (already listed)") {
		t.Errorf("PrintTree() =\n%s\nwant the repeat marked as already listed", out.String())
	}
}

func TestBuildTiersOrdersHostingLikeAnOrdering(t *testing.T) {
	// Hosting implies its ordering, so a guest never reboots before the machine
	// underneath it, whether that was written as after or as runs-on.
	hosts := Hosts{
		"hv1":   {Name: "hv1"},
		"vm-a":  {Name: "vm-a", RunsOn: "hv1"},
		"vm-b":  {Name: "vm-b", RunsOn: "hv1"},
		"ctr-1": {Name: "ctr-1", RunsOn: "vm-a"},
	}
	tiers, err := BuildTiers(hosts, []string{"ctr-1", "vm-a", "vm-b", "hv1"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := joinTiers(tiers), "hv1 | vm-a,vm-b | ctr-1"; got != want {
		t.Errorf("BuildTiers() = %q, want %q", got, want)
	}
}

func TestBuildTiersSeparatesExcludedHosts(t *testing.T) {
	tests := []struct {
		name    string
		hosts   Hosts
		targets []string
		want    string
	}{
		{
			// The pair would otherwise share a tier and go down together,
			// which for an HA pair is the one outcome to avoid. Neither is
			// ordered before the other; name order decides, deterministically.
			name: "an HA pair is split across tiers",
			hosts: Hosts{
				"dns1": {Name: "dns1", NotWith: []string{"dns2"}},
				"dns2": {Name: "dns2"},
			},
			targets: []string{"dns1", "dns2"},
			want:    "dns1 | dns2",
		},
		{
			// Only the pair is separated. A host with no exclusion of its own
			// stays in the first tier it was ready for.
			name: "unrelated hosts are not delayed",
			hosts: Hosts{
				"dns1": {Name: "dns1", NotWith: []string{"dns2"}},
				"dns2": {Name: "dns2"},
				"web":  {Name: "web"},
			},
			targets: []string{"dns1", "dns2", "web"},
			want:    "dns1,web | dns2",
		},
		{
			// Three mutually exclusive hosts need three tiers: admitting one
			// rules out both others for that round.
			name: "a quorum goes one at a time",
			hosts: Hosts{
				"etcd1": {Name: "etcd1", NotWith: []string{"etcd2", "etcd3"}},
				"etcd2": {Name: "etcd2", NotWith: []string{"etcd3"}},
				"etcd3": {Name: "etcd3"},
			},
			targets: []string{"etcd1", "etcd2", "etcd3"},
			want:    "etcd1 | etcd2 | etcd3",
		},
		{
			// A host held back by an exclusion carries everything behind it,
			// which is what makes thinning the ready set inside Kahn's loop
			// correct where splitting finished tiers afterwards would not be.
			name: "a delayed host delays its own dependents",
			hosts: Hosts{
				"dns1": {Name: "dns1", NotWith: []string{"dns2"}},
				"dns2": {Name: "dns2"},
				"web":  {Name: "web", After: []string{"dns2"}},
			},
			targets: []string{"dns1", "dns2", "web"},
			want:    "dns1 | dns2 | web",
		},
		{
			// Written on either host, the exclusion binds both.
			name: "the exclusion is read from either end",
			hosts: Hosts{
				"dns1": {Name: "dns1"},
				"dns2": {Name: "dns2", NotWith: []string{"dns1"}},
			},
			targets: []string{"dns1", "dns2"},
			want:    "dns1 | dns2",
		},
		{
			// An exclusion naming a host this run does not touch constrains
			// nothing, the same way an ordering to an untargeted host does not.
			name: "an untargeted exclusion is ignored",
			hosts: Hosts{
				"dns1": {Name: "dns1", NotWith: []string{"dns2"}},
				"dns2": {Name: "dns2"},
				"web":  {Name: "web"},
			},
			targets: []string{"dns1", "web"},
			want:    "dns1,web",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tiers, err := BuildTiers(tt.hosts, tt.targets)
			if err != nil {
				t.Fatal(err)
			}
			if got := joinTiers(tiers); got != tt.want {
				t.Errorf("BuildTiers() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPrintTreeMarksHostingBranches(t *testing.T) {
	// The nesting alone cannot say which edge put a host under a parent, and
	// the two mean different things: the hosting branch is the one that will
	// not cost a reboot of its own.
	hosts := Hosts{
		"hv1":  {Name: "hv1"},
		"vm-a": {Name: "vm-a", RunsOn: "hv1"},
		"web":  {Name: "web", After: []string{"vm-a"}},
	}
	var out bytes.Buffer
	PrintTree(&out, hosts, []string{"hv1", "vm-a", "web"})

	want := strings.Join([]string{
		"└── hv1",
		"    └── vm-a (runs on hv1)",
		"        └── web",
		"",
	}, "\n")
	if out.String() != want {
		t.Errorf("PrintTree() =\n%s\nwant\n%s", out.String(), want)
	}
}

func TestPrintTreeListsExclusions(t *testing.T) {
	// An exclusion produces no nesting, so the tree cannot show it — yet it
	// changes what the run does. Unsaid, a plan that correctly separated an HA
	// pair would read exactly like one that had forgotten the pair existed.
	hosts := Hosts{
		"dns1": {Name: "dns1", NotWith: []string{"dns2"}},
		"dns2": {Name: "dns2"},
		"web":  {Name: "web"},
	}
	var out bytes.Buffer
	PrintTree(&out, hosts, []string{"dns1", "dns2", "web"})

	if !strings.Contains(out.String(), "Never rebooted together: dns1 / dns2") {
		t.Errorf("PrintTree() =\n%s\nwant the exclusion listed", out.String())
	}
	// A pair is named once, from whichever end declared it.
	if got := strings.Count(out.String(), "dns1 / dns2"); got != 1 {
		t.Errorf("the pair is listed %d times, want once", got)
	}
}

func TestPrintTreeSaysNothingWithoutExclusions(t *testing.T) {
	var out bytes.Buffer
	PrintTree(&out, topology(), []string{"hv1", "vm-a"})
	if strings.Contains(out.String(), "Never rebooted together") {
		t.Errorf("PrintTree() =\n%s\nwant no exclusion line when there are none", out.String())
	}
}
