package reboot

import (
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
)

// Spec field keys, as they appear in a host spec.
const (
	keyAddr    = "addr"
	keyUser    = "user"
	keySSHArg  = "ssh-arg"
	keyAfter   = "after"
	keyRunsOn  = "runs-on"
	keyNotWith = "not-with"
	keyReady   = "ready"
)

// ParseSpec parses one host spec.
//
// A spec is a host name followed by comma-separated key=value fields:
//
//	web1
//	web1,addr=10.0.0.4,user=admin,after=dns1
//	vm-a,addr=10.0.0.21,runs-on=hv1
//	dns1,not-with=dns2,ready=dig +short @127.0.0.1 example.internal
//
// The connection fields are addr, user and ssh-arg. The rest say how the host
// relates to the fleet, and each says a different thing:
//
//	after=HOST     do not reboot this host until HOST is back. An ordering and
//	               nothing more; it makes no claim that HOST's reboot affects
//	               this host. Repeatable.
//	runs-on=HOST   this host is hosted by HOST, so rebooting HOST restarts it.
//	               Implies after=HOST, and adds the claim that after refuses to
//	               make. Given at most once: a host is in one place at a time.
//	not-with=HOST  never reboot this host in the same tier as HOST. Symmetric
//	               and unordered — either may go first. Repeatable.
//	ready=COMMAND  this host counts as back only once COMMAND succeeds on it
//	               over SSH. Defaults to true, meaning a completed login.
//
// The after, ssh-arg and not-with keys may repeat; runs-on and ready may not.
// A spec is a CSV record, so a value containing a comma is written as a quoted
// field, which is also how a readiness command carrying one is written:
//
//	web1,"ssh-arg=-oCiphers=aes128-ctr,aes256-ctr"
//	dns1,"ready=systemctl is-active named,nsd"
func ParseSpec(spec string) (Host, error) {
	fields, err := splitFields(spec)
	if err != nil {
		return Host{}, fmt.Errorf("host spec %q: %w", spec, err)
	}
	return hostFromFields(fields)
}

// hostFromFields builds a host from the already-split fields of one spec.
func hostFromFields(fields []string) (Host, error) {
	// Surrounding whitespace is never meaningful here and is easy to leave
	// behind in a hand-written host file, where it would otherwise turn into a
	// host name that matches nothing.
	fields = slices.Clone(fields)
	for i, field := range fields {
		fields[i] = strings.TrimSpace(field)
	}

	if len(fields) == 0 || fields[0] == "" {
		return Host{}, fmt.Errorf("host spec %q has no host name", strings.Join(fields, ","))
	}

	host := Host{Name: fields[0]}
	if strings.Contains(host.Name, "=") {
		return Host{}, fmt.Errorf("host spec %q starts with a key=value field; the host name must come first",
			strings.Join(fields, ","))
	}

	for _, field := range fields[1:] {
		if field == "" {
			continue
		}
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			return Host{}, fmt.Errorf("host %q: field %q is not key=value", host.Name, field)
		}
		if value == "" {
			return Host{}, fmt.Errorf("host %q: field %q has an empty value", host.Name, key)
		}
		switch key {
		case keyAddr:
			host.Addr = value
		case keyUser:
			host.User = value
		case keySSHArg:
			host.SSHArgs = append(host.SSHArgs, value)
		case keyAfter:
			host.After = append(host.After, value)
		case keyRunsOn:
			if host.RunsOn != "" {
				return Host{}, fmt.Errorf(
					"host %q: runs-on is given more than once; a host runs on at most one other",
					host.Name)
			}
			host.RunsOn = value
		case keyNotWith:
			host.NotWith = append(host.NotWith, value)
		case keyReady:
			if host.Ready != "" {
				return Host{}, fmt.Errorf(
					"host %q: ready is given more than once; write one command, joining tests with &&",
					host.Name)
			}
			host.Ready = value
		default:
			return Host{}, fmt.Errorf("host %q: unknown field %q", host.Name, key)
		}
	}
	return host, nil
}

// FormatSpec renders a host as a spec that ParseSpec reads back. Quoting is
// left to encoding/csv, so a value containing the delimiter round-trips without
// this package inventing an escape of its own.
func FormatSpec(h Host) string {
	fields := []string{h.Name}
	add := func(key, value string) {
		if value != "" {
			fields = append(fields, key+"="+value)
		}
	}
	add(keyAddr, h.Addr)
	add(keyUser, h.User)
	for _, arg := range h.SSHArgs {
		add(keySSHArg, arg)
	}
	add(keyRunsOn, h.RunsOn)
	for _, after := range h.After {
		add(keyAfter, after)
	}
	for _, peer := range h.NotWith {
		add(keyNotWith, peer)
	}
	add(keyReady, h.Ready)

	var buf strings.Builder
	writer := csv.NewWriter(&buf)
	// Writing to a strings.Builder cannot fail, so neither call can error.
	_ = writer.Write(fields)
	writer.Flush()
	return strings.TrimRight(buf.String(), "\r\n")
}

