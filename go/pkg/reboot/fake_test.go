package reboot

// Test doubles shared across this package's tests. Every side effect the
// orchestrator has — SSH, ping, sleeping — goes through Runner or Clock, so
// substituting these two keeps the whole engine testable without a network.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

// errSSHFailed stands in for any reason a connection did not complete.
var errSSHFailed = errors.New("ssh: connect to host port 22: connection refused")

// call is one command a fake runner was asked to execute.
type call struct {
	// name is the executable.
	name string
	// args are its arguments.
	args []string
	// stdin is what was piped to it.
	stdin string
	// started records whether it was dispatched rather than awaited.
	started bool
}

// line renders the call as a shell command, for assertions.
func (c call) line() string { return FormatCommand(c.name, c.args...) }

// remote returns the command sent to the far end of an ssh invocation.
func (c call) remote() string {
	if c.name != "ssh" || len(c.args) == 0 {
		return ""
	}
	return c.args[len(c.args)-1]
}

// fakeRunner scripts command results and records everything it was asked to
// run. It is safe for the concurrent use ProbeHosts makes of it.
type fakeRunner struct {
	mu sync.Mutex
	// calls is every command, in the order it was issued.
	calls []call
	// respond returns the stdout and error for a command. A nil respond
	// succeeds silently, which suits commands whose output is never read.
	respond func(c call) (string, error)
	// startErr is returned by every Start call when set.
	startErr error
	// onStart, when set, runs for each dispatched command, letting a test
	// model the effect a fire-and-forget command has on its host.
	onStart func(c call)
}

// Run records the call and returns its scripted result.
func (f *fakeRunner) Run(ctx context.Context, stdin, name string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c := call{name: name, args: args, stdin: stdin}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	respond := f.respond
	f.mu.Unlock()
	if respond == nil {
		return "", nil
	}
	return respond(c)
}

// Start records a dispatched command and applies its modelled effect.
func (f *fakeRunner) Start(name string, args ...string) error {
	c := call{name: name, args: args, started: true}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	onStart, startErr := f.onStart, f.startErr
	f.mu.Unlock()
	if onStart != nil && startErr == nil {
		onStart(c)
	}
	return startErr
}

// lines returns every recorded command as a shell line.
func (f *fakeRunner) lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.line())
	}
	return out
}

// remotes returns the remote command of every ssh invocation.
func (f *fakeRunner) remotes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		if remote := c.remote(); remote != "" {
			out = append(out, remote)
		}
	}
	return out
}

// countMatching counts recorded commands whose shell line contains substr.
func (f *fakeRunner) countMatching(substr string) int {
	n := 0
	for _, line := range f.lines() {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// fakeClock advances only when something sleeps, so tests exercise the real
// waiting logic instantly and can assert on how long was waited.
type fakeClock struct {
	mu sync.Mutex
	// now is the current instant.
	now time.Time
	// slept is every duration slept, in order.
	slept []time.Duration
}

// newFakeClock returns a clock at a fixed, readable instant.
func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)}
}

// Now reports the current fake time.
func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// Sleep advances the fake time without blocking.
func (f *fakeClock) Sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.slept = append(f.slept, d)
	f.now = f.now.Add(d)
}

// total returns the sum of every sleep.
func (f *fakeClock) total() time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	var sum time.Duration
	for _, d := range f.slept {
		sum += d
	}
	return sum
}
