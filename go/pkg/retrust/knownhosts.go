package retrust

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// KnownHostsEntries returns the known_hosts lines currently matching a name
// (which may be hashed). A missing entry is simply an empty result.
func KnownHostsEntries(knownHosts, name string) string {
	// ssh-keygen -F exits 1 when the host simply isn't present.
	output, _ := exec.Command("ssh-keygen", "-F", name, "-f", knownHosts).Output()
	return string(output)
}

// Retrust replaces a guest's known_hosts entries (all its names) with the
// verified keys, on a single combined line per key.
func Retrust(knownHosts string, names, keys []string) error {
	for _, name := range names {
		// -R keeps a known_hosts.old backup; it exits nonzero if nothing
		// matched, which is fine.
		_ = exec.Command("ssh-keygen", "-R", name, "-f", knownHosts).Run()
	}
	file, err := os.OpenFile(knownHosts, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open %s: %w", knownHosts, err)
	}
	joined := strings.Join(names, ",")
	for _, key := range keys {
		if _, err := fmt.Fprintf(file, "%s %s\n", joined, key); err != nil {
			_ = file.Close()
			return fmt.Errorf("append to %s: %w", knownHosts, err)
		}
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close %s: %w", knownHosts, err)
	}
	return nil
}
