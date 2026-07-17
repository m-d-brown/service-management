package retrust

import (
	"fmt"
	"io"
	"strings"
)

// Config drives one Run over a set of Proxmox nodes.
type Config struct {
	// Nodes are SSH destinations of Proxmox nodes, e.g. root@pve1.example.com.
	Nodes []string
	// KnownHosts is the known_hosts file to maintain.
	KnownHosts string
	// DryRun reports stale entries without modifying known_hosts.
	DryRun bool
	// GuestSpecs limits processing to these guests (matched by reported or
	// short name) and supplies extra names (FQDN, IP) to trust each guest's
	// keys under. An empty map processes every guest.
	GuestSpecs map[string][]string
}

// Run discovers running guests on each node, verifies their host keys via the
// hypervisor, and refreshes stale known_hosts entries. It reports progress to
// out and returns whether a dry run found stale entries.
func Run(cfg Config, run Runner, out io.Writer) (staleFound bool, err error) {
	for _, node := range cfg.Nodes {
		guests, err := RunningGuests(run, node)
		if err != nil {
			report(out, "WARN:      cannot list guests on %s: %v\n", node, err)
			continue
		}
		for _, guest := range guests {
			label := ShortName(guest.Name)
			aliases, matched := cfg.GuestSpecs[guest.Name]
			if !matched {
				aliases, matched = cfg.GuestSpecs[label]
			}
			if len(cfg.GuestSpecs) > 0 && !matched {
				continue
			}
			keys := GuestHostKeys(run, node, guest)
			if len(keys) == 0 {
				report(out, "WARN:      %s (vmid %s on %s): can't read keys\n", label, guest.VMID, node)
				continue
			}

			names := TrustNames(guest.Name, aliases)
			switch {
			case allTrusted(cfg.KnownHosts, names, keys):
				report(out, "ok:        %s already trusted\n", label)
			case cfg.DryRun:
				report(out, "STALE:     %s would install %d keys under %d names\n", label, len(keys), len(names))
				staleFound = true
			default:
				if err := Retrust(cfg.KnownHosts, names, keys); err != nil {
					return false, err
				}
				report(out, "retrusted: %s %d verified keys installed under %d names\n", label, len(keys), len(names))
			}
		}
	}
	return staleFound, nil
}

// report writes a progress line; progress output is best-effort, so write
// errors are deliberately ignored.
func report(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// allTrusted reports whether every verified key is already present under
// every name.
func allTrusted(knownHosts string, names, keys []string) bool {
	for _, name := range names {
		entries := KnownHostsEntries(knownHosts, name)
		for _, key := range keys {
			if !strings.Contains(entries, key) {
				return false
			}
		}
	}
	return true
}
