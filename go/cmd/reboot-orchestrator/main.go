// Command reboot-orchestrator reboots hosts in dependency order, over plain
// SSH, and proves each one actually restarted.
//
// Usage:
//
//	reboot-orchestrator [flags] HOST_SPEC...
//	ansible-inventory-reboot-hosts -i inventory.yml | reboot-orchestrator [flags] HOST...
//
// Hosts are described by specs — a name plus optional comma-separated fields —
// given as arguments, piped in on stdin, or both. Specs on stdin describe the
// topology; the hosts named as arguments are the ones rebooted, so a piped-in
// inventory can be far larger than the set being touched. --all targets
// everything that arrived on stdin instead.
//
// There is no inventory format and no configuration management runner here: see
// ansible-inventory-reboot-hosts for translating an existing Ansible inventory.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"service-management/pkg/reboot"
)

// stringSlice collects a repeatable string flag.
type stringSlice []string

// String returns a comma-separated representation of the slice.
func (s *stringSlice) String() string { return strings.Join(*s, ", ") }

// Set appends a value, implementing the flag.Value interface.
func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// options are the parsed command line.
type options struct {
	user       string
	sshArgs    stringSlice
	hostsFrom  string
	all        bool
	yes        bool
	ifNeeded   bool
	skipVerify bool
	config     reboot.Config
}

func main() {
	log := func(format string, args ...any) {
		_, _ = fmt.Fprintf(os.Stderr, format+"\n", args...)
	}

	// Ctrl-C and SIGTERM cancel the run: without this, a wait for a host that
	// is never coming back could only be escaped by killing the process
	// mid-tier, with no summary of what had already been done.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := run(ctx, os.Args[1:], os.Stdin, os.Stdout)
	if err != nil {
		log("FATAL: %v", err)
	}
	os.Exit(code)
}

// run parses arguments, builds the plan, and orchestrates. It returns the
// process exit status so main stays free of policy.
func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) (int, error) {
	opts, targets, err := parseArgs(args)
	if errors.Is(err, flag.ErrHelp) {
		return 0, nil
	}
	if err != nil {
		return 1, err
	}

	plan, stdinConsumed, err := buildPlan(opts, targets, stdin)
	if err != nil {
		return 1, err
	}

	orch := &reboot.Orchestrator{
		Config: opts.config,
		Runner: reboot.ExecRunner{},
		Clock:  reboot.RealClock{},
		Out:    stdout,
	}

	// Probe before confirming, so the operator approves the work that will
	// actually happen rather than the full list of candidates.
	plan, unprobed := orch.SelectPending(ctx, plan)
	if len(plan.Targets) == 0 {
		_, _ = fmt.Fprintln(stdout, "\nNothing to reboot.")
		reportUnprobed(unprobed)
		if len(unprobed) > 0 {
			return 1, nil
		}
		return 0, nil
	}

	_, _ = fmt.Fprintln(stdout,
		"\nThe following hosts will be rebooted (parents first, nested dependents last):")
	reboot.PrintTree(stdout, plan.Hosts, plan.Targets)

	if !opts.yes {
		ok, err := confirm(stdinConsumed, stdout)
		if err != nil {
			return 1, err
		}
		if !ok {
			_, _ = fmt.Fprintln(stdout, "Aborting.")
			return 0, nil
		}
	}

	result, err := orch.Run(ctx, plan)
	// A cancelled run still reports what it managed to establish, so the
	// operator knows which tiers landed before they interrupted it.
	unprobed = append(unprobed, result.Unprobed...)
	reportUnprobed(unprobed)

	notRebooted := result.NotRebooted()
	if len(notRebooted) > 0 {
		names := make([]string, 0, len(notRebooted))
		for _, v := range notRebooted {
			names = append(names, v.Host)
		}
		_, _ = fmt.Fprintf(os.Stderr, "FATAL: these hosts never rebooted: %s\n", strings.Join(names, ", "))
	}

	switch {
	case errors.Is(err, context.Canceled):
		// Interrupting mid-run is a legitimate way out, not a crash. Say so
		// plainly, and keep the exit non-zero: tiers were left unrebooted.
		return 1, errors.New("interrupted; some tiers may not have been rebooted")
	case err != nil:
		return 1, err
	case len(unprobed) > 0, len(notRebooted) > 0:
		return 1, nil
	}
	return 0, nil
}

