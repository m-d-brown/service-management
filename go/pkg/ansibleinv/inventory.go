// Package ansibleinv converts an Ansible YAML inventory into reboot host specs.
//
// It exists so that reboot-orchestrator itself needs to know nothing about
// Ansible. Topology that already lives in an inventory is translated here and
// piped across; a fleet with no inventory at all is described directly on the
// orchestrator's command line. Neither tool has to grow the other's concerns,
// and the boundary between them is a stream of host specs anyone can read,
// diff, or write by hand.
package ansibleinv

import (
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/kballard/go-shellquote"
	"gopkg.in/yaml.v3"

	"service-management/pkg/reboot"
)

// group is one Ansible inventory group. Groups nest arbitrarily deep through
// children, and a host may appear in several of them.
type group struct {
	// Hosts are the hosts defined directly in this group.
	Hosts map[string]hostVars `yaml:"hosts"`
	// Children are the nested groups.
	Children map[string]group `yaml:"children"`
}

// hostVars are the inventory variables this tool reads from a host. Everything
// else an inventory carries is ignored rather than rejected, so an inventory
// serving other tools too still works untouched.
type hostVars struct {
	// IPAddr is the preferred connection address.
	IPAddr scalar `yaml:"ip_addr"`
	// AnsibleHost is the fallback connection address.
	AnsibleHost scalar `yaml:"ansible_host"`
	// AnsibleUser is the SSH login user.
	AnsibleUser scalar `yaml:"ansible_user"`
	// SSHCommonArgs is one string of extra ssh arguments.
	SSHCommonArgs scalar `yaml:"ansible_ssh_common_args"`
	// DependsOn names the hosts this one draws a service from — a resolver, a
	// gateway, a database. It is read in the direction it is written: the host
	// carrying the key is the consumer, and the hosts named are what it
	// consumes. The reboot ordering it implies is the reverse, and is derived
	// rather than written; see invertDependencies.
	DependsOn []string `yaml:"depends_on"`
	// RunsOn names the host this one is hosted by, whose reboot restarts it.
	RunsOn scalar `yaml:"runs_on"`
	// NotWith names hosts this one must never reboot alongside.
	NotWith []string `yaml:"not_with"`
	// Ready is the command that proves this host is serving again.
	Ready scalar `yaml:"ready"`
}

// scalar is a YAML scalar read as text whatever its type. An inventory writes
// a vmid as a bare number and an address sometimes unquoted, so decoding
// straight into a string would fail on documents that are perfectly valid.
type scalar string

// UnmarshalYAML reads any scalar node as its literal text.
func (s *scalar) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("line %d: expected a single value, found %s", node.Line, kindName(node.Kind))
	}
	*s = scalar(node.Value)
	return nil
}

// kindName names a YAML node kind for error messages.
func kindName(kind yaml.Kind) string {
	switch kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.MappingNode:
		return "a mapping"
	case yaml.AliasNode:
		return "an alias"
	case yaml.DocumentNode:
		return "a document"
	case yaml.ScalarNode:
		return "a value"
	default:
		return "an unsupported node"
	}
}

// Load reads an inventory file and returns its hosts, sorted by name.
func Load(path string) ([]reboot.Host, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the inventory path is the operator's own argument
	if err != nil {
		return nil, fmt.Errorf("cannot read inventory: %w", err)
	}
	return Parse(data)
}

// Parse converts inventory YAML into hosts, sorted by name.
func Parse(data []byte) ([]reboot.Host, error) {
	// Real inventories hang everything off the implicit "all" group. Decoding
	// into both shapes lets a fragment without it work too, which is what makes
	// a hand-written test inventory or a split-out group file usable directly.
	var root struct {
		All *group `yaml:"all"`
	}
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("invalid inventory YAML: %w", err)
	}

	top := root.All
	if top == nil {
		top = &group{}
		if err := yaml.Unmarshal(data, top); err != nil {
			return nil, fmt.Errorf("invalid inventory YAML: %w", err)
		}
	}

	appearances := map[string][]hostVars{}
	flatten(*top, appearances)

	hosts := make([]reboot.Host, 0, len(appearances))
	for _, name := range slices.Sorted(maps.Keys(appearances)) {
		var host reboot.Host
		for _, vars := range appearances[name] {
			parsed, err := hostFromVars(name, vars)
			if err != nil {
				return nil, err
			}
			// Variables accumulate across every group a host belongs to, later
			// groups overriding earlier ones field by field. Replacing the
			// whole definition instead would silently drop an address set in
			// one group when another group only adds an ordering constraint.
			host = reboot.Merge(host, parsed)
		}
		hosts = append(hosts, host)
	}
	if err := validateDependencies(hosts); err != nil {
		return nil, err
	}
	return invertDependencies(hosts), nil
}

