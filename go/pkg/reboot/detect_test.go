package reboot

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestProbeHost(t *testing.T) {
	tests := []struct {
		name       string
		stdout     string
		err        error
		wantNeed   RebootNeed
		wantReason string
	}{
		{
			name:       "debian flag with packages",
			stdout:     "NEEDED packages awaiting restart: libc6 linux-image-amd64\n",
			wantNeed:   NeedYes,
			wantReason: "packages awaiting restart: libc6 linux-image-amd64",
		},
		{
			name:       "freebsd kernel mismatch",
			stdout:     "NEEDED kernel 14.1-RELEASE running, 14.2-RELEASE installed\n",
			wantNeed:   NeedYes,
			wantReason: "kernel 14.1-RELEASE running, 14.2-RELEASE installed",
		},
		{
			name:       "up to date",
			stdout:     "OK no pending reboot flag\n",
			wantNeed:   NeedNo,
			wantReason: "no pending reboot flag",
		},
		{
			// An unreachable host is deliberately not "up to date": treating it
			// as current would quietly skip a host nobody checked.
			name:       "unreachable",
			err:        errSSHFailed,
			wantNeed:   NeedUnknown,
			wantReason: "probe failed",
		},
		{
			name:       "unrecognised output",
			stdout:     "bash: /bin/sh: Permission denied\n",
			wantNeed:   NeedUnknown,
			wantReason: "unexpected probe output",
		},
		{
			name:       "empty output",
			stdout:     "",
			wantNeed:   NeedUnknown,
			wantReason: "unexpected probe output",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &fakeRunner{respond: func(call) (string, error) {
				return tt.stdout, tt.err
			}}
			status := ProbeHost(context.Background(), runner, Host{Name: "web1"})

			if status.Host != "web1" {
				t.Errorf("Host = %q, want web1", status.Host)
			}
			if status.Need != tt.wantNeed {
				t.Errorf("Need = %v, want %v (reason: %s)", status.Need, tt.wantNeed, status.Reason)
			}
			if !strings.Contains(status.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to contain %q", status.Reason, tt.wantReason)
			}
		})
	}
}

func TestProbeHostPipesScriptToStdin(t *testing.T) {
	runner := &fakeRunner{respond: func(call) (string, error) { return "OK current\n", nil }}
	ProbeHost(context.Background(), runner, Host{Name: "web1"})

	if len(runner.calls) != 1 {
		t.Fatalf("ran %d commands, want 1", len(runner.calls))
	}
	c := runner.calls[0]

	// Piping the script to /bin/sh rather than passing it as an argument keeps
	// it working where the login shell is csh, as it commonly is on FreeBSD,
	// and takes shell quoting out of the picture entirely.
	if c.remote() != "/bin/sh" {
		t.Errorf("remote command = %q, want /bin/sh", c.remote())
	}
	if !strings.Contains(c.stdin, "freebsd-version -k") {
		t.Errorf("stdin = %q, want the probe script piped in", c.stdin)
	}
	if strings.Contains(c.line(), "freebsd-version") {
		t.Errorf("command line %q carries the script, want it only on stdin", c.line())
	}
}

func TestProbeHostsPreservesOrder(t *testing.T) {
	// Probes run concurrently, so results must be placed by index rather than
	// appended as they finish.
	runner := &fakeRunner{respond: func(c call) (string, error) {
		if strings.Contains(c.line(), "web2") {
			return "NEEDED packages awaiting restart: libc6\n", nil
		}
		return "OK no pending reboot flag\n", nil
	}}

	hosts := []Host{{Name: "web1"}, {Name: "web2"}, {Name: "web3"}}
	statuses := ProbeHosts(context.Background(), runner, hosts)

	if len(statuses) != 3 {
		t.Fatalf("got %d statuses, want 3", len(statuses))
	}
	for i, want := range []string{"web1", "web2", "web3"} {
		if statuses[i].Host != want {
			t.Errorf("statuses[%d].Host = %q, want %q", i, statuses[i].Host, want)
		}
	}
	if statuses[1].Need != NeedYes {
		t.Errorf("web2 Need = %v, want NeedYes", statuses[1].Need)
	}
	if statuses[0].Need != NeedNo {
		t.Errorf("web1 Need = %v, want NeedNo", statuses[0].Need)
	}
}

func TestProbeHostsEmpty(t *testing.T) {
	runner := &fakeRunner{}
	if got := ProbeHosts(context.Background(), runner, nil); len(got) != 0 {
		t.Errorf("ProbeHosts() = %v, want nothing", got)
	}
}

func TestPrintProbeReport(t *testing.T) {
	statuses := []RebootStatus{
		{Host: "web1", Need: NeedYes, Reason: "packages awaiting restart: libc6"},
		{Host: "web2", Need: NeedNo, Reason: "no pending reboot flag"},
		{Host: "web3", Need: NeedUnknown, Reason: "probe failed: connection refused"},
	}
	var out bytes.Buffer
	PrintProbeReport(&out, statuses)

	if !strings.Contains(out.String(), "3 hosts checked, 1 need a reboot") {
		t.Errorf("output = %q, want a count of checked and pending hosts", out.String())
	}
	// Every host gets a line, so a verdict is never inferred from silence.
	for _, want := range []string{"web1: packages", "web2: no pending", "web3: probe failed"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}
