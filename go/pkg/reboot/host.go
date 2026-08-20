// Package reboot orchestrates dependency-aware reboots across network
// infrastructure.
//
// Rebooting a hypervisor before its guests have shut down, or a core gateway
// before the systems behind it are ready, causes state loss and network
// instability. This package groups hosts into tiers by their declared ordering,
// reboots each tier over plain SSH, waits for it to answer ping again, and
// proves the kernel actually restarted before moving on.
//
// Hosts are described entirely by the caller — there is no inventory format and
// no configuration management runner in the loop. A Host carries its own
// address, SSH user, and ordering constraints, so callers can build the set from
// command line arguments, a piped stream, or any other source. Package
// ansibleinv converts an Ansible inventory into these specs for callers that
// keep their topology there.
//
// This file is the data model; specs.go is the textual spec format that carries
// it in and out.
package reboot

import (
	"fmt"
	"maps"
	"slices"
)

// Host is one machine the orchestrator knows about.
//
// A host is either a target (named for reboot) or context: context hosts are
// never rebooted, but they can be depended on and are waited on when a host
// they sit behind goes down.
type Host struct {
	// Name identifies the host and is how other hosts refer to it.
	Name string
	// Addr is the address to ping and SSH to. Empty means use Name.
	Addr string
	// User is the SSH login user. Empty means let SSH decide.
	User string
	// SSHArgs are extra arguments passed to every ssh invocation for this host.
	SSHArgs []string
	// After names hosts that must be rebooted and back online before this host
	// is rebooted. References to hosts outside the target set are ignored when
	// building tiers, so a partial run stays possible.
	After []string
}

// Target returns the address to connect to, falling back to the host name.
func (h Host) Target() string {
	if h.Addr != "" {
		return h.Addr
	}
	return h.Name
}

// Hosts maps host names to their definitions.
type Hosts map[string]Host

// Names returns every known host name, sorted.
func (hs Hosts) Names() []string {
	return slices.Sorted(maps.Keys(hs))
}

// Dependents returns the hosts that declare the named host in their After list.
// These come back online with their parent, so a tier waits for them too.
func (hs Hosts) Dependents(name string) []string {
	var out []string
	for _, other := range hs.Names() {
		if slices.Contains(hs[other].After, name) {
			out = append(out, other)
		}
	}
	return out
}

// Merge overlays a spec onto an existing definition of the same host, field by
// field: a field the overlay leaves empty keeps the value it already had.
//
// This is what lets a caller name a host on the command line to target it —
// carrying no attributes at all — without discarding the definition that
// arrived on stdin, while still allowing any individual field to be overridden
// at the point of use.
func Merge(base, overlay Host) Host {
	merged := base
	merged.Name = overlay.Name
	if overlay.Addr != "" {
		merged.Addr = overlay.Addr
	}
	if overlay.User != "" {
		merged.User = overlay.User
	}
	if len(overlay.SSHArgs) > 0 {
		merged.SSHArgs = overlay.SSHArgs
	}
	// An explicit ordering replaces rather than extends the inherited one, so a
	// caller can narrow a host's constraints instead of only ever adding to them.
	if len(overlay.After) > 0 {
		merged.After = overlay.After
	}
	return merged
}

// Validate checks that every reference between hosts resolves.
func (hs Hosts) Validate() error {
	for _, name := range hs.Names() {
		host := hs[name]
		for _, after := range host.After {
			if _, ok := hs[after]; !ok {
				return fmt.Errorf("host %q must reboot after %q, which is not a known host", name, after)
			}
			if after == name {
				return fmt.Errorf("host %q cannot reboot after itself", name)
			}
		}
	}
	return nil
}