// invertDependencies turns each declared dependency into the reboot ordering it
// implies: a host is rebooted before the hosts it depends on, so the provider
// goes last.
//
// The inventory and the spec stream deliberately describe the same fact from
// opposite ends, and this is the hinge between them. An operator writes what is
// true of a host — "I use the resolver" — on the host it is true of, so every
// line reads as a sentence about its own subject. The orchestrator wants the
// only thing it can act on, an order. Deriving one from the other here is what
// keeps either from having to be written backwards: expressed as an order, a
// resolver deliberately rebooted last has to name every client that queries it,
// and "the resolver depends on its clients" is a line nobody reads twice the
// same way.
//
// Last, rather than first, because a consumer rebooted after its provider comes
// up into the outage that provider's own restart just opened — booting without
// the DNS, gateway or storage it was waiting on. Rebooting it while the service
// is still there, and the provider once nothing is mid-boot behind it, puts the
// gap where nothing is starting up. A host that genuinely cannot boot without
// the service is a different claim, and says so with runs_on.
func invertDependencies(hosts []reboot.Host) []reboot.Host {
	position := make(map[string]int, len(hosts))
	for i, host := range hosts {
		position[host.Name] = i
	}

	// Detached first, so a dependency still waiting to be inverted is never
	// mistaken for an ordering already in place. Left in, an edge appended to a
	// provider would be read as that provider's own declaration once the loop
	// reached it, and inverted a second time.
	declared := make([][]string, len(hosts))
	for i := range hosts {
		declared[i], hosts[i].After = hosts[i].After, nil
	}

	for i, deps := range declared {
		for _, dep := range deps {
			// validateDependencies has already rejected a name no host answers
			// to, so the lookup cannot miss.
			provider := position[dep]
			hosts[provider].After = append(hosts[provider].After, hosts[i].Name)
		}
	}

	// Two consumers of one provider arrive in whatever order the hosts were
	// sorted into; sorting again keeps a provider's spec line byte-identical
	// between runs, which is what makes the stream diffable.
	for i := range hosts {
		slices.Sort(hosts[i].After)
		hosts[i].After = slices.Compact(hosts[i].After)
	}
	return hosts
}

// flatten records each appearance of a host in a group and everything nested
// under it. Children are visited in sorted order so a host defined in two
// sibling groups resolves the same way on every run.
func flatten(g group, into map[string][]hostVars) {
	for _, name := range slices.Sorted(maps.Keys(g.Hosts)) {
		into[name] = append(into[name], g.Hosts[name])
	}
	for _, name := range slices.Sorted(maps.Keys(g.Children)) {
		flatten(g.Children[name], into)
	}
}

// hostFromVars builds a host from one host's inventory variables.
func hostFromVars(name string, vars hostVars) (reboot.Host, error) {
	host := reboot.Host{
		Name:    name,
		Addr:    string(firstSet(vars.IPAddr, vars.AnsibleHost)),
		User:    string(vars.AnsibleUser),
		After:   vars.DependsOn,
		RunsOn:  string(vars.RunsOn),
		NotWith: vars.NotWith,
		Ready:   string(vars.Ready),
	}

	if args := string(vars.SSHCommonArgs); args != "" {
		// The value is one string of shell words while ssh is executed without
		// a shell, so it has to be split exactly once, here.
		words, err := shellquote.Split(args)
		if err != nil {
			return reboot.Host{}, fmt.Errorf("host %q: ansible_ssh_common_args: %w", name, err)
		}
		host.SSHArgs = words
	}

	for _, dep := range host.After {
		if dep == "" {
			return reboot.Host{}, fmt.Errorf("host %q: depends_on contains an empty entry", name)
		}
	}
	for _, peer := range host.NotWith {
		if peer == "" {
			return reboot.Host{}, fmt.Errorf("host %q: not_with contains an empty entry", name)
		}
	}
	return host, nil
}

// firstSet returns the first non-empty scalar, giving ip_addr precedence over
// ansible_host as the orchestrator did when it read inventories itself.
func firstSet(preferred, fallback scalar) scalar {
	if preferred != "" {
		return preferred
	}
	return fallback
}

// validateDependencies checks that every host a relationship names is one the
// inventory actually defines. Catching it here means the orchestrator is handed
// a topology that already holds together, and that a typo is reported against
// the file it was typed into rather than against the spec stream it became.
//
// Only the references are checked. Whether the relationships contradict each
// other — a hosting chain that closes on itself, an exclusion between a guest
// and the hypervisor it cannot help rebooting with — is settled once, in the
// orchestrator, against the full host set the run will actually act on. That
// set can be wider than any single inventory.
func validateDependencies(hosts []reboot.Host) error {
	known := make(map[string]bool, len(hosts))
	for _, host := range hosts {
		known[host.Name] = true
	}
	for _, host := range hosts {
		for _, dep := range host.After {
			if !known[dep] {
				return fmt.Errorf("host %q depends on %q, which does not exist in the inventory",
					host.Name, dep)
			}
		}
		if host.RunsOn != "" && !known[host.RunsOn] {
			return fmt.Errorf("host %q runs on %q, which does not exist in the inventory",
				host.Name, host.RunsOn)
		}
		for _, peer := range host.NotWith {
			if !known[peer] {
				return fmt.Errorf("host %q must not reboot with %q, which does not exist in the inventory",
					host.Name, peer)
			}
		}
	}
	return nil
}
