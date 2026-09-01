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
// address, SSH user, and relationships to the rest of the fleet, so callers can
// build the set from command line arguments, a piped stream, or any other
// source. Package ansibleinv converts an Ansible inventory into these specs for
// callers that keep their topology there.
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
// never rebooted, but they can be depended on, and are waited on when a host
// they sit behind goes down.
//
// Four fields say how a host relates to the rest of the fleet, and they are
// deliberately not interchangeable, because the thing people call a dependency
// is at least four different claims:
//
//   - After is an ordering and says nothing about cause.
//   - RunsOn is the one causal claim — rebooting the parent restarts this host
//     — which is what lets a carried reboot be credited rather than repeated.
//   - NotWith is neither an ordering nor a cause: it forbids simultaneity,
//     in either direction.
//   - Ready says what "back" means for this host, which is what everything
//     ordered after it is actually waiting for.
//
// Collapsing them into one edge, as this package once did, means every consumer
// has to assume the weakest reading of all of them.
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
	// is rebooted. It is an ordering and nothing more: it never claims that the
	// named host's reboot affects this one, which is why nothing is concluded
	// from an After dependent that stays up. References to hosts outside the
	// target set are ignored when building tiers, so a partial run stays
	// possible.
	After []string
	// RunsOn names the host this one is hosted by: a hypervisor to its guest, a
	// guest to its container. It carries the ordering of After and adds the
	// causal claim After refuses to make — rebooting the parent power-cycles
	// this host. That claim is what earns the run its three extra behaviours:
	// the drop is expected rather than merely possible, the carried reboot is
	// credited instead of delivered twice, and a guest that stayed up while its
	// hypervisor restarted is a topology error worth saying out loud. A host
	// runs on at most one other, because it is only ever in one place at a time.
	RunsOn string
	// NotWith names hosts this one must never reboot alongside: the other half
	// of an HA pair, another member of a quorum. It is symmetric — declaring it
	// on either host binds both, because "never these two at once" is a fact
	// about the pair — and it orders nothing. Either may go first; they simply
	// may not go together.
	NotWith []string
	// Ready is the command that decides when this host counts as back, run over
	// SSH on the host itself, with only its exit status read. Empty means a
	// completed login is enough, which is all that can be assumed of a host the
	// orchestrator knows nothing else about. A host others depend on for a
	// service should say what serving means here: accepting logins and
	// answering queries are not the same moment, and everything ordered After
	// this host is waiting for the second one.
	Ready string
}

// Target returns the address to connect to, falling back to the host name.
func (h Host) Target() string {
	if h.Addr != "" {
		return h.Addr
	}
	return h.Name
}

// ReadyCommand returns the command that proves this host is back.
//
// The default is true: for a host that declared nothing, completing an SSH
// login is the whole of what can be checked, and it is already far more than
// answering ping.
func (h Host) ReadyCommand() string {
	if h.Ready != "" {
		return h.Ready
	}
	return "true"
}

// predecessors returns every host that must be rebooted and back before this
// one — its orderings, plus the host it runs on, which implies one.
//
// A host that names the same predecessor both ways is counted once, so writing
// the ordering out alongside the hosting it already implies is redundant rather
// than an error.
func (h Host) predecessors() []string {
	if h.RunsOn == "" || slices.Contains(h.After, h.RunsOn) {
		return h.After
	}
	return append(slices.Clone(h.After), h.RunsOn)
}

// Hosts maps host names to their definitions.
type Hosts map[string]Host

// Names returns every known host name, sorted.
func (hs Hosts) Names() []string {
	return slices.Sorted(maps.Keys(hs))
}

// Dependents returns the hosts that declare the named host in their After list.
//
// The edge declares an ordering and nothing more: do not reboot me until that
// host is back. Whether the named host's reboot actually takes these down is
// never claimed, and often untrue — a host ordered after a gateway usually has
// no intention of going anywhere when the gateway restarts. A tier therefore
// waits for its dependents, because any of them may go down, but concludes
// nothing from one that does not. Hosting is the edge that does make the claim;
// see Children.
func (hs Hosts) Dependents(name string) []string {
	var out []string
	for _, other := range hs.Names() {
		if slices.Contains(hs[other].After, name) {
			out = append(out, other)
		}
	}
	return out
}

// Children returns the hosts that declare the named host as the one they run on.
//
// Unlike Dependents, this edge is a statement about what happens: rebooting the
// named host restarts every host listed here. That is what makes a child's
// behaviour evidence — its drop is expected, its return is required, and the
// reboot it received is one it does not need again.
func (hs Hosts) Children(name string) []string {
	var out []string
	for _, other := range hs.Names() {
		if hs[other].RunsOn == name {
			out = append(out, other)
		}
	}
	return out
}

