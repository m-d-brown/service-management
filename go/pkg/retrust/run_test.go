package retrust

// Run() end-to-end tests with a fake runner and a real temporary known_hosts
// file (real ssh-keygen).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testNode = "root@node.lab.example"

// fakeRunner dispatches on the command line Run would execute.
func fakeRunner(t *testing.T) Runner {
	return func(name string, args ...string) (string, error) {
		t.Helper()
		if name != "ssh" || args[0] != testNode {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		remote := args[1]
		switch {
		case remote == "qm list":
			return qmList, nil
		case remote == "pct list":
			return pctList, nil
		case strings.HasPrefix(remote, "qm guest exec 201"):
			reply, _ := json.Marshal(map[string]any{
				"exitcode": 0,
				"out-data": fmt.Sprintf("%s root@citrus\n%s root@citrus\n", ed25519Key, rsaKey),
			})
			return string(reply), nil
		case strings.HasPrefix(remote, "pct exec 202"):
			return beaconKey + " root@beacon\n", nil
		}
		t.Fatalf("unexpected remote command: %s", remote)
		return "", nil
	}
}

func newConfig(t *testing.T, dryRun bool, guestArgs ...string) Config {
	knownHosts := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHosts, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	specs := map[string][]string{}
	for _, arg := range guestArgs {
		name, aliases := ParseGuestArg(arg)
		specs[name] = aliases
	}
	return Config{
		Nodes:      []string{testNode},
		KnownHosts: knownHosts,
		DryRun:     dryRun,
		GuestSpecs: specs,
	}
}

func runToString(t *testing.T, cfg Config) (bool, string) {
	var out bytes.Buffer
	staleFound, err := Run(cfg, fakeRunner(t), &out)
	if err != nil {
		t.Fatal(err)
	}
	return staleFound, out.String()
}

func TestRunDryRunReportsStale(t *testing.T) {
	cfg := newConfig(t, true)
	staleFound, out := runToString(t, cfg)
	if !staleFound {
		t.Error("dry run should report stale entries")
	}
	for _, want := range []string{"STALE:     citrus", "STALE:     beacon"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if content, _ := os.ReadFile(cfg.KnownHosts); len(content) != 0 {
		t.Errorf("dry run modified known_hosts: %q", content)
	}
}

func TestRunRetrustThenRerunIsIdempotent(t *testing.T) {
	cfg := newConfig(t, false, "citrus=citrus.lab.example,192.0.2.20", "beacon")
	_, out := runToString(t, cfg)
	for _, want := range []string{
		"retrusted: citrus 2 verified keys installed under 3 names",
		"retrusted: beacon 1 verified keys installed under 2 names",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	content, _ := os.ReadFile(cfg.KnownHosts)
	wantLine := "citrus,citrus.lab.example,192.0.2.20 " + ed25519Key
	if !strings.Contains(string(content), wantLine) {
		t.Errorf("known_hosts missing %q:\n%s", wantLine, content)
	}

	staleFound, out := runToString(t, cfg)
	if staleFound {
		t.Error("rerun should not report stale entries")
	}
	for _, want := range []string{"ok:        citrus", "ok:        beacon"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRunGuestFilterLimitsScope(t *testing.T) {
	cfg := newConfig(t, false, "citrus")
	_, out := runToString(t, cfg)
	if !strings.Contains(out, "retrusted: citrus") {
		t.Errorf("output missing citrus retrust:\n%s", out)
	}
	if strings.Contains(out, "beacon") {
		t.Errorf("filtered guest appeared in output:\n%s", out)
	}
}

func TestRunFilterMatchesFQDNReportedGuestByShortName(t *testing.T) {
	cfg := newConfig(t, false, "beacon=192.0.2.30")
	_, out := runToString(t, cfg)
	if !strings.Contains(out, "retrusted: beacon 1 verified keys installed under 3 names") {
		t.Errorf("output missing beacon retrust:\n%s", out)
	}
	content, _ := os.ReadFile(cfg.KnownHosts)
	wantLine := "beacon.lab.example,beacon,192.0.2.30 " + beaconKey
	if !strings.Contains(string(content), wantLine) {
		t.Errorf("known_hosts missing %q:\n%s", wantLine, content)
	}
}
