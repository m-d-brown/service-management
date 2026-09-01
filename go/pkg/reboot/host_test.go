package reboot

import (
	"reflect"
	"strings"
	"testing"
)

// equalStrings fails the test unless got and want hold the same elements. It
// treats a nil slice and an empty one as equal, which keeps table entries from
// having to distinguish "no value" from "empty value".
func equalStrings(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestMerge(t *testing.T) {
	base := Host{Name: "vm-a", Addr: "10.0.0.21", User: "admin", After: []string{"hv1"}}

	tests := []struct {
		name    string
		overlay Host
		want    Host
	}{
		{
			// Naming a host to target it carries no fields, and must not erase
			// what the piped-in definition already established.
			name:    "bare overlay preserves everything",
			overlay: Host{Name: "vm-a"},
			want:    base,
		},
		{
			name:    "set field overrides, others survive",
			overlay: Host{Name: "vm-a", User: "root"},
			want:    Host{Name: "vm-a", Addr: "10.0.0.21", User: "root", After: []string{"hv1"}},
		},
		{
			// Ordering replaces rather than extends, so constraints can be
			// narrowed and not only added to.
			name:    "explicit ordering replaces",
			overlay: Host{Name: "vm-a", After: []string{"dns1"}},
			want:    Host{Name: "vm-a", Addr: "10.0.0.21", User: "admin", After: []string{"dns1"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Merge(base, tt.overlay); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Merge() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestHostsDependents(t *testing.T) {
	hosts := Hosts{
		"hv1":  {Name: "hv1"},
		"vm-a": {Name: "vm-a", After: []string{"hv1"}},
		"vm-b": {Name: "vm-b", After: []string{"hv1"}},
		"web":  {Name: "web", After: []string{"vm-a"}},
	}
	tests := []struct {
		host string
		want []string
	}{
		{"hv1", []string{"vm-a", "vm-b"}},
		{"vm-a", []string{"web"}},
		{"web", nil},
	}
	for _, tt := range tests {
		t.Run(tt.host, func(t *testing.T) {
			equalStrings(t, "Dependents()", hosts.Dependents(tt.host), tt.want)
		})
	}
}

func TestHostsValidate(t *testing.T) {
	tests := []struct {
		name  string
		hosts Hosts
		want  string
	}{
		{
			name:  "unknown dependency",
			hosts: Hosts{"vm-a": {Name: "vm-a", After: []string{"ghost"}}},
			want:  `"ghost", which is not a known host`,
		},
		{
			name:  "self dependency",
			hosts: Hosts{"vm-a": {Name: "vm-a", After: []string{"vm-a"}}},
			want:  "cannot reboot after itself",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.hosts.Validate()
			if err == nil {
				t.Fatal("Validate() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}

	valid := Hosts{
		"hv1":  {Name: "hv1"},
		"vm-a": {Name: "vm-a", After: []string{"hv1"}},
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("Validate() on a sound topology: %v", err)
	}
}

func TestHostsNamesSorted(t *testing.T) {
	hosts := Hosts{"zeta": {Name: "zeta"}, "alpha": {Name: "alpha"}, "mid": {Name: "mid"}}
	equalStrings(t, "Names()", hosts.Names(), []string{"alpha", "mid", "zeta"})
}

func TestMergeRelationsReplaceIndependently(t *testing.T) {
	// The three relationships are different claims, so an overlay stating one
	// must not silently discard another. Naming an ordering says nothing about
	// where a host runs, and a definition that lost its hosting that way would
	// go on to be rebooted twice.
	base := Host{
		Name:    "vm-a",
		Addr:    "10.0.0.21",
		After:   []string{"dns1"},
		RunsOn:  "hv1",
		NotWith: []string{"vm-b"},
		Ready:   "systemctl is-system-running",
	}

	tests := []struct {
		name    string
		overlay Host
		want    Host
	}{
		{
			name:    "bare overlay preserves every relationship",
			overlay: Host{Name: "vm-a"},
			want:    base,
		},
		{
			name:    "a new ordering leaves hosting alone",
			overlay: Host{Name: "vm-a", After: []string{"dns2"}},
			want: Host{Name: "vm-a", Addr: "10.0.0.21", After: []string{"dns2"},
				RunsOn: "hv1", NotWith: []string{"vm-b"}, Ready: "systemctl is-system-running"},
		},
		{
			name:    "moving a guest leaves its ordering alone",
			overlay: Host{Name: "vm-a", RunsOn: "hv2"},
			want: Host{Name: "vm-a", Addr: "10.0.0.21", After: []string{"dns1"},
				RunsOn: "hv2", NotWith: []string{"vm-b"}, Ready: "systemctl is-system-running"},
		},
		{
			name:    "a readiness command overrides on its own",
			overlay: Host{Name: "vm-a", Ready: "true"},
			want: Host{Name: "vm-a", Addr: "10.0.0.21", After: []string{"dns1"},
				RunsOn: "hv1", NotWith: []string{"vm-b"}, Ready: "true"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Merge(base, tt.overlay); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Merge() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// hosting is the fixture the hosting tests share: two levels of guests under
// one hypervisor, plus a host merely ordered after it.
func hosting() Hosts {
	return Hosts{
		"hv1":   {Name: "hv1"},
		"vm-a":  {Name: "vm-a", RunsOn: "hv1"},
		"vm-b":  {Name: "vm-b", RunsOn: "hv1"},
		"ctr-1": {Name: "ctr-1", RunsOn: "vm-a"},
		"web":   {Name: "web", After: []string{"hv1"}},
	}
}

func TestHostsChildren(t *testing.T) {
	hosts := hosting()
	// web is ordered after hv1 and nothing more, so it is not a child: the
	// distinction is the whole reason both edges exist.
	equalStrings(t, "Children(hv1)", hosts.Children("hv1"), []string{"vm-a", "vm-b"})
	equalStrings(t, "Children(vm-a)", hosts.Children("vm-a"), []string{"ctr-1"})
	equalStrings(t, "Children(web)", hosts.Children("web"), nil)
}

func TestHostsCarriedIsTransitive(t *testing.T) {
	hosts := hosting()
	// Rebooting the hypervisor restarts the guests and everything inside them.
	equalStrings(t, "Carried(hv1)", hosts.Carried([]string{"hv1"}),
		[]string{"ctr-1", "vm-a", "vm-b"})
	equalStrings(t, "Carried(vm-a)", hosts.Carried([]string{"vm-a"}), []string{"ctr-1"})
	// Hosts already in the set are not returned as their own descendants.
	equalStrings(t, "Carried(hv1,vm-a)", hosts.Carried([]string{"hv1", "vm-a"}),
		[]string{"ctr-1", "vm-b"})
	equalStrings(t, "Carried(web)", hosts.Carried([]string{"web"}), nil)
}

func TestHostsExcludesIsSymmetric(t *testing.T) {
	hosts := Hosts{
		"dns1": {Name: "dns1", NotWith: []string{"dns2"}},
		"dns2": {Name: "dns2"},
		"web":  {Name: "web"},
	}
	// Declared on one host, it binds both: requiring it on each would only
	// create the opportunity to write it once and believe it took.
	if !hosts.Excludes("dns1", "dns2") {
		t.Error("Excludes(dns1, dns2) = false, want the declared exclusion honoured")
	}
	if !hosts.Excludes("dns2", "dns1") {
		t.Error("Excludes(dns2, dns1) = false, want the exclusion read from either end")
	}
	if hosts.Excludes("dns1", "web") {
		t.Error("Excludes(dns1, web) = true, want no exclusion between unrelated hosts")
	}
}

func TestHostReadyCommand(t *testing.T) {
	if got := (Host{Name: "web"}).ReadyCommand(); got != "true" {
		t.Errorf("ReadyCommand() = %q, want a bare login test", got)
	}
	declared := Host{Name: "dns1", Ready: "dig +short @127.0.0.1 example.internal"}
	if got := declared.ReadyCommand(); got != declared.Ready {
		t.Errorf("ReadyCommand() = %q, want the declared command %q", got, declared.Ready)
	}
}

func TestHostPredecessorsFoldHostingIntoOrdering(t *testing.T) {
	// Hosting implies the ordering, so a guest waits for its hypervisor without
	// having to also declare that it does...
	implied := Host{Name: "vm-a", RunsOn: "hv1"}
	equalStrings(t, "predecessors", implied.predecessors(), []string{"hv1"})

	// ...and saying both is redundant rather than an error, counted once.
	both := Host{Name: "vm-a", RunsOn: "hv1", After: []string{"hv1", "dns1"}}
	equalStrings(t, "predecessors", both.predecessors(), []string{"hv1", "dns1"})

	// A pure ordering is untouched.
	ordered := Host{Name: "web", After: []string{"dns1"}}
	equalStrings(t, "predecessors", ordered.predecessors(), []string{"dns1"})
}

func TestValidateRelations(t *testing.T) {
	tests := []struct {
		name  string
		hosts Hosts
		want  string
	}{
		{
			name: "unknown hosting parent",
			hosts: Hosts{
				"vm-a": {Name: "vm-a", RunsOn: "hv9"},
			},
			want: `runs on "hv9", which is not a known host`,
		},
		{
			name: "self hosting",
			hosts: Hosts{
				"vm-a": {Name: "vm-a", RunsOn: "vm-a"},
			},
			want: "cannot run on itself",
		},
		{
			name: "hosting cycle",
			hosts: Hosts{
				"a": {Name: "a", RunsOn: "b"},
				"b": {Name: "b", RunsOn: "a"},
			},
			want: "hosting cycle",
		},
		{
			name: "unknown exclusion",
			hosts: Hosts{
				"dns1": {Name: "dns1", NotWith: []string{"dns9"}},
			},
			want: `must not reboot with "dns9", which is not a known host`,
		},
		{
			name: "self exclusion",
			hosts: Hosts{
				"dns1": {Name: "dns1", NotWith: []string{"dns1"}},
			},
			want: "cannot exclude itself",
		},
		{
			// A guest is power-cycled by its hypervisor whether or not anyone
			// asked, so this describes a fleet that cannot exist rather than a
			// constraint the run could honour.
			name: "exclusion between a guest and its host",
			hosts: Hosts{
				"hv1":  {Name: "hv1"},
				"vm-a": {Name: "vm-a", RunsOn: "hv1", NotWith: []string{"hv1"}},
			},
			want: "one runs on the other",
		},
		{
			// The same contradiction two levels down.
			name: "exclusion across a hosting chain",
			hosts: Hosts{
				"hv1":   {Name: "hv1"},
				"vm-a":  {Name: "vm-a", RunsOn: "hv1"},
				"ctr-1": {Name: "ctr-1", RunsOn: "vm-a", NotWith: []string{"hv1"}},
			},
			want: "one runs on the other",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.hosts.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestValidateAcceptsSiblingsThatExcludeEachOther(t *testing.T) {
	// Two guests of one hypervisor may exclude each other, unlike a guest and
	// the hypervisor itself. The reboots this tool issues can always be
	// separated — reboot one guest, wait, reboot the other — which is exactly
	// what an HA pair sharing a hypervisor wants. Only rebooting the
	// hypervisor takes both at once, and that is a property of the fleet
	// rather than an inconsistency in what was declared.
	hosts := hosting()
	hosts["vm-b"] = Host{Name: "vm-b", RunsOn: "hv1", NotWith: []string{"vm-a"}}
	if err := hosts.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
