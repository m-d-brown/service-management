package reboot

import (
	"bytes"
	"strings"
	"testing"
)

func TestSSHDestination(t *testing.T) {
	tests := []struct {
		name string
		host Host
		want string
	}{
		{"name only", Host{Name: "web1"}, "web1"},
		{"address overrides name", Host{Name: "web1", Addr: "10.0.0.4"}, "10.0.0.4"},
		{"user and address", Host{Name: "web1", Addr: "10.0.0.4", User: "ops"}, "ops@10.0.0.4"},
		{"user and name", Host{Name: "web1", User: "ops"}, "ops@web1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshDestination(tt.host); got != tt.want {
				t.Errorf("sshDestination() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSSHCommandLayout(t *testing.T) {
	host := Host{Name: "web1", Addr: "10.0.0.4", User: "ops", SSHArgs: []string{"-4", "-C"}}
	args := sshCommand(host, "uptime")

	// The remote command must be last and the destination immediately before
	// it, or ssh reads one of them as an option.
	if got := args[len(args)-1]; got != "uptime" {
		t.Errorf("last argument = %q, want the remote command", got)
	}
	if got := args[len(args)-2]; got != "ops@10.0.0.4" {
		t.Errorf("second to last argument = %q, want the destination", got)
	}

	line := FormatCommand("ssh", args...)
	for _, want := range []string{"BatchMode=yes", "ConnectTimeout=5", "StrictHostKeyChecking=accept-new", "-4 -C"} {
		if !strings.Contains(line, want) {
			t.Errorf("command %q is missing %q", line, want)
		}
	}
}

func TestRebootHosts(t *testing.T) {
	runner := &fakeRunner{}
	var out bytes.Buffer

	hosts := []Host{{Name: "web1", Addr: "10.0.0.4"}, {Name: "web2", Addr: "10.0.0.5"}}
	RebootHosts(&out, runner, hosts)

	if len(runner.calls) != 2 {
		t.Fatalf("issued %d commands, want 2", len(runner.calls))
	}
	for _, c := range runner.calls {
		// Rebooting severs the connection, so waiting would only ever be
		// waiting for a failure.
		if !c.started {
			t.Errorf("command %q was awaited, want it dispatched", c.line())
		}
		if c.remote() != "sudo reboot || reboot" {
			t.Errorf("remote command = %q, want the reboot fallback pair", c.remote())
		}
	}
	if !strings.Contains(out.String(), "web1, web2") {
		t.Errorf("output = %q, want both hosts announced", out.String())
	}
	// Each command is echoed as a copy-pasteable line before it runs.
	if got := strings.Count(out.String(), "$ ssh "); got != 2 {
		t.Errorf("echoed %d commands, want 2", got)
	}
}

func TestRebootHostsEmpty(t *testing.T) {
	runner := &fakeRunner{}
	var out bytes.Buffer
	RebootHosts(&out, runner, nil)

	if len(runner.calls) != 0 {
		t.Errorf("issued %d commands for an empty tier, want none", len(runner.calls))
	}
	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing for an empty tier", out.String())
	}
}

func TestRebootHostsReportsDispatchFailure(t *testing.T) {
	runner := &fakeRunner{startErr: errSSHFailed}
	var out bytes.Buffer

	RebootHosts(&out, runner, []Host{{Name: "web1"}})

	if !strings.Contains(out.String(), "WARNING") || !strings.Contains(out.String(), "web1") {
		t.Errorf("output = %q, want a warning naming the host", out.String())
	}
}
