package retrust

// Fixture hosts are fictional and use RFC 5737 documentation addresses
// (192.0.2.0/24).

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

const (
	ed25519Key = "ssh-ed25519 AAAAC3Nza_citrus_ed25519_key"
	rsaKey     = "ssh-rsa AAAAB3Nza_citrus_rsa_key"
	beaconKey  = "ssh-ed25519 AAAAC3Nza_beacon_ed25519_key"
)

const qemuListJSON = `[
  {"vmid": 201, "name": "citrus", "status": "running", "mem": 4096},
  {"vmid": 900, "name": "dormant-vm", "status": "stopped", "mem": 512}
]`

// LXCs are often reported by their hostname (FQDN).
const lxcListJSON = `[
  {"vmid": 202, "name": "beacon.lab.example", "status": "running"},
  {"vmid": 901, "name": "dormant-lxc", "status": "stopped"}
]`

// fileReadJSON wraps a file's content the way the agent file-read endpoint
// reports it.
func fileReadJSON(content string) string {
	return fmt.Sprintf(`{"content": %q, "truncated": 0}`, content)
}

func TestParseGuestListRunningOnly(t *testing.T) {
	got, err := ParseGuestList("vm", qemuListJSON)
	if err != nil {
		t.Fatal(err)
	}
	want := []Guest{{Kind: "vm", VMID: "201", Name: "citrus"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGuestList(vm) = %v, want %v", got, want)
	}

	got, err = ParseGuestList("lxc", lxcListJSON)
	if err != nil {
		t.Fatal(err)
	}
	want = []Guest{{Kind: "lxc", VMID: "202", Name: "beacon.lab.example"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseGuestList(lxc) = %v, want %v", got, want)
	}
}

func TestParseGuestListEmptyAndInvalid(t *testing.T) {
	if got, err := ParseGuestList("vm", "[]"); err != nil || got != nil {
		t.Errorf("ParseGuestList on empty = %v, %v; want nil, nil", got, err)
	}
	if _, err := ParseGuestList("vm", "not json"); err == nil {
		t.Error("ParseGuestList on invalid JSON should error")
	}
}

func TestParsePublicKeys(t *testing.T) {
	output := fmt.Sprintf("%s root@citrus\n\n%s root@citrus\n", ed25519Key, rsaKey)
	got := ParsePublicKeys(output)
	want := []string{ed25519Key, rsaKey}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePublicKeys = %v, want %v", got, want)
	}
	if got := ParsePublicKeys(""); got != nil {
		t.Errorf("ParsePublicKeys on empty = %v, want nil", got)
	}
}

func TestShortName(t *testing.T) {
	if got := ShortName("beacon.lab.example"); got != "beacon" {
		t.Errorf("ShortName = %q, want beacon", got)
	}
	if got := ShortName("citrus"); got != "citrus" {
		t.Errorf("ShortName = %q, want citrus", got)
	}
}

func TestParseGuestArg(t *testing.T) {
	name, aliases := ParseGuestArg("citrus")
	if name != "citrus" || aliases != nil {
		t.Errorf("ParseGuestArg = %q, %v; want citrus, nil", name, aliases)
	}
	name, aliases = ParseGuestArg("citrus=citrus.lab.example,192.0.2.20")
	if name != "citrus" || !reflect.DeepEqual(aliases, []string{"citrus.lab.example", "192.0.2.20"}) {
		t.Errorf("ParseGuestArg = %q, %v", name, aliases)
	}
}

func TestTrustNamesOrderAndDedup(t *testing.T) {
	got := TrustNames("citrus", []string{"citrus.lab.example", "192.0.2.20"})
	want := []string{"citrus", "citrus.lab.example", "192.0.2.20"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TrustNames = %v, want %v", got, want)
	}
	// FQDN-reported guests also get their short name; duplicates collapse.
	got = TrustNames("beacon.lab.example", []string{"beacon.lab.example"})
	want = []string{"beacon.lab.example", "beacon"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TrustNames = %v, want %v", got, want)
	}
}

func TestRunningGuestsCombinesVMsAndLXCs(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		switch args[1] {
		case "pvesh get /nodes/localhost/qemu --output-format json":
			return qemuListJSON, nil
		case "pvesh get /nodes/localhost/lxc --output-format json":
			return lxcListJSON, nil
		}
		return "", fmt.Errorf("unexpected command: %v", args)
	}
	got, err := RunningGuests(runner, "root@node.lab.example")
	if err != nil {
		t.Fatal(err)
	}
	want := []Guest{
		{Kind: "vm", VMID: "201", Name: "citrus"},
		{Kind: "lxc", VMID: "202", Name: "beacon.lab.example"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RunningGuests = %v, want %v", got, want)
	}
}

func TestGuestHostKeysVMReadsEachKeyFileSkippingAbsentTypes(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		remote := args[1]
		switch {
		case strings.Contains(remote, "ssh_host_rsa_key.pub"):
			return fileReadJSON(rsaKey + " root@citrus\n"), nil
		case strings.Contains(remote, "ssh_host_ecdsa_key.pub"):
			return "", errors.New("no such file")
		case strings.Contains(remote, "ssh_host_ed25519_key.pub"):
			return fileReadJSON(ed25519Key + " root@citrus\n"), nil
		}
		return "", fmt.Errorf("unexpected command: %v", args)
	}
	got := GuestHostKeys(runner, "root@node", Guest{Kind: "vm", VMID: "201"})
	want := []string{rsaKey, ed25519Key}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("GuestHostKeys = %v, want %v", got, want)
	}
}

func TestGuestHostKeysLXCUsesPlainOutput(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		return beaconKey + " root@beacon\n", nil
	}
	got := GuestHostKeys(runner, "root@node", Guest{Kind: "lxc", VMID: "202"})
	if !reflect.DeepEqual(got, []string{beaconKey}) {
		t.Errorf("GuestHostKeys = %v, want [%s]", got, beaconKey)
	}
}

func TestGuestHostKeysAgentDownReturnsNil(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		return "", errors.New("QEMU guest agent is not running")
	}
	if got := GuestHostKeys(runner, "root@node", Guest{Kind: "vm", VMID: "100"}); got != nil {
		t.Errorf("GuestHostKeys = %v, want nil", got)
	}
}

func TestGuestHostKeysLXCSSHFailureReturnsNil(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		return "", errors.New("connection refused")
	}
	if got := GuestHostKeys(runner, "root@node", Guest{Kind: "lxc", VMID: "202"}); got != nil {
		t.Errorf("GuestHostKeys = %v, want nil", got)
	}
}

func TestGuestHostKeysCommandShape(t *testing.T) {
	for kind, wantPrefix := range map[string]string{
		"vm":  "pvesh get /nodes/localhost/qemu/201/agent/file-read --file /etc/ssh/ssh_host_",
		"lxc": "pct exec 201 --",
	} {
		var seen string
		runner := func(name string, args ...string) (string, error) {
			seen = args[1]
			if kind == "vm" {
				return fileReadJSON(""), nil
			}
			return "", nil
		}
		GuestHostKeys(runner, "root@node", Guest{Kind: kind, VMID: "201"})
		if !strings.HasPrefix(seen, wantPrefix) {
			t.Errorf("%s remote command = %q, want prefix %q", kind, seen, wantPrefix)
		}
	}
}
