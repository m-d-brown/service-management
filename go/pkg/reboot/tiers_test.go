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