// splitFields splits a spec into its comma-separated fields.
func splitFields(spec string) ([]string, error) {
	reader := csv.NewReader(strings.NewReader(spec))
	// Specs are records of whatever length the host needs, and a bare quote
	// inside an SSH argument is data rather than a syntax error.
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	fields, err := reader.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil
	}
	return fields, err
}

// Defaults are the fleet-wide connection settings a caller supplies once,
// applied to any host that does not set them itself.
type Defaults struct {
	// User is the SSH login user for hosts with no user of their own.
	User string
	// SSHArgs are extra ssh arguments added to every host's own.
	SSHArgs []string
}

// apply overlays fleet-wide defaults onto a single host.
func (d Defaults) apply(h Host) Host {
	if h.User == "" {
		h.User = d.User
	}
	// Fleet-wide arguments come first so a host's own arguments are the later
	// ones on the command line; ssh honours the first occurrence of most
	// options, but this ordering keeps the per-host intent visible next to the
	// destination.
	if len(d.SSHArgs) > 0 {
		h.SSHArgs = append(append([]string{}, d.SSHArgs...), h.SSHArgs...)
	}
	return h
}

// ParseSpecs parses a list of host specs, preserving order.
func ParseSpecs(specs []string) ([]Host, error) {
	hosts := make([]Host, 0, len(specs))
	for _, spec := range specs {
		host, err := ParseSpec(spec)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// ReadSpecs reads one host spec per line. Blank lines and # comments are
// skipped so a generated stream can carry a header, and so an operator can keep
// a hand-written host file with notes in it.
func ReadSpecs(r io.Reader) ([]Host, error) {
	reader := csv.NewReader(r)
	reader.FieldsPerRecord = -1
	reader.LazyQuotes = true
	reader.Comment = '#'
	reader.TrimLeadingSpace = true

	records, err := reader.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("reading host specs: %w", err)
	}

	hosts := make([]Host, 0, len(records))
	for _, fields := range records {
		host, err := hostFromFields(fields)
		if err != nil {
			return nil, err
		}
		hosts = append(hosts, host)
	}
	return hosts, nil
}

// Plan is a validated set of hosts plus the subset to reboot.
type Plan struct {
	// Hosts is every host the orchestrator knows about, targets included.
	Hosts Hosts
	// Targets are the hosts to reboot, in the order they were named.
	Targets []string
}

// BuildPlan combines context hosts with target hosts into a validated plan.
//
// Context hosts — typically piped in from an inventory — describe the topology
// without being rebooted. Target hosts are the ones named for reboot; a target
// that repeats a context host's name overlays its fields onto that definition
// rather than replacing it, so naming a host to reboot it costs nothing more
// than its name. When targets is empty, every context host is targeted, which
// is how a caller reboots a whole piped-in inventory.
func BuildPlan(contextHosts, targets []Host, defaults Defaults, targetAll bool) (Plan, error) {
	hosts := Hosts{}
	var order []string
	for _, host := range contextHosts {
		if _, seen := hosts[host.Name]; !seen {
			order = append(order, host.Name)
		}
		hosts[host.Name] = Merge(hosts[host.Name], host)
	}

	var targetNames []string
	seen := map[string]bool{}
	for _, host := range targets {
		hosts[host.Name] = Merge(hosts[host.Name], host)
		if !seen[host.Name] {
			seen[host.Name] = true
			targetNames = append(targetNames, host.Name)
		}
	}

	if targetAll {
		for _, name := range order {
			if !seen[name] {
				seen[name] = true
				targetNames = append(targetNames, name)
			}
		}
	}

	for name, host := range hosts {
		hosts[name] = defaults.apply(host)
	}

	if len(hosts) == 0 {
		return Plan{}, fmt.Errorf("no hosts given")
	}
	if len(targetNames) == 0 {
		return Plan{}, fmt.Errorf("no hosts targeted for reboot")
	}
	if err := hosts.Validate(); err != nil {
		return Plan{}, err
	}
	// Surfaced here rather than at execution time: a cycle means no valid
	// ordering exists, and reporting that before the confirmation prompt beats
	// discovering it after the first tier has already rebooted.
	if _, err := BuildTiers(hosts, targetNames); err != nil {
		return Plan{}, err
	}
	return Plan{Hosts: hosts, Targets: targetNames}, nil
}
