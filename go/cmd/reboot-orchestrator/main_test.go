package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseArgsDefaults(t *testing.T) {
	opts, targets, err := parseArgs([]string{"web1", "web2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0] != "web1" || targets[1] != "web2" {
		t.Errorf("targets = %v, want web1 and web2", targets)
	}
	// Verification is on unless explicitly turned off: a run that cannot prove
	// what it did should be the deliberate choice, not the default.
	if !opts.config.VerifyBootState {
		t.Error("VerifyBootState = false, want it on by default")
	}
	if opts.config.SampleInterval != time.Second {
		t.Errorf("SampleInterval = %v, want 1s", opts.config.SampleInterval)
	}
	if opts.config.DropWait != 15*time.Second {
		t.Errorf("DropWait = %v, want 15s", opts.config.DropWait)
	}
	if opts.config.PingTimeout != time.Second {
		t.Errorf("PingTimeout = %v, want 1s", opts.config.PingTimeout)
	}
}

func TestParseArgsFlags(t *testing.T) {
	opts, targets, err := parseArgs([]string{
		"--user", "ops",
		"--ssh-arg", "-4",
		"--ssh-arg", "-o", "--ssh-arg", "LogLevel=ERROR",
		"--if-needed",
		"--skip-boot-verification",
		"--wait-drop", "30s",
		"--sample-interval", "250ms",
		"--probe-timeout", "5s",
		"--yes",
		"web1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.user != "ops" {
		t.Errorf("user = %q, want ops", opts.user)
	}
	if strings.Join(opts.sshArgs, " ") != "-4 -o LogLevel=ERROR" {
		t.Errorf("sshArgs = %v, want the three repeated values", opts.sshArgs)
	}
	if !opts.config.IfNeeded {
		t.Error("IfNeeded = false, want true")
	}
	if opts.config.VerifyBootState {
		t.Error("VerifyBootState = true, want it disabled by --skip-boot-verification")
	}
	if opts.config.SampleInterval != 250*time.Millisecond {
		t.Errorf("SampleInterval = %v, want 250ms", opts.config.SampleInterval)
	}
	if opts.config.DropWait != 30*time.Second {
		t.Errorf("DropWait = %v, want 30s", opts.config.DropWait)
	}
	if opts.config.ProbeTimeout != 5*time.Second {
		t.Errorf("ProbeTimeout = %v, want 5s", opts.config.ProbeTimeout)
	}
	if !opts.yes {
		t.Error("yes = false, want true")
	}
	if len(targets) != 1 || targets[0] != "web1" {
		t.Errorf("targets = %v, want web1", targets)
	}
}

func TestBuildPlanFromStdin(t *testing.T) {
	specs := "hv1,addr=10.0.0.5\nvm-a,addr=10.0.0.21,after=hv1\n"
	opts, targets, err := parseArgs([]string{"--hosts-from", "-", "vm-a"})
	if err != nil {
		t.Fatal(err)
	}

	plan, consumed, err := buildPlan(opts, targets, strings.NewReader(specs))
	if err != nil {
		t.Fatal(err)
	}
	if !consumed {
		t.Error("stdin was not marked consumed, so the prompt would read from a spent stream")
	}

	// Hosts piped in are context: only the host named on the command line is
	// rebooted, but the rest stay known so ordering and waiting still work.
	if len(plan.Targets) != 1 || plan.Targets[0] != "vm-a" {
		t.Errorf("targets = %v, want just vm-a", plan.Targets)
	}
	if got := plan.Hosts["vm-a"].Addr; got != "10.0.0.21" {
		t.Errorf("vm-a addr = %q, want the piped-in address", got)
	}
	if _, ok := plan.Hosts["hv1"]; !ok {
		t.Error("hv1 should stay known as context")
	}
}

func TestBuildPlanAll(t *testing.T) {
	opts, targets, err := parseArgs([]string{"--hosts-from", "-", "--all"})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := buildPlan(opts, targets, strings.NewReader("hv1\nvm-a,after=hv1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Targets) != 2 {
		t.Errorf("targets = %v, want both piped-in hosts", plan.Targets)
	}
}

func TestBuildPlanFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hosts.txt")
	contents := "# the lab\n\nhv1,addr=10.0.0.5\nvm-a,addr=10.0.0.21,after=hv1\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	opts, targets, err := parseArgs([]string{"--hosts-from", path, "--all"})
	if err != nil {
		t.Fatal(err)
	}
	plan, consumed, err := buildPlan(opts, targets, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	// Reading from a file leaves stdin free, so the prompt still works.
	if consumed {
		t.Error("stdin was marked consumed, want it left alone when reading a file")
	}
	if len(plan.Targets) != 2 {
		t.Errorf("targets = %v, want both hosts from the file", plan.Targets)
	}
}

func TestBuildPlanAppliesDefaults(t *testing.T) {
	opts, targets, err := parseArgs([]string{"--user", "ops", "web1", "web2,user=root"})
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := buildPlan(opts, targets, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Hosts["web1"].User; got != "ops" {
		t.Errorf("web1 user = %q, want the default ops", got)
	}
	if got := plan.Hosts["web2"].User; got != "root" {
		t.Errorf("web2 user = %q, want its own root", got)
	}
}

func TestBuildPlanErrors(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{
			name: "nothing given",
			args: nil,
			want: "no hosts given",
		},
		{
			// --all means "everything piped in", so naming hosts as well is a
			// contradiction worth reporting rather than silently resolving.
			name:  "all with named hosts",
			args:  []string{"--hosts-from", "-", "--all", "web1"},
			stdin: "web1\n",
			want:  "do not also name hosts",
		},
		{
			name: "unparseable spec",
			args: []string{"web1,nonsense"},
			want: "is not key=value",
		},
		{
			name: "missing spec file",
			args: []string{"--hosts-from", "/nonexistent/hosts.txt", "--all"},
			want: "cannot read host specs",
		},
		{
			name:  "ordering references an unknown host",
			args:  []string{"web1,after=ghost"},
			want:  `"ghost", which is not a known host`,
			stdin: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, targets, err := parseArgs(tt.args)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = buildPlan(opts, targets, strings.NewReader(tt.stdin))
			if err == nil {
				t.Fatal("buildPlan() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestIsPipe(t *testing.T) {
	// A strings.Reader is not a file at all, so it cannot be mistaken for a
	// pipe and made to swallow specs that were never sent.
	if isPipe(strings.NewReader("")) {
		t.Error("isPipe(strings.Reader) = true, want false")
	}

	path := filepath.Join(t.TempDir(), "specs")
	if err := os.WriteFile(path, []byte("web1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	// A redirected file is the shell's `< hosts.txt`, which should be read.
	if !isPipe(file) {
		t.Error("isPipe(regular file) = false, want true for a redirect")
	}
}
