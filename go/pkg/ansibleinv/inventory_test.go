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

	// Each depends_on comes back as the ordering it implies, attached to the
	// host depended on: vm-a uses hv1, so hv1 is what reboots after vm-a.
	want := []reboot.Host{
		{Name: "hv1", Addr: "10.0.0.5", User: "root", After: []string{"vm-a"}},
		{Name: "vm-a", Addr: "10.0.0.21", After: []string{"web1"}},
		{Name: "web1", Addr: "10.0.0.30"},
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

	byName := map[string]reboot.Host{}
	for _, host := range hosts {
		byName[host.Name] = host
	}

	web1, ok := byName["web1"]
	if !ok {
		t.Fatal("web1 is missing from the parsed inventory")
	}
	if web1.Addr != "10.0.0.30" {
		t.Errorf("addr = %q, want it kept from the network group", web1.Addr)
	}
	// The dependency the second group added survives the merge, and lands as
	// an ordering on hv1 rather than on the host that declared it.
	if !reflect.DeepEqual(byName["hv1"].After, []string{"web1"}) {
		t.Errorf("hv1 after = %v, want [web1] from the ordered group", byName["hv1"].After)
	}
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

func TestParseReadsRelationships(t *testing.T) {
	inventory := `
all:
  hosts:
    hv1:
      ip_addr: 10.0.0.5
    vm-a:
      ip_addr: 10.0.0.21
      runs_on: hv1
    dns1:
      ip_addr: 10.0.0.41
      not_with: [dns2]
      ready: systemctl is-active named
    dns2:
      ip_addr: 10.0.0.42
    web1:
      ip_addr: 10.0.0.30
      depends_on: [vm-a, dns1]
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]reboot.Host{}
	for _, host := range hosts {
		byName[host.Name] = host
	}

	// Hosting and ordering stay distinct all the way through the converter:
	// rebooting hv1 restarts vm-a, while rebooting vm-a does nothing to web1.
	if got := byName["vm-a"].RunsOn; got != "hv1" {
		t.Errorf("vm-a RunsOn = %q, want hv1", got)
	}
	if got := byName["hv1"].After; len(got) != 0 {
		t.Errorf("hv1 After = %v, want none: runs_on is not depends_on", got)
	}
	// web1 declared both dependencies, and both come back as orderings on the
	// providers — web1 itself is left free to reboot first.
	if got := byName["web1"].After; len(got) != 0 {
		t.Errorf("web1 After = %v, want none: it depends, so it goes first", got)
	}
	if got := byName["vm-a"].After; !reflect.DeepEqual(got, []string{"web1"}) {
		t.Errorf("vm-a After = %v, want [web1]", got)
	}
	if got := byName["dns1"].After; !reflect.DeepEqual(got, []string{"web1"}) {
		t.Errorf("dns1 After = %v, want [web1]", got)
	}
	if got := byName["web1"].RunsOn; got != "" {
		t.Errorf("web1 RunsOn = %q, want empty", got)
	}
	if got := byName["dns1"].NotWith; !reflect.DeepEqual(got, []string{"dns2"}) {
		t.Errorf("dns1 NotWith = %v, want [dns2]", got)
	}
	if got, want := byName["dns1"].Ready, "systemctl is-active named"; got != want {
		t.Errorf("dns1 Ready = %q, want %q", got, want)
	}
}

func TestParseInvertsDependenciesOntoTheProvider(t *testing.T) {
	// Written on the consumer, where it is a true sentence about its own
	// subject; read as the ordering it implies, where the provider goes last.
	// The resolver never has to name a client, and no client has to state an
	// order — which is the whole point of deriving one from the other.
	inventory := `
all:
  hosts:
    dns1:
      ip_addr: 10.0.0.41
    web1:
      depends_on: [dns1]
    web2:
      depends_on: [dns1, dns1]
    app1:
      depends_on: [dns1, web1]
`
	hosts, err := Parse([]byte(inventory))
	if err != nil {
		t.Fatal(err)
	}

	after := map[string][]string{}
	for _, host := range hosts {
		after[host.Name] = host.After
	}

	// Sorted and deduplicated, so a repeated declaration costs nothing and the
	// provider's spec line is byte-identical between runs.
	if got, want := after["dns1"], []string{"app1", "web1", "web2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dns1 After = %v, want %v", got, want)
	}
	// A host can be both, and each role is read separately.
	if got, want := after["web1"], []string{"app1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("web1 After = %v, want %v", got, want)
	}
	// A host that only consumes is ordered by nothing, and reboots first.
	if got := after["app1"]; len(got) != 0 {
		t.Errorf("app1 After = %v, want none: it depends on others, so it goes first", got)
	}
	if got := after["web2"]; len(got) != 0 {
		t.Errorf("web2 After = %v, want none", got)
	}
}

func TestParseRejectsUnknownRelationshipTargets(t *testing.T) {
	// A typo is reported against the file it was typed into rather than
	// against the spec stream it became.
	tests := []struct {
		name      string
		inventory string
		want      string
	}{
		{
			name: "unknown hosting parent",
			inventory: `
all:
  hosts:
    vm-a:
      runs_on: hv9
`,
			want: `runs on "hv9"`,
		},
		{
			name: "unknown exclusion",
			inventory: `
all:
  hosts:
    dns1:
      not_with: [dns9]
`,
			want: `must not reboot with "dns9"`,
		},
		{
			name: "empty exclusion entry",
			inventory: `
all:
  hosts:
    dns1:
      not_with: [""]
`,
			want: "not_with contains an empty entry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.inventory))
			if err == nil {
				t.Fatalf("Parse() = nil error, want one mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