// parseArgs parses flags and returns them alongside the positional host specs.
func parseArgs(args []string) (options, []string, error) {
	var opts options

	fs := flag.NewFlagSet("reboot-orchestrator", flag.ContinueOnError)
	fs.StringVar(&opts.user, "user", "", "SSH user for hosts that do not set one themselves")
	fs.Var(&opts.sshArgs, "ssh-arg", "extra argument passed to every ssh invocation (repeatable)")
	fs.StringVar(&opts.hostsFrom, "hosts-from", "", "read host specs from this file (- for stdin)")
	fs.BoolVar(&opts.all, "all", false, "target every host read from stdin or --hosts-from")
	fs.BoolVar(&opts.yes, "yes", false, "bypass the interactive confirmation prompt")
	fs.BoolVar(&opts.yes, "y", false, "bypass the interactive confirmation prompt (shorthand)")
	fs.BoolVar(&opts.ifNeeded, "if-needed", false,
		"reboot only the targeted hosts that report a pending reboot")
	fs.BoolVar(&opts.skipVerify, "skip-boot-verification", false,
		"do not read boot_id/uptime over SSH to confirm hosts actually rebooted")
	fs.DurationVar(&opts.config.PingTimeout, "ping-timeout", time.Second,
		"timeout for a single ping query")
	fs.DurationVar(&opts.config.DropWait, "drop-wait", 15*time.Second,
		"how long to wait for a host to drop off the network before giving up on seeing it")
	fs.DurationVar(&opts.config.SampleInterval, "sample-interval", time.Second,
		"how often to probe each host while it reboots")
	fs.DurationVar(&opts.config.ProbeTimeout, "probe-timeout", 15*time.Second,
		"timeout for each SSH boot state probe")
	fs.Usage = func() {
		out := fs.Output()
		_, _ = fmt.Fprintf(out,
			"Usage: %s [flags] HOST_SPEC...\n\n"+
				"Reboot hosts in dependency order over SSH, watching each tier leave the\n"+
				"network and come back, and proving it actually restarted before starting\n"+
				"the next.\n\n"+
				"A HOST_SPEC is a host name followed by optional comma-separated fields:\n"+
				"  addr       address to ping and ssh to (default: the host name)\n"+
				"  user       ssh login user (default: --user)\n"+
				"  ssh-arg    extra ssh argument; repeatable\n"+
				"  after      reboot this host only once the named host is back. An ordering\n"+
				"             and nothing more; repeatable\n"+
				"  runs-on    the host this one is hosted by, whose reboot restarts it. Orders\n"+
				"             like after, and lets the carried reboot be credited rather than\n"+
				"             given twice; at most one\n"+
				"  not-with   never reboot this host in the same tier as the named one.\n"+
				"             Symmetric, and orders nothing; repeatable\n"+
				"  ready      the command proving this host is back, run on it over ssh\n"+
				"             (default: true, meaning a completed login)\n\n"+
				"Specs also arrive on stdin, one per line, which is how a topology kept in\n"+
				"an Ansible inventory reaches this tool:\n\n"+
				"  ansible-inventory-reboot-hosts -i inventory.yml | %s vm-a vm-b\n\n"+
				"Hosts read from stdin are context: they can be depended on and are waited\n"+
				"for, but only the hosts named as arguments are rebooted. Naming one that\n"+
				"stdin already defined targets that definition rather than replacing it.\n"+
				"Use --all to target every host from stdin instead.\n\nFlags:\n",
			os.Args[0], os.Args[0])
		fs.PrintDefaults()
	}

	// flag has already reported the problem and printed usage by this point,
	// including for -h, which run distinguishes so that asking for help is not
	// treated as a failed run.
	if err := fs.Parse(args); err != nil {
		return opts, nil, err
	}

	opts.config.VerifyBootState = !opts.skipVerify
	opts.config.IfNeeded = opts.ifNeeded
	return opts, fs.Args(), nil
}