// Carried returns every host hosted by one of the named hosts, transitively and
// sorted: a hypervisor's guests, and the containers inside those guests.
//
// This is the set a tier takes down with it, and it has to be known in advance
// rather than discovered afterwards. A carried host must be watched, or the
// tier is declared finished while a guest is still booting; and its boot
// identity has to be read before its parent goes down, or the reboot it is
// about to receive for free cannot be credited to it once the evidence has
// already been overwritten.
func (hs Hosts) Carried(names []string) []string {
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}

	var out []string
	queue := slices.Clone(names)
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range hs.Children(parent) {
			// A hosting cycle is rejected by Validate, but the guard costs
			// nothing and keeps this total for callers holding a raw map.
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	slices.Sort(out)
	return out
}

// Excludes reports whether two hosts may not be rebooted in the same tier.
//
// The declaration is symmetric: written on either host it binds both, because
// an exclusion is a fact about the pair rather than about one of them. Making
// an operator write it twice would only create the chance to write it once.
func (hs Hosts) Excludes(a, b string) bool {
	return slices.Contains(hs[a].NotWith, b) || slices.Contains(hs[b].NotWith, a)
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
	// Each relationship replaces rather than extends the inherited one, so a
	// caller can narrow a host's constraints instead of only ever adding to
	// them. They replace independently: an overlay that states an ordering says
	// nothing about where the host runs, so it must not silently discard it.
	if len(overlay.After) > 0 {
		merged.After = overlay.After
	}
	if overlay.RunsOn != "" {
		merged.RunsOn = overlay.RunsOn
	}
	if len(overlay.NotWith) > 0 {
		merged.NotWith = overlay.NotWith
	}
	if overlay.Ready != "" {
		merged.Ready = overlay.Ready
	}
	return merged
}

// Validate checks that every relationship between hosts resolves and that no
// two of them contradict.
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
		if host.RunsOn != "" {
			if _, ok := hs[host.RunsOn]; !ok {
				return fmt.Errorf("host %q runs on %q, which is not a known host", name, host.RunsOn)
			}
			if host.RunsOn == name {
				return fmt.Errorf("host %q cannot run on itself", name)
			}
		}
		for _, peer := range host.NotWith {
			if _, ok := hs[peer]; !ok {
				return fmt.Errorf("host %q must not reboot with %q, which is not a known host", name, peer)
			}
			if peer == name {
				return fmt.Errorf("host %q cannot exclude itself from its own tier", name)
			}
		}
	}
	if err := hs.validateHosting(); err != nil {
		return err
	}
	return hs.validateExclusions()
}

// validateHosting rejects a hosting chain that closes on itself.
//
// Nothing runs on a machine that runs on it. Left in, the cycle would surface
// as a tier graph with no valid order — which is true, but reported as an
// ordering problem when the mistake is a claim about where things live.
func (hs Hosts) validateHosting() error {
	for _, name := range hs.Names() {
		seen := map[string]bool{name: true}
		for current := hs[name].RunsOn; current != ""; current = hs[current].RunsOn {
			if seen[current] {
				return fmt.Errorf("hosting cycle: host %q ultimately runs on itself", name)
			}
			seen[current] = true
		}
	}
	return nil
}

// validateExclusions rejects an exclusion that hosting already contradicts.
//
// A guest is power-cycled by its hypervisor whether or not anyone asked, so
// "never reboot these two together" is not a constraint the run could honour —
// it describes a fleet that cannot exist. Saying so while the plan is being
// built beats building tiers that quietly violate it.
func (hs Hosts) validateExclusions() error {
	for _, name := range hs.Names() {
		for _, peer := range hs[name].NotWith {
			if hs.hostedBy(name, peer) || hs.hostedBy(peer, name) {
				return fmt.Errorf(
					"host %q cannot exclude %q: one runs on the other, so they always reboot together",
					name, peer)
			}
		}
	}
	return nil
}

// hostedBy reports whether ancestor appears anywhere up the descendant's
// hosting chain. It stops on a repeat so a cycle cannot spin it, leaving
// validateHosting to report the cycle itself.
func (hs Hosts) hostedBy(descendant, ancestor string) bool {
	seen := map[string]bool{descendant: true}
	for current := hs[descendant].RunsOn; current != ""; current = hs[current].RunsOn {
		if current == ancestor {
			return true
		}
		if seen[current] {
			return false
		}
		seen[current] = true
	}
	return false
}
