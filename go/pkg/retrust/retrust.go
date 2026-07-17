// Package retrust safely re-trusts SSH host keys of Proxmox guests.
//
// When a guest's SSH host keys change (reprovisioning, an OS reinstall,
// cloud-init deciding it is on a new instance), SSH refuses to connect with
// "REMOTE HOST IDENTIFICATION HAS CHANGED". Blindly deleting the known_hosts
// entry and reconnecting would accept whatever key the network presents; this
// package instead reads each guest's public host keys out-of-band through the
// Proxmox hypervisor (`qm guest exec` for VMs, `pct exec` for LXCs) and
// replaces only stale known_hosts entries with those verified keys.
//
// Root of trust: the SSH host keys of the Proxmox nodes themselves, which are
// long-lived and must already be trusted. Connections deliberately go through
// the system ssh binary so the user's own known_hosts and ssh_config govern
// node verification.
package retrust

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// readPubKeys runs on the guest via qm guest exec / pct exec.
const readPubKeys = "sh -c 'cat /etc/ssh/ssh_host_*.pub'"

// Runner executes a command and returns its stdout. It exists so tests can
// substitute a fake for the ssh invocations.
type Runner func(name string, args ...string) (string, error)

// ExecRunner runs the command for real, folding stderr into the error.
func ExecRunner(name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(name, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// Guest is one running VM or LXC on a Proxmox node.
type Guest struct {
	// Kind is "vm" (qm) or "lxc" (pct).
	Kind string
	// VMID is the numeric Proxmox guest ID, as a string.
	VMID string
	// Name is the guest name as the hypervisor reports it. LXCs are often
	// reported by their FQDN.
	Name string
}

// ParseQMList yields the running VMs from `qm list` output.
func ParseQMList(output string) []Guest {
	var guests []Guest
	for _, line := range splitDataLines(output) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[2] == "running" {
			guests = append(guests, Guest{Kind: "vm", VMID: fields[0], Name: fields[1]})
		}
	}
	return guests
}

// ParsePCTList yields the running LXCs from `pct list` output.
func ParsePCTList(output string) []Guest {
	var guests []Guest
	for _, line := range splitDataLines(output) {
		// The lock column may be empty: the name is the last field.
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == "running" {
			guests = append(guests, Guest{Kind: "lxc", VMID: fields[0], Name: fields[len(fields)-1]})
		}
	}
	return guests
}

// splitDataLines drops the header line and blank lines from tabular output.
func splitDataLines(output string) []string {
	lines := strings.Split(output, "\n")
	if len(lines) == 0 {
		return nil
	}
	var data []string
	for _, line := range lines[1:] {
		if strings.TrimSpace(line) != "" {
			data = append(data, line)
		}
	}
	return data
}

// ParsePublicKeys returns "<keytype> <base64-key>" per line, dropping
// comments and blanks.
func ParsePublicKeys(output string) []string {
	var keys []string
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			keys = append(keys, fields[0]+" "+fields[1])
		}
	}
	return keys
}

// ShortName is the first label of a guest name (LXCs are often reported by
// FQDN).
func ShortName(reported string) string {
	name, _, _ := strings.Cut(reported, ".")
	return name
}

// ParseGuestArg splits "name=alias1,alias2" into the name and its aliases.
func ParseGuestArg(arg string) (string, []string) {
	name, aliasPart, _ := strings.Cut(arg, "=")
	var aliases []string
	for _, alias := range strings.Split(aliasPart, ",") {
		if alias != "" {
			aliases = append(aliases, alias)
		}
	}
	return name, aliases
}

// TrustNames is every name to maintain the guest's known_hosts entries under,
// deduped: the reported name, its first label, and any aliases. SSH records
// trust per name-as-typed, so entries must cover all of them.
func TrustNames(reported string, aliases []string) []string {
	var names []string
	seen := map[string]bool{}
	for _, name := range append([]string{reported, ShortName(reported)}, aliases...) {
		if name != "" && !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// RunningGuests lists the running VMs and LXCs on one Proxmox node.
func RunningGuests(run Runner, node string) ([]Guest, error) {
	qm, err := run("ssh", node, "qm list")
	if err != nil {
		return nil, err
	}
	pct, err := run("ssh", node, "pct list")
	if err != nil {
		return nil, err
	}
	return append(ParseQMList(qm), ParsePCTList(pct)...), nil
}

// GuestHostKeys reads a guest's public SSH host keys through the hypervisor.
// It returns nil if the guest is unreachable (e.g. its guest agent is not
// running).
func GuestHostKeys(run Runner, node string, guest Guest) []string {
	remote := fmt.Sprintf("pct exec %s -- %s", guest.VMID, readPubKeys)
	if guest.Kind == "vm" {
		remote = fmt.Sprintf("qm guest exec %s -- %s", guest.VMID, readPubKeys)
	}
	output, err := run("ssh", node, remote)
	if err != nil {
		return nil
	}
	if guest.Kind == "vm" {
		// `qm guest exec` wraps the command's output in JSON.
		var reply struct {
			Exitcode int    `json:"exitcode"`
			OutData  string `json:"out-data"`
		}
		if json.Unmarshal([]byte(output), &reply) != nil || reply.Exitcode != 0 {
			return nil
		}
		output = reply.OutData
	}
	return ParsePublicKeys(output)
}
