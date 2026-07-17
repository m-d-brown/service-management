package retrust

// known_hosts handling is exercised with the real ssh-keygen: entry matching,
// -R removal, and combined-name lines are exactly what the tool exists to get
// right, so mocking ssh-keygen here would test nothing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetrustReplacesStaleEntriesUnderEveryName(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	initial := "192.0.2.20 ssh-ed25519 OLDKEY\n" +
		"citrus.lab.example ssh-ed25519 OLDKEY\n" +
		"192.0.2.99 ssh-ed25519 OTHER_HOST_KEY\n"
	if err := os.WriteFile(knownHosts, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	names := []string{"citrus", "citrus.lab.example", "192.0.2.20"}
	if err := Retrust(knownHosts, names, []string{ed25519Key, rsaKey}); err != nil {
		t.Fatal(err)
	}

	content, err := os.ReadFile(knownHosts)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(content), "OLDKEY") {
		t.Error("stale OLDKEY entries were not removed")
	}
	// Unrelated hosts are untouched.
	if !strings.Contains(string(content), "192.0.2.99 ssh-ed25519 OTHER_HOST_KEY") {
		t.Error("unrelated host entry was removed")
	}
	// Every name now resolves to every verified key.
	for _, name := range names {
		entries := KnownHostsEntries(knownHosts, name)
		if !strings.Contains(entries, ed25519Key) || !strings.Contains(entries, rsaKey) {
			t.Errorf("entries for %s are missing verified keys: %q", name, entries)
		}
	}
}

func TestEntriesForUnknownNameAreEmpty(t *testing.T) {
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHosts, []byte("192.0.2.20 ssh-ed25519 SOMEKEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if entries := KnownHostsEntries(knownHosts, "elsewhere"); entries != "" {
		t.Errorf("entries for unknown name = %q, want empty", entries)
	}
}
