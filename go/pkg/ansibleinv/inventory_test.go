package ansibleinv

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"service-management/pkg/reboot"
)

func TestParseNestedGroups(t *testing.T) {
	inventory := `
all:
  children:
    hypervisors:
      hosts:
        hv1:
          ip_addr: 10.0.0.5
          ansible_user: root
    guests:
      hosts:
        vm-a:
          ip_addr: 10.0.0.21
          depends_on:
            - hv1
      children:
        services:
          hosts:
            web1:
              ansible_host: 10.0.0.30
              depends_on: [vm-a]
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}

	want := []reboot.Host{
		{Name: "hv1", Addr: "10.0.0.5", User: "root"},
		{Name: "vm-a", Addr: "10.0.0.21", After: []string{"hv1"}},
		{Name: "web1", Addr: "10.0.0.30", After: []string{"vm-a"}},
	}
	if !reflect.DeepEqual(hosts, want) {
		t.Errorf("Parse() = %+v, want %+v", hosts, want)
	}
}

func TestParseUnquotedScalars(t *testing.T) {
	// Inventories are hand-written, and a value YAML resolves to something other
	// than a string must not abort the parse of the whole document.
	inventory := `
all:
  hosts:
    hv1:
      ansible_host: 10001
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Addr != "10001" {
		t.Errorf("Parse() = %+v, want the address read as text", hosts)
	}
}

func TestParseAddressPrecedence(t *testing.T) {
	inventory := `
all:
  hosts:
    both:
      ip_addr: 10.0.0.1
      ansible_host: 10.0.0.2
    fallback:
      ansible_host: 10.0.0.3
    neither: {}
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, host := range hosts {
		got[host.Name] = host.Addr
	}
	want := map[string]string{"both": "10.0.0.1", "fallback": "10.0.0.3", "neither": ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("addresses = %v, want %v", got, want)
	}
}

func TestParseMergesVariablesAcrossGroups(t *testing.T) {
	// A host in two groups accumulates variables from both. Replacing the
	// whole definition would drop the address the first group established.
	inventory := `
all:
  children:
    network:
      hosts:
        web1:
          ip_addr: 10.0.0.30
    ordered:
      hosts:
        web1:
          depends_on: [hv1]
    hypervisors:
      hosts:
        hv1: {}
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}

	for _, host := range hosts {
		if host.Name != "web1" {
			continue
		}
		if host.Addr != "10.0.0.30" {
			t.Errorf("addr = %q, want it kept from the network group", host.Addr)
		}
		if !reflect.DeepEqual(host.After, []string{"hv1"}) {
			t.Errorf("after = %v, want it added by the ordered group", host.After)
		}
		return
	}
	t.Fatal("web1 is missing from the parsed inventory")
}

func TestParseSplitsSSHCommonArgs(t *testing.T) {
	inventory := `
all:
  hosts:
    web1:
      ansible_ssh_common_args: -o StrictHostKeyChecking=no -o "UserKnownHostsFile=/dev/null"
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}
	// The value is one string of shell words while ssh runs without a shell,
	// so it has to be split exactly once, here.
	want := []string{"-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile=/dev/null"}
	if !reflect.DeepEqual(hosts[0].SSHArgs, want) {
		t.Errorf("SSHArgs = %v, want %v", hosts[0].SSHArgs, want)
	}
}

func TestParseWithoutAllGroup(t *testing.T) {
	// A fragment or split-out group file has no "all", and should still work.
	hosts, err := Parse([]byte("hosts:\n  web1:\n    ip_addr: 10.0.0.30\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web1" || hosts[0].Addr != "10.0.0.30" {
		t.Errorf("Parse() = %+v, want the single host read", hosts)
	}
}

func TestParseEmpty(t *testing.T) {
	hosts, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 0 {
		t.Errorf("Parse(nil) = %v, want no hosts", hosts)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name      string
		inventory string
		want      string
	}{
		{
			name:      "invalid yaml",
			inventory: "all:\n  hosts:\n   - not a mapping\n",
			want:      "invalid inventory YAML",
		},
		{
			name:      "dependency on an unknown host",
			inventory: "all:\n  hosts:\n    web1:\n      depends_on: [ghost]\n",
			want:      `depends on "ghost"`,
		},
		{
			name:      "depends_on is not a list",
			inventory: "all:\n  hosts:\n    web1:\n      depends_on: hv1\n",
			want:      "invalid inventory YAML",
		},
		{
			name:      "unbalanced quote in ssh args",
			inventory: "all:\n  hosts:\n    web1:\n      ansible_ssh_common_args: \"-o 'unclosed\"\n",
			want:      "ansible_ssh_common_args",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.inventory))
			if err == nil {
				t.Fatal("Parse() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestParseRoundTripsThroughSpecs(t *testing.T) {
	// The two commands are joined by this format, so what Parse produces has to
	// survive being printed and read back by the orchestrator.
	inventory := `
all:
  hosts:
    hv1:
      ip_addr: 10.0.0.5
      ansible_user: root
    vm-a:
      ip_addr: 10.0.0.21
      ansible_ssh_common_args: -o StrictHostKeyChecking=no
      depends_on: [hv1]
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}

	var lines []string
	for _, host := range hosts {
		lines = append(lines, reboot.FormatSpec(host))
	}
	parsed, err := reboot.ReadSpecs(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatalf("reading back %q: %v", lines, err)
	}
	if !reflect.DeepEqual(parsed, hosts) {
		t.Errorf("round trip gave %+v, want %+v", parsed, hosts)
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.yml")
	if err := os.WriteFile(path, []byte("all:\n  hosts:\n    web1:\n      ip_addr: 10.0.0.30\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	hosts, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Name != "web1" {
		t.Errorf("Load() = %+v, want the single host", hosts)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yml"))
	if err == nil {
		t.Fatal("Load() succeeded, want an error for a missing file")
	}
	if !strings.Contains(err.Error(), "cannot read inventory") {
		t.Errorf("error = %q, want it to explain the file could not be read", err)
	}
}
