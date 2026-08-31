package reboot

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFormatCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "plain arguments are left alone",
			args: []string{"ping", "-c", "1", "10.0.0.4"},
			want: "ping -c 1 10.0.0.4",
		},
		{
			// The remote command has to come back quoted, or the line an
			// operator copies would run its second half locally.
			name: "remote command is quoted as one word",
			args: []string{"ssh", "ops@10.0.0.4", "sudo reboot || reboot"},
			want: `ssh ops@10.0.0.4 'sudo reboot || reboot'`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatCommand(tt.args[0], tt.args[1:]...)
			if got != tt.want {
				t.Errorf("FormatCommand() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestHostLineLeadsWithTheHost(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "an echoed command is named by its host",
			got:  hostLine("web1", "$ %s", "ping -c 1 10.0.0.4"),
			want: "web1: $ ping -c 1 10.0.0.4\n",
		},
		{
			// The tag stays with the message it grades; the host still leads.
			name: "a tagged observation keeps its tag after the host",
			got:  hostLine("vm-a", "[down] stopped answering at %s", "09:14:07"),
			want: "vm-a: [down] stopped answering at 09:14:07\n",
		},
		{
			// The message is formatted before it is embedded, so a percent sign
			// that survives into the text is not read as a verb a second time.
			name: "a formatted message is not re-scanned for verbs",
			got:  hostLine("web1", "%s", "boot_id=%s unread"),
			want: "web1: boot_id=%s unread\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("hostLine() = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestFormatCommandIsCopyPasteable(t *testing.T) {
	// The echoed line is only worth printing if an operator can paste it back
	// into a shell and get the same argument vector, so check that against a
	// real shell rather than pinning one particular quoting style.
	args := []string{
		"sudo reboot || reboot",
		"echo 'hi there'",
		`back\slash`,
		"semi;colon $HOME",
		"",
	}
	for _, arg := range args {
		t.Run(arg, func(t *testing.T) {
			line := FormatCommand("printf", "%s", arg)
			stdout, err := ExecRunner{}.Run(context.Background(), "", "/bin/sh", "-c", line)
			if err != nil {
				t.Fatalf("running %s: %v", line, err)
			}
			if stdout != arg {
				t.Errorf("%s printed %q, want %q", line, stdout, arg)
			}
		})
	}
}

func TestExecRunnerRun(t *testing.T) {
	stdout, err := ExecRunner{}.Run(context.Background(), "", "echo", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", stdout)
	}
}

func TestExecRunnerRunPipesStdin(t *testing.T) {
	// The pending-reboot probe is delivered this way, so a shell reading its
	// script from standard input has to work.
	stdout, err := ExecRunner{}.Run(context.Background(), "echo piped\n", "/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != "piped" {
		t.Errorf("stdout = %q, want piped", stdout)
	}
}

func TestExecRunnerRunFoldsStderrIntoError(t *testing.T) {
	_, err := ExecRunner{}.Run(context.Background(), "", "/bin/sh", "-c", "echo nope >&2; exit 3")
	if err == nil {
		t.Fatal("Run() succeeded, want an error")
	}
	// Without stderr the caller is left with a bare exit status and no idea
	// which host refused what.
	if !strings.Contains(err.Error(), "nope") {
		t.Errorf("error = %q, want it to carry the command's stderr", err)
	}
}

func TestExecRunnerRunRespectsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ExecRunner{}.Run(ctx, "", "sleep", "10")
	if err == nil {
		t.Fatal("Run() succeeded, want a timeout")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run() took %v, want it cut short by the context", elapsed)
	}
}

func TestExecRunnerStartDoesNotWait(t *testing.T) {
	// Reboot commands are dispatched precisely so a severed connection cannot
	// stall the run, so Start must return before the command finishes.
	start := time.Now()
	if err := (ExecRunner{}).Start("sleep", "3"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Start() took %v, want it to return immediately", elapsed)
	}
}

func TestExecRunnerStartReportsMissingBinary(t *testing.T) {
	err := ExecRunner{}.Start("definitely-not-a-real-binary-42")
	if err == nil {
		t.Fatal("Start() succeeded, want an error for a missing binary")
	}
}
