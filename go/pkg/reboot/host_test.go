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
