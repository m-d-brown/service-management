package reboot

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// errPingFailed stands in for a host that did not answer an echo request.
var errPingFailed = errors.New("ping: no answer from host")

// reachability is what one host answers at one point in a scripted run.
type reachability struct {
	// ping is whether it answers ICMP.
	ping bool
	// ssh is whether it accepts an SSH session.
	ssh bool
}

// up and down are the two states a host spends most of a script in.
var (
	up   = reachability{ping: true, ssh: true}
	down = reachability{}
)

// scriptedRunner walks each host through a sequence of reachability states, one
// entry per sample, holding the last entry once the script runs out.
//
// A sample is counted per ping, and the SSH probe that may follow within the
// same sweep reads the same entry — so one entry describes one whole sample of
// one host, however many commands that takes.
func scriptedRunner(script map[string][]reachability) *fakeRunner {
	var mu sync.Mutex
	samples := map[string]int{}

	state := func(host string, isPing bool) reachability {
		mu.Lock()
		defer mu.Unlock()
		seq := script[host]
		if len(seq) == 0 {
			return up
		}
		i := samples[host]
		if isPing {
			samples[host]++
		} else {
			i--
		}
		if i >= len(seq) {
			i = len(seq) - 1
		}
		if i < 0 {
			i = 0
		}
		return seq[i]
	}

	return &fakeRunner{respond: func(c call) (string, error) {
		switch c.name {
		case "ping":
			if state(c.args[len(c.args)-1], true).ping {
				return "", nil
			}
			return "", errPingFailed
		case "ssh":
			if state(c.args[len(c.args)-2], false).ssh {
				return "", nil
			}
			return "", errSSHFailed
		}
		return "", nil
	}}
}

// runMonitor watches one host through a script and returns what was observed.
func runMonitor(t *testing.T, host string, script []reachability, dropWait time.Duration) (Cycle, string) {
	t.Helper()
	var out bytes.Buffer
	runner := scriptedRunner(map[string][]reachability{host: script})

	monitor := StartMonitor(context.Background(), &out, runner, newFakeClock(),
		[]Host{{Name: host}}, time.Second, time.Second, time.Second, dropWait)
	monitor.StartDropWait()
	if err := monitor.WaitForReturn(context.Background()); err != nil {
		t.Fatalf("WaitForReturn() = %v, want nil", err)
	}
	monitor.Stop()
	return monitor.Cycles()[host], out.String()
}

func TestMonitorObservesTheWholePowerCycle(t *testing.T) {
	// Up for two samples, gone for three, then back.
	cycle, out := runMonitor(t, "vm-a",
		[]reachability{up, up, down, down, down, up}, time.Minute)

	if !cycle.Complete() {
		t.Fatalf("Cycle = %+v, want a completed power cycle", cycle)
	}
	// The drop is the third sample and the return the sixth; the first is taken
	// before the ticker starts, so each later sample is one interval on.
	if got := cycle.DownFor(); got != 3*time.Second {
		t.Errorf("DownFor() = %v, want 3s", got)
	}
	for _, want := range []string{
		"[down] vm-a stopped answering",
		"[back] vm-a is back",
		"after 3s down",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output = %q, want it to contain %q", out, want)
		}
	}
}

func TestMonitorWaitsForSSHNotPing(t *testing.T) {
	// The kernel answers ping well before sshd accepts a connection. A host is
	// not back until it can actually be used.
	pingOnly := reachability{ping: true}
	cycle, out := runMonitor(t, "vm-a",
		[]reachability{up, down, pingOnly, pingOnly, up}, time.Minute)

	if !cycle.Complete() {
		t.Fatalf("Cycle = %+v, want a completed power cycle", cycle)
	}
	// Down at sample 2, ping back at 3, SSH back at 5: the two intervening
	// samples answered ping and must not have counted as a return.
	if got := cycle.DownFor(); got != 3*time.Second {
		t.Errorf("DownFor() = %v, want the host back only once SSH answered", got)
	}
	if !strings.Contains(out, "[ping] vm-a answers ping again") {
		t.Errorf("output = %q, want the ping-but-not-SSH stage reported", out)
	}
	if strings.Count(out, "[ping] vm-a") != 1 {
		t.Errorf("output = %q, want the ping stage reported once, not every sample", out)
	}
}

func TestMonitorReportsAHostThatNeverDrops(t *testing.T) {
	cycle, out := runMonitor(t, "web1", []reachability{up}, 3*time.Second)

	if cycle.Dropped {
		t.Errorf("Cycle = %+v, want no drop observed", cycle)
	}
	if !cycle.StayedUp() {
		t.Errorf("StayedUp() = false, want a host that answered the whole drop wait")
	}
	if !strings.Contains(out, "[warn] web1 answered every probe") {
		t.Errorf("output = %q, want the never-dropped warning", out)
	}
	if got := strings.Count(out, "[warn] web1"); got != 1 {
		t.Errorf("warning count = %d, want it said once rather than every sample", got)
	}
}

func TestMonitorTakesItsFirstSampleBeforeReturning(t *testing.T) {
	// The first sample has to be in before the caller powers anything down,
	// or the drop it is meant to catch happens unobserved.
	var out bytes.Buffer
	runner := scriptedRunner(nil)
	monitor := StartMonitor(context.Background(), &out, runner, newFakeClock(),
		[]Host{{Name: "vm-a"}}, time.Second, time.Second, time.Second, time.Minute)
	defer monitor.Stop()

	if got := runner.countMatching("ping"); got == 0 {
		t.Error("StartMonitor() returned without sampling; the baseline would be missed")
	}
}

