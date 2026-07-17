// Package retrust safely re-trusts SSH host keys of Proxmox guests.
//
// When a guest's SSH host keys change (reprovisioning, an OS reinstall,
// cloud-init deciding it is on a new instance), SSH refuses to connect with
// "REMOTE HOST IDENTIFICATION HAS CHANGED". Blindly deleting the known_hosts
// entry and reconnecting would accept whatever key the network presents; this
// package instead reads each guest's public host keys out-of-band through the
// Proxmox hypervisor — the API's guest-agent file-read (via pvesh) for VMs,
// `pct exec` for LXCs, which the API cannot reach into — and replaces only
// stale known_hosts entries with those verified keys.
//
// Root of trust: the SSH host keys of the Proxmox nodes themselves, which are
// long-lived and must already be trusted. Connections deliberately go through
// the system ssh binary so the user's own known_hosts and ssh_config govern
// node verification; pvesh then exposes the Proxmox API locally on the node,
// avoiding a second trust root (API tokens, TLS pinning) entirely.
package retrust

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// hostKeyPaths are the standard OpenSSH host key files read from VM guests.
// The agent's file-read endpoint takes exact paths (no globs), so the
// standard names are tried individually; missing types are skipped.
var hostKeyPaths = []string{
	"/etc/ssh/ssh_host_rsa_key.pub",
	"/etc/ssh/ssh_host_ecdsa_key.pub",
	"/etc/ssh/ssh_host_ed25519_key.pub",
}

// readPubKeys runs on LXC guests via pct exec; containers have no guest
// agent or API file-read, so the files are read with a shell glob instead.
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
	// Kind is "vm" (qemu) or "lxc".
	Kind string
	// VMID is the numeric Proxmox guest ID, as a string.
	VMID string
	// Name is the guest name as the hypervisor reports it. LXCs are often
	// reported by their FQDN.
	Name string
}

// ParseGuestList yields the running guests of one kind from a pvesh guest
// list JSON response (`pvesh get /nodes/localhost/{qemu,lxc}`).
func ParseGuestList(kind, jsonText string) ([]Guest, error) {
	var entries []struct {
		VMID   json.Number `json:"vmid"`
		Name   string      `json:"name"`
		Status string      `json:"status"`
	}
	if err := json.Unmarshal([]byte(jsonText), &entries); err != nil {
		return nil, fmt.Errorf("parse %s guest list: %w", kind, err)
	}
	var guests []Guest
	for _, entry := range entries {
		if entry.Status == "running" {
			guests = append(guests, Guest{Kind: kind, VMID: entry.VMID.String(), Name: entry.Name})
		}
	}
	return guests, nil
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

// RunningGuests lists the running VMs and LXCs on one Proxmox node, via the
// node-local API (pvesh) for typed JSON rather than human-oriented CLI output.
func RunningGuests(run Runner, node string) ([]Guest, error) {
	var all []Guest
	for _, list := range []struct{ kind, apiPath string }{
		{"vm", "qemu"},
		{"lxc", "lxc"},
	} {
		output, err := run("ssh", node,
			fmt.Sprintf("pvesh get /nodes/localhost/%s --output-format json", list.apiPath))
		if err != nil {
			return nil, err
		}
		guests, err := ParseGuestList(list.kind, output)
		if err != nil {
			return nil, err
		}
		all = append(all, guests...)
	}
	return all, nil
}

// GuestHostKeys reads a guest's public SSH host keys through the hypervisor.
// It returns nil if the guest is unreachable (e.g. a VM's guest agent is not
// running, in which case every file-read fails).
func GuestHostKeys(run Runner, node string, guest Guest) []string {
	if guest.Kind == "lxc" {
		output, err := run("ssh", node, fmt.Sprintf("pct exec %s -- %s", guest.VMID, readPubKeys))
		if err != nil {
			return nil
		}
		return ParsePublicKeys(output)
	}
	var keys []string
	for _, path := range hostKeyPaths {
		output, err := run("ssh", node, fmt.Sprintf(
			"pvesh get /nodes/localhost/qemu/%s/agent/file-read --file %s --output-format json",
			guest.VMID, path))
		if err != nil {
			continue // this key type is absent, or the agent is down
		}
		var fileRead struct {
			Content string `json:"content"`
		}
		if json.Unmarshal([]byte(output), &fileRead) != nil {
			continue
		}
		keys = append(keys, ParsePublicKeys(fileRead.Content)...)
	}
	return keys
}
