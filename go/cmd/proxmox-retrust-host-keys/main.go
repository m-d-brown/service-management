// Command proxmox-retrust-host-keys safely re-trusts SSH host keys of
// Proxmox guests, verified out-of-band via the hypervisor.
//
// Usage:
//
//	proxmox-retrust-host-keys --node root@pve1.example.com \
//	    [--node ...] [--dry-run] [--known-hosts FILE] [GUEST[=NAME,...] ...]
//
// SSH records trust per name-as-typed, so each GUEST argument may carry
// comma-separated extra names (FQDN, IP) to maintain its known_hosts entries
// under. With no GUEST arguments every running guest is processed, trusted
// under the name the hypervisor reports (plus its first label, for
// FQDN-reported LXCs). --dry-run reports stale entries without touching
// known_hosts and exits 1 if any are stale.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"service-management/pkg/retrust"
)

type stringSlice []string

// String returns a comma-separated representation of the string slice.
func (s *stringSlice) String() string {
	return strings.Join(*s, ", ")
}

// Set appends a value to the string slice, implementing the flag.Value interface.
func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	log.SetFlags(0)

	var nodes stringSlice
	flag.Var(&nodes, "node", "Proxmox node to query, e.g. root@pve1.example.com (repeatable)")
	dryRun := flag.Bool("dry-run", false, "report stale entries without modifying known_hosts")
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("cannot determine home directory: %v", err)
	}
	knownHosts := flag.String("known-hosts", filepath.Join(home, ".ssh", "known_hosts"),
		"known_hosts file to update")
	flag.Usage = func() {
		_, _ = fmt.Fprintf(flag.CommandLine.Output(),
			"Usage: %s --node SSH_DEST [flags] [GUEST[=NAME,...] ...]\n\n"+
				"Safely re-trust SSH host keys of Proxmox guests, verified out-of-band\n"+
				"via the hypervisor. GUEST arguments limit processing to those guests;\n"+
				"each may carry comma-separated extra names (FQDN, IP) to maintain its\n"+
				"known_hosts entries under.\n\nFlags:\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if len(nodes) == 0 {
		log.Fatal("at least one --node is required")
	}

	specs := map[string][]string{}
	for _, arg := range flag.Args() {
		name, aliases := retrust.ParseGuestArg(arg)
		specs[name] = aliases
	}

	staleFound, err := retrust.Run(retrust.Config{
		Nodes:      nodes,
		KnownHosts: *knownHosts,
		DryRun:     *dryRun,
		GuestSpecs: specs,
	}, retrust.ExecRunner, os.Stdout)
	if err != nil {
		log.Fatal(err)
	}
	if staleFound {
		os.Exit(1)
	}
}
