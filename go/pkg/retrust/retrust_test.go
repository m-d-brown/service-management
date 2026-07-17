package retrust

// Fixture hosts are fictional and use RFC 5737 documentation addresses
// (192.0.2.0/24).

import (
	"encoding/json"
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

const qmList = `      VMID NAME                 STATUS     MEM(MB)    BOOTDISK(GB) PID
       201 citrus               running    4096             150.00 1000
       900 dormant-vm           stopped    512                8.00 0
`

// pct reports the LXC by its hostname (FQDN) and has an empty Lock column.
const pctList = `VMID       Status     Lock         Name
202        running                 beacon.lab.example
901        stopped                 dormant-lxc
`

func TestParseQMListRunningOnly(t *testing.T) {
	got := ParseQMList(qmList)
	want := []Guest{{Kind: "vm", VMID: "201", Name: "citrus"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParseQMList = %v, want %v", got, want)
	}
}

func TestParsePCTListRunningOnlyWithEmptyLockColumn(t *testing.T) {
	got := ParsePCTList(pctList)
	want := []Guest{{Kind: "lxc", VMID: "202", Name: "beacon.lab.example"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePCTList = %v, want %v", got, want)
	}
}

func TestParsePCTListWithLockColumnStillTakesLastField(t *testing.T) {
	output := "VMID       Status     Lock         Name\n" +
		"203        running    backup       lockedguest\n"
	got := ParsePCTList(output)
	want := []Guest{{Kind: "lxc", VMID: "203", Name: "lockedguest"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ParsePCTList = %v, want %v", got, want)
	}
}

func TestParseEmptyListings(t *testing.T) {
	if got := ParseQMList("HEADER\n"); got != nil {
		t.Errorf("ParseQMList on empty = %v, want nil", got)
	}
	if got := ParsePCTList("HEADER\n"); got != nil {
		t.Errorf("ParsePCTList on empty = %v, want nil", got)
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
		case "qm list":
			return qmList, nil
		case "pct list":
			return pctList, nil
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

func TestGuestHostKeysVMUnwrapsAgentJSON(t *testing.T) {
	reply, _ := json.Marshal(map[string]any{
		"exitcode": 0,
		"out-data": fmt.Sprintf("%s root@citrus\n%s root@citrus\n", ed25519Key, rsaKey),
	})
	runner := func(name string, args ...string) (string, error) { return string(reply), nil }
	got := GuestHostKeys(runner, "root@node", Guest{Kind: "vm", VMID: "201"})
	want := []string{ed25519Key, rsaKey}
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

func TestGuestHostKeysAgentFailureReturnsNil(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		return `{"exitcode": 1, "out-data": ""}`, nil
	}
	if got := GuestHostKeys(runner, "root@node", Guest{Kind: "vm", VMID: "100"}); got != nil {
		t.Errorf("GuestHostKeys = %v, want nil", got)
	}
}

func TestGuestHostKeysSSHFailureReturnsNil(t *testing.T) {
	runner := func(name string, args ...string) (string, error) {
		return "", errors.New("connection refused")
	}
	if got := GuestHostKeys(runner, "root@node", Guest{Kind: "vm", VMID: "201"}); got != nil {
		t.Errorf("GuestHostKeys = %v, want nil", got)
	}
}

func TestGuestHostKeysCommandShape(t *testing.T) {
	for kind, wantPrefix := range map[string]string{
		"vm":  "qm guest exec 201 --",
		"lxc": "pct exec 201 --",
	} {
		var seen string
		runner := func(name string, args ...string) (string, error) {
			seen = args[1]
			if kind == "vm" {
				return `{"exitcode": 0, "out-data": ""}`, nil
			}
			return "", nil
		}
		GuestHostKeys(runner, "root@node", Guest{Kind: kind, VMID: "201"})
		if !strings.HasPrefix(seen, wantPrefix) {
			t.Errorf("%s remote command = %q, want prefix %q", kind, seen, wantPrefix)
		}
	}
}