func TestMonitorStopsSamplingWhenStopped(t *testing.T) {
	var out bytes.Buffer
	runner := scriptedRunner(nil)
	monitor := StartMonitor(context.Background(), &out, runner, newFakeClock(),
		[]Host{{Name: "web1"}}, time.Second, time.Second, time.Second, time.Second)
	monitor.StartDropWait()
	if err := monitor.WaitForReturn(context.Background()); err != nil {
		t.Fatalf("WaitForReturn() = %v, want nil", err)
	}
	monitor.Stop()

	settled := runner.countMatching("ping")
	// Stop waits for the sampler to exit, so nothing may probe after it returns.
	if got := runner.countMatching("ping"); got != settled {
		t.Errorf("probes went from %d to %d after Stop", settled, got)
	}
}

func TestMonitorHonoursCancellation(t *testing.T) {
	var out bytes.Buffer
	// A host that drops and never comes back would otherwise be waited on
	// forever; cancelling has to be a way out.
	runner := scriptedRunner(map[string][]reachability{"vm-a": {up, down}})
	ctx, cancel := context.WithCancel(context.Background())

	monitor := StartMonitor(ctx, &out, runner, newFakeClock(),
		[]Host{{Name: "vm-a"}}, time.Second, time.Second, time.Second, time.Minute)
	cancel()

	if err := monitor.WaitForReturn(ctx); err == nil {
		t.Error("WaitForReturn() = nil, want the cancellation reported")
	}
	monitor.Stop()
}

func TestMonitorReportsEachHostSeparately(t *testing.T) {
	var out bytes.Buffer
	runner := scriptedRunner(map[string][]reachability{
		"vm-a": {up, down, down, up},
		"vm-b": {up},
	})
	monitor := StartMonitor(context.Background(), &out, runner, newFakeClock(),
		[]Host{{Name: "vm-a"}, {Name: "vm-b"}}, time.Second, time.Second, time.Second, 3*time.Second)
	monitor.StartDropWait()
	if err := monitor.WaitForReturn(context.Background()); err != nil {
		t.Fatalf("WaitForReturn() = %v, want nil", err)
	}
	monitor.Stop()

	cycles := monitor.Cycles()
	if !cycles["vm-a"].Complete() {
		t.Errorf("vm-a = %+v, want a completed cycle", cycles["vm-a"])
	}
	if !cycles["vm-b"].StayedUp() {
		t.Errorf("vm-b = %+v, want a host that never dropped", cycles["vm-b"])
	}
}

func TestMonitorWithNoHostsSettlesImmediately(t *testing.T) {
	var out bytes.Buffer
	monitor := StartMonitor(context.Background(), &out, scriptedRunner(nil), newFakeClock(),
		nil, time.Second, time.Second, time.Second, time.Minute)
	monitor.StartDropWait()
	if err := monitor.WaitForReturn(context.Background()); err != nil {
		t.Fatalf("WaitForReturn() = %v, want nil", err)
	}
	monitor.Stop()

	if out.Len() != 0 {
		t.Errorf("output = %q, want nothing said about an empty watch list", out.String())
	}
}

func TestMonitorDropWaitStartsOnlyWhenTold(t *testing.T) {
	// Sampling starts before anything is powered down, so the drop wait cannot
	// start with it: it would run out while the run was still issuing the
	// commands it is timing. That is what warned about hosts which had not been
	// asked to go anywhere yet, and let a tier settle before any host had.
	clock := newFakeClock()
	m := &Monitor{clock: clock, dropWait: 3 * time.Second}

	if m.dropWaitElapsed(clock.Now().Add(time.Hour)) {
		t.Error("a drop wait that never started counted as elapsed, an hour or not")
	}

	// Started, it runs for the configured wait and no longer.
	m.StartDropWait()
	if m.dropWaitElapsed(clock.Now().Add(2 * time.Second)) {
		t.Error("the drop wait elapsed inside the wait it was started for")
	}
	if !m.dropWaitElapsed(clock.Now().Add(4 * time.Second)) {
		t.Error("the drop wait never elapsed, a full wait past starting")
	}
}

func TestMonitorJudgesNoHostBeforeTheDropWaitStarts(t *testing.T) {
	// Until the drop wait starts the monitor still watches and still reports
	// drops; what it must not do is conclude anything from a host answering.
	var out bytes.Buffer
	runner := scriptedRunner(map[string][]reachability{"web1": {up}})
	monitor := StartMonitor(context.Background(), &out, runner, newFakeClock(),
		[]Host{{Name: "web1"}}, time.Second, time.Second, time.Second, time.Second)
	defer monitor.Stop()

	if cycle := monitor.Cycles()["web1"]; cycle.DropWaitElapsed {
		t.Errorf("Cycle = %+v, want no elapsed drop wait before one started", cycle)
	}
	if cycle := monitor.Cycles()["web1"]; cycle.StayedUp() {
		t.Error("StayedUp() = true before the host had been asked to go anywhere")
	}
	if strings.Contains(out.String(), "[warn]") {
		t.Errorf("output = %q, want no verdict before the drop wait started", out.String())
	}
}

func TestCycleZeroValueIsNotEvidence(t *testing.T) {
	// A host nobody watched has not been seen to stay up. Reading the zero
	// value as evidence would fail hosts that rebooted perfectly well.
	var unwatched Cycle
	if unwatched.Complete() {
		t.Error("Complete() = true for an unwatched host")
	}
	if unwatched.StayedUp() {
		t.Error("StayedUp() = true for an unwatched host")
	}
	// Nor has one whose drop wait had not run out.
	partial := Cycle{Host: "web1", Watched: true}
	if partial.StayedUp() {
		t.Error("StayedUp() = true before the drop wait elapsed")
	}
}