// buildPlan assembles the host set from stdin and the command line. It reports
// whether stdin was consumed, which decides where a confirmation prompt can
// still be read from.
func buildPlan(opts options, targetSpecs []string, stdin io.Reader) (reboot.Plan, bool, error) {
	var (
		contextHosts  []reboot.Host
		stdinConsumed bool
		err           error
	)

	switch {
	case opts.hostsFrom == "-":
		contextHosts, err = reboot.ReadSpecs(stdin)
		stdinConsumed = true
	case opts.hostsFrom != "":
		contextHosts, err = readSpecFile(opts.hostsFrom)
	case isPipe(stdin):
		// A piped stdin is unambiguous: nothing else would be feeding it, and
		// requiring a flag to read an obvious pipe only invites forgetting it.
		contextHosts, err = reboot.ReadSpecs(stdin)
		stdinConsumed = true
	}
	if err != nil {
		return reboot.Plan{}, stdinConsumed, err
	}

	targets, err := reboot.ParseSpecs(targetSpecs)
	if err != nil {
		return reboot.Plan{}, stdinConsumed, err
	}

	if len(contextHosts) == 0 && len(targets) == 0 {
		return reboot.Plan{}, stdinConsumed, errors.New(
			"no hosts given: name at least one host, or pipe host specs in on stdin")
	}
	if opts.all && len(targets) > 0 {
		return reboot.Plan{}, stdinConsumed, errors.New(
			"--all targets every host read from stdin; do not also name hosts")
	}

	defaults := reboot.Defaults{User: opts.user, SSHArgs: opts.sshArgs}
	plan, err := reboot.BuildPlan(contextHosts, targets, defaults, opts.all)
	return plan, stdinConsumed, err
}

// readSpecFile reads host specs from a file.
func readSpecFile(path string) ([]reboot.Host, error) {
	file, err := os.Open(path) //nolint:gosec // the path is the operator's own argument
	if err != nil {
		return nil, fmt.Errorf("cannot read host specs: %w", err)
	}
	defer func() { _ = file.Close() }()
	return reboot.ReadSpecs(file)
}

// isPipe reports whether stdin is a pipe or a redirected file rather than a
// terminal.
func isPipe(stdin io.Reader) bool {
	file, ok := stdin.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// confirm asks the operator to approve the run.
//
// When host specs came in on stdin there is no answer to be read from it, so
// the terminal is opened directly. A run with neither a terminal nor --yes is
// refused rather than assumed approved.
func confirm(stdinConsumed bool, out io.Writer) (bool, error) {
	var source io.Reader = os.Stdin
	if stdinConsumed {
		tty, err := os.Open("/dev/tty")
		if err != nil {
			return false, errors.New(
				"host specs were read from stdin and no terminal is available to confirm on; pass --yes")
		}
		defer func() { _ = tty.Close() }()
		source = tty
	}

	_, _ = fmt.Fprint(out, "\nProceed with tiered orchestration? [y/N]: ")
	scanner := bufio.NewScanner(source)
	if !scanner.Scan() {
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(scanner.Text()), "y"), nil
}

// reportUnprobed warns about hosts that could not be checked. They are excluded
// from the reboot set, so a caller driving this from a script has to be told
// that something went unchecked rather than finding a quiet success.
func reportUnprobed(statuses []reboot.RebootStatus) {
	if len(statuses) == 0 {
		return
	}
	_, _ = fmt.Fprintln(os.Stderr, "\nWARNING: these hosts could not be checked:")
	for _, status := range statuses {
		_, _ = fmt.Fprintf(os.Stderr, "  %s: %s\n", status.Host, status.Reason)
	}
}
