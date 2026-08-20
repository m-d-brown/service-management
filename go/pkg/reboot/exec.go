package reboot

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/kballard/go-shellquote"
)

// Runner executes the external commands the orchestrator drives infrastructure
// with. It exists so tests can substitute a fake for invocations that would
// otherwise reboot real machines.
type Runner interface {
	// Run executes a command, feeding it stdin, and returns its stdout. The
	// context bounds how long the command may take.
	Run(ctx context.Context, stdin, name string, args ...string) (string, error)
	// Start dispatches a command without waiting for it to finish. Commands
	// that sever their own connection — a reboot, a power off — are started
	// this way so a dropped SSH session cannot stall the run.
	Start(name string, args ...string) error
}

// ExecRunner runs commands for real.
type ExecRunner struct{}

// Run executes the command and returns its stdout, folding stderr into the
// error so a failure explains itself.
func (ExecRunner) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return stdout.String(), fmt.Errorf("%s: %w", name, ctx.Err())
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return stdout.String(), fmt.Errorf("%s: %w", name, err)
		}
		return stdout.String(), fmt.Errorf("%s: %w: %s", name, err, detail)
	}
	return stdout.String(), nil
}

// Start dispatches a command and returns as soon as it is running. Output is
// discarded: these commands sever their own connection by design, so what they
// print on the way down is noise. A reboot that silently failed to land is
// caught afterwards by boot state verification, not here.
func (ExecRunner) Start(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	// Reap the child in the background. Without this the orchestrator, which
	// outlives every command it starts, would accumulate zombie processes for
	// the whole run.
	go func() { _ = cmd.Wait() }()
	return nil
}

// Clock supplies the passage of time, so tests neither sleep nor depend on the
// wall clock.
type Clock interface {
	// Now reports the current time.
	Now() time.Time
	// Sleep blocks for the given duration.
	Sleep(d time.Duration)
}

// RealClock is the production Clock.
type RealClock struct{}

// Now reports the current time.
func (RealClock) Now() time.Time { return time.Now() }

// Sleep blocks for the given duration.
func (RealClock) Sleep(d time.Duration) { time.Sleep(d) }

// report writes a progress line. Progress output is best-effort, so write
// errors are deliberately ignored.
func report(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// FormatCommand renders an argument vector as a copy-pasteable shell line.
//
// Every command the orchestrator runs is echoed before it runs. Because the
// tool mutates infrastructure through fire-and-forget subprocesses, an operator
// watching a run can see exactly what was attempted against which host, and
// reproduce any step by hand — which only holds if the rendering is quoted
// correctly, so it is delegated rather than approximated.
func FormatCommand(name string, args ...string) string {
	return shellquote.Join(append([]string{name}, args...)...)
}
