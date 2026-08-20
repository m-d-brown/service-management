package reboot

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestPingArgs(t *testing.T) {
	tests := []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"whole seconds", 3 * time.Second, "-c 1 -W 3 10.0.0.4"},
		// -W takes whole seconds and rejects zero, so anything under a second
		// rounds up rather than silently becoming "no timeout".
		{"sub-second rounds up", 200 * time.Millisecond, "-c 1 -W 1 10.0.0.4"},
		{"zero becomes one", 0, "-c 1 -W 1 10.0.0.4"},
		{"fractional rounds up", 1500 * time.Millisecond, "-c 1 -W 2 10.0.0.4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := strings.Join(pingArgs("10.0.0.4", tt.timeout), " ")
			if got != tt.want {
				t.Errorf("pingArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPingHostUsesTargetAddress(t *testing.T) {
	runner := &fakeRunner{}
	host := Host{Name: "web1", Addr: "10.0.0.4"}

	if !PingHost(context.Background(), runner, host, time.Second) {
		t.Error("PingHost() = false, want true when ping succeeds")
	}
	if got := runner.lines(); len(got) != 1 || !strings.HasSuffix(got[0], "10.0.0.4") {
		t.Errorf("commands = %v, want a ping of the host address", got)
	}
}

func TestPingHostReportsFailure(t *testing.T) {
	runner := &fakeRunner{respond: func(call) (string, error) { return "", errSSHFailed }}
	if PingHost(context.Background(), runner, Host{Name: "web1"}, time.Second) {
		t.Error("PingHost() = true, want false when ping fails")
	}
}

func TestWaitForHostsPollsUntilAllAnswer(t *testing.T) {
	// web1 answers immediately; web2 stays down for two sweeps, so the wait
	// must keep polling rather than declaring the tier back after one pass.
	attempts := map[string]int{}
	runner := &fakeRunner{respond: func(c call) (string, error) {
		addr := c.args[len(c.args)-1]
		attempts[addr]++
		if addr == "10.0.0.5" && attempts[addr] < 3 {
			return "", errSSHFailed
		}
		return "", nil
	}}
	clock := newFakeClock()
	var out bytes.Buffer

	hosts := []Host{{Name: "web2", Addr: "10.0.0.5"}, {Name: "web1", Addr: "10.0.0.4"}}
	err := WaitForHosts(context.Background(), &out, runner, clock, hosts, 15*time.Second, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if attempts["10.0.0.4"] != 1 {
		t.Errorf("pinged web1 %d times, want 1 — it answered immediately", attempts["10.0.0.4"])
	}
	if attempts["10.0.0.5"] != 3 {
		t.Errorf("pinged web2 %d times, want 3", attempts["10.0.0.5"])
	}

	// The drop delay comes first, then one poll interval per unsuccessful
	// sweep. Without the delay a host still on its way down answers and the
	// run advances to a dependent tier too early.
	wantSlept := 15*time.Second + 2*pingPollInterval
	if clock.total() != wantSlept {
		t.Errorf("slept %v, want %v", clock.total(), wantSlept)
	}
	if slept := clock.slept[0]; slept != 15*time.Second {
		t.Errorf("first sleep = %v, want the drop delay of 15s", slept)
	}

	for _, want := range []string{"[✓] web1 is reachable.", "[✓] web2 is reachable."} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to contain %q", out.String(), want)
		}
	}
	// The polling command is announced once per host, not once per sweep.
	if got := strings.Count(out.String(), "$ ping"); got != 2 {
		t.Errorf("echoed the ping command %d times, want once per host", got)
	}
}

func TestWaitForHostsEmpty(t *testing.T) {
	runner := &fakeRunner{}
	clock := newFakeClock()
	var out bytes.Buffer

	if err := WaitForHosts(context.Background(), &out, runner, clock, nil, time.Minute, time.Second); err != nil {
		t.Fatal(err)
	}
	// Nothing went down, so nothing is waited for — including the drop delay.
	if clock.total() != 0 {
		t.Errorf("slept %v with no hosts, want none", clock.total())
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands with no hosts, want none", len(runner.calls))
	}
}

func TestWaitForHostsStopsOnCancel(t *testing.T) {
	// A host that never returns must not trap the operator: cancelling has to
	// break the poll loop.
	ctx, cancel := context.WithCancel(context.Background())
	runner := &fakeRunner{respond: func(call) (string, error) {
		cancel()
		return "", errSSHFailed
	}}
	var out bytes.Buffer

	err := WaitForHosts(ctx, &out, runner, newFakeClock(),
		[]Host{{Name: "web1", Addr: "10.0.0.4"}}, time.Second, time.Second)
	if err == nil {
		t.Fatal("WaitForHosts() = nil, want the cancellation reported")
	}
}
