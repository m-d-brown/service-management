package reboot

import (
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
