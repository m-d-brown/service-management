package reboot

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fleet models the hosts an orchestration run acts on: each one has a boot
// identity that changes when it is told to reboot, which is what lets a test
// tell a real reboot from a host that merely stayed reachable.
type fleet struct {
	mu sync.Mutex
	// bootIDs is the current boot identity per ssh destination.
	bootIDs map[string]string
	// ignoresReboot names destinations that accept the command and stay up,
	// the failure boot state verification exists to catch.
	ignoresReboot map[string]bool
	// pending names destinations reporting that they await a reboot.
	pending map[string]bool
	// unprobeable names destinations whose pending-reboot probe fails.
	unprobeable map[string]bool
	// reboots counts reboot commands per destination.
	reboots map[string]int
	// markerless names destinations exposing neither boot_id nor uptime, the
	// appliances whose reboots only the monitor can vouch for.
	markerless map[string]bool
	// downFor is how many more probes a destination will refuse, modelling the
	// stretch of a reboot where the host is off the network.
	downFor map[string]int
	// guests names the destinations that go down with a destination, as a
	// hypervisor's guests go down with it whether or not anyone rebooted them.
	guests map[string][]string
}

// rebootDownSamples is how many probes a rebooting host misses before it
// answers again. More than one, so a drop is distinguishable from a blip.
const rebootDownSamples = 3

// newFleet builds a fleet where every named destination is up to date.
func newFleet(destinations ...string) *fleet {
	f := &fleet{
		bootIDs:       map[string]string{},
		ignoresReboot: map[string]bool{},
		pending:       map[string]bool{},
		unprobeable:   map[string]bool{},
		reboots:       map[string]int{},
		markerless:    map[string]bool{},
		downFor:       map[string]int{},
		guests:        map[string][]string{},
	}
	for _, dest := range destinations {
		f.bootIDs[dest] = "boot-" + dest + "-0"
	}
	return f
}

// runner returns a Runner wired to this fleet.
func (f *fleet) runner() *fakeRunner {
	return &fakeRunner{respond: f.respond, onStart: f.start}
}

// destination extracts the address an ssh call was aimed at.
//
// The user is stripped so a host is keyed the same way however it is reached:
// ssh addresses it as user@addr while ping uses the bare address, and a fleet
// that keyed them separately would model one host as two.
func destination(c call) string {
	if len(c.args) < 2 {
		return ""
	}
	dest := c.args[len(c.args)-2]
	if _, addr, found := strings.Cut(dest, "@"); found {
		return addr
	}
	return dest
}

// respond answers the commands the orchestrator awaits.
func (f *fleet) respond(c call) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if c.name == "ping" {
		// One ping per host per sample, so counting them down here is what
		// gives a rebooting host a plausible stretch of being unreachable.
		addr := c.args[len(c.args)-1]
		if f.downFor[addr] > 0 {
			f.downFor[addr]--
			return "", errPingFailed
		}
		return "", nil
	}
	dest := destination(c)
	if f.downFor[dest] > 0 {
		return "", errSSHFailed
	}
	switch {
	case c.remote() == bootProbeCommand:
		if f.markerless[dest] {
			return "boot_id=\nuptime=\n", nil
		}
		return fmt.Sprintf("boot_id=%s\nuptime=1000.0\n", f.bootIDs[dest]), nil
	case c.remote() == "/bin/sh":
		if f.unprobeable[dest] {
			return "", errSSHFailed
		}
		if f.pending[dest] {
			return "NEEDED packages awaiting restart: libc6\n", nil
		}
		return "OK no pending reboot flag\n", nil
	default:
		return "", nil
	}
}

// start applies the effect of a dispatched command.
func (f *fleet) start(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()

	dest := destination(c)
	if !strings.Contains(c.remote(), "reboot") {
		return
	}
	f.reboots[dest]++
	if f.ignoresReboot[dest] {
		return
	}
	// The host leaves the network for a while, which is what the monitor is
	// watching for, and takes anything running on it down too.
	f.downFor[dest] = rebootDownSamples
	for _, guest := range f.guests[dest] {
		f.downFor[guest] = rebootDownSamples
	}
	// A real reboot regenerates the kernel's boot id and clears any pending
	// reboot the host was reporting.
	f.bootIDs[dest] = fmt.Sprintf("boot-%s-%d", dest, f.reboots[dest])
	delete(f.pending, dest)
}

// testConfig is a run with every wait shortened; the fake clock makes them
// instant regardless, but the values still show up in assertions.
func testConfig() Config {
	return Config{
		PingTimeout:     time.Second,
		DropWait:        15 * time.Second,
		ProbeTimeout:    15 * time.Second,
		SampleInterval:  time.Second,
		VerifyBootState: true,
	}
}

// newPlan builds a plan from specs, targeting the named hosts.
func newPlan(t *testing.T, specs []string, targets ...string) Plan {
	t.Helper()
	context, err := ParseSpecs(specs)
	if err != nil {
		t.Fatal(err)
	}
	targetHosts, err := ParseSpecs(targets)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildPlan(context, targetHosts, Defaults{}, len(targets) == 0)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestRunRebootsInTierOrder(t *testing.T) {
	f := newFleet("10.0.0.5", "10.0.0.21", "10.0.0.22")
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,after=hv1",
		"vm-b,addr=10.0.0.22,after=vm-a",
	})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Verifications) != 3 {
		t.Fatalf("got %d verifications, want 3", len(result.Verifications))
	}
	for _, v := range result.Verifications {
		if v.Status != StatusConfirmed {
			t.Errorf("%s: status = %v (%s), want confirmed", v.Host, v.Status, v.Detail)
		}
	}
	if len(result.NotRebooted()) != 0 {
		t.Errorf("NotRebooted() = %v, want none", result.NotRebooted())
	}

	// Each dependency level is its own tier, so a parent is fully back before
	// anything behind it is touched.
	rebootOrder := rebootedDestinations(runner)
	want := []string{"10.0.0.5", "10.0.0.21", "10.0.0.22"}
	equalStrings(t, "reboot order", rebootOrder, want)

	for i := 1; i <= 3; i++ {
		if !strings.Contains(out.String(), fmt.Sprintf("=== Executing Tier: %d ===", i)) {
			t.Errorf("output is missing tier %d", i)
		}
	}
	if !strings.Contains(out.String(), "finished successfully") {
		t.Errorf("output = %q, want a successful closing summary", out.String())
	}
}

// rebootedDestinations returns the destinations sent a reboot, in order.
func rebootedDestinations(runner *fakeRunner) []string {
	var out []string
	for _, c := range runner.calls {
		if c.started && strings.Contains(c.remote(), "reboot") {
			out = append(out, destination(c))
		}
	}
	return out
}

func TestRunReportsHostThatNeverRebooted(t *testing.T) {
	// The whole point of boot state verification: this host answers ping
	// throughout and is indistinguishable from a healthy one without it.
	f := newFleet("10.0.0.5", "10.0.0.21")
	f.ignoresReboot["10.0.0.21"] = true
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{"hv1,addr=10.0.0.5", "vm-a,addr=10.0.0.21,after=hv1"})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	notRebooted := result.NotRebooted()
	if len(notRebooted) != 1 || notRebooted[0].Host != "vm-a" {
		t.Fatalf("NotRebooted() = %v, want just vm-a", notRebooted)
	}
	if !strings.Contains(notRebooted[0].Detail, "boot_id is unchanged") {
		t.Errorf("detail = %q, want the unchanged boot id cited", notRebooted[0].Detail)
	}

	// The run still completes every tier: stopping half way would leave the
	// fleet in a state nobody asked for.
	if len(result.Verifications) != 2 {
		t.Errorf("got %d verifications, want both tiers attempted", len(result.Verifications))
	}
	if !strings.Contains(out.String(), "did NOT reboot") {
		t.Errorf("output = %q, want the failure called out", out.String())
	}
	if !strings.Contains(out.String(), "could not be confirmed as rebooted") {
		t.Errorf("output = %q, want the summary to flag the unconfirmed host", out.String())
	}
}

func TestRunWaitsForUntargetedDependents(t *testing.T) {
	// vm-a is not being rebooted, but it goes down with the hypervisor it sits
	// on, so the run cannot move on until it has answered again.
	f := newFleet("10.0.0.5", "10.0.0.21")
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,after=hv1",
	}, "hv1")

	if _, err := orch.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	if got := runner.countMatching("ping -c 1 -W 1 10.0.0.21"); got == 0 {
		t.Error("vm-a was never pinged, want the untargeted dependent waited for")
	}
	if got := rebootedDestinations(runner); len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("rebooted %v, want only the targeted hypervisor", got)
	}
}

func TestRunIfNeededRechecksLaterTiers(t *testing.T) {
	// Rebooting the hypervisor power-cycles the guest behind it, so by the time
	// the guest's tier comes up its earlier verdict is stale. Acting on it
	// would reboot the guest a second time for an update already applied.
	f := newFleet("10.0.0.5", "10.0.0.21")
	f.pending["10.0.0.5"] = true
	f.pending["10.0.0.21"] = true

	// Model the hypervisor's reboot clearing its guest's pending flag.
	inner := f.start
	runner := f.runner()
	runner.onStart = func(c call) {
		inner(c)
		if destination(c) == "10.0.0.5" && strings.Contains(c.remote(), "reboot") {
			f.mu.Lock()
			delete(f.pending, "10.0.0.21")
			f.mu.Unlock()
		}
	}

	var out bytes.Buffer
	cfg := testConfig()
	cfg.IfNeeded = true
	orch := &Orchestrator{Config: cfg, Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,after=hv1",
	})

	plan, unprobed := orch.SelectPending(context.Background(), plan)
	if len(unprobed) != 0 {
		t.Fatalf("unprobed = %v, want none", unprobed)
	}
	equalStrings(t, "targets after the initial probe", plan.Targets, []string{"hv1", "vm-a"})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	if got := rebootedDestinations(runner); len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("rebooted %v, want only the hypervisor", got)
	}
	if !strings.Contains(out.String(), "Skipping vm-a: no longer needs a reboot.") {
		t.Errorf("output = %q, want the skip announced", out.String())
	}
	if !strings.Contains(out.String(), "Tier 2 is already up to date") {
		t.Errorf("output = %q, want the emptied tier reported", out.String())
	}
	// A host that was never rebooted must not appear in the verification
	// summary as one that failed to reboot.
	for _, v := range result.Verifications {
		if v.Host == "vm-a" {
			t.Errorf("vm-a was verified (%v) despite never being rebooted", v.Status)
		}
	}
}

func TestSelectPendingNarrowsToPendingHosts(t *testing.T) {
	f := newFleet("10.0.0.5", "10.0.0.21", "10.0.0.22")
	f.pending["10.0.0.21"] = true
	f.unprobeable["10.0.0.22"] = true

	var out bytes.Buffer
	cfg := testConfig()
	cfg.IfNeeded = true
	orch := &Orchestrator{Config: cfg, Runner: f.runner(), Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21",
		"vm-b,addr=10.0.0.22",
	})

	plan, unprobed := orch.SelectPending(context.Background(), plan)

	equalStrings(t, "targets", plan.Targets, []string{"vm-a"})
	// An unprobed host is excluded from the reboot set but reported, so a
	// caller cannot mistake it for one that was checked and found current.
	if len(unprobed) != 1 || unprobed[0].Host != "vm-b" {
		t.Errorf("unprobed = %v, want just vm-b", unprobed)
	}
}

func TestSelectPendingPassesThroughWhenDisabled(t *testing.T) {
	f := newFleet("10.0.0.5")
	runner := f.runner()
	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &bytes.Buffer{}}
	plan := newPlan(t, []string{"hv1,addr=10.0.0.5"})

	got, unprobed := orch.SelectPending(context.Background(), plan)

	equalStrings(t, "targets", got.Targets, []string{"hv1"})
	if len(unprobed) != 0 {
		t.Errorf("unprobed = %v, want none", unprobed)
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands, want none without --if-needed", len(runner.calls))
	}
}

func TestRunSkipsVerificationWhenDisabled(t *testing.T) {
	f := newFleet("10.0.0.5")
	runner := f.runner()
	cfg := testConfig()
	cfg.VerifyBootState = false

	orch := &Orchestrator{Config: cfg, Runner: runner, Clock: newFakeClock(), Out: &bytes.Buffer{}}
	plan := newPlan(t, []string{"hv1,addr=10.0.0.5"})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Verifications) != 0 {
		t.Errorf("got %d verifications, want none", len(result.Verifications))
	}
	if got := runner.countMatching("boot_id"); got != 0 {
		t.Errorf("ran %d boot probes, want none", got)
	}
	if got := rebootedDestinations(runner); len(got) != 1 {
		t.Errorf("rebooted %v, want the host still rebooted", got)
	}
}

func TestRunNoTargets(t *testing.T) {
	runner := &fakeRunner{}
	var out bytes.Buffer
	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}

	result, err := orch.Run(context.Background(), Plan{Hosts: Hosts{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Verifications) != 0 {
		t.Errorf("got %d verifications, want none", len(result.Verifications))
	}
	if len(runner.calls) != 0 {
		t.Errorf("ran %d commands with no targets, want none", len(runner.calls))
	}
}

func TestRunBaselineIsTakenBeforeReboot(t *testing.T) {
	// The baseline has to be recorded before the host goes down, or the reboot
	// races the probe and there is nothing left to compare against.
	f := newFleet("10.0.0.5")
	runner := f.runner()
	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &bytes.Buffer{}}
	plan := newPlan(t, []string{"hv1,addr=10.0.0.5"})

	if _, err := orch.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	firstProbe, firstReboot := -1, -1
	for i, c := range runner.calls {
		if c.remote() == bootProbeCommand && firstProbe < 0 {
			firstProbe = i
		}
		if strings.Contains(c.remote(), "reboot") && firstReboot < 0 {
			firstReboot = i
		}
	}
	if firstProbe < 0 || firstReboot < 0 {
		t.Fatalf("probe index %d, reboot index %d; want both to have run", firstProbe, firstReboot)
	}
	if firstProbe > firstReboot {
		t.Error("the boot state baseline was taken after the reboot, want it before")
	}
}

func TestRunWatchesBeforeAnythingGoesDown(t *testing.T) {
	f := newFleet("10.0.0.5")
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	if _, err := orch.Run(context.Background(), newPlan(t, []string{"hv1,addr=10.0.0.5"})); err != nil {
		t.Fatal(err)
	}

	// A drop is only evidence if something was already watching when it
	// happened, so the first sample has to precede the first reboot command.
	firstPing, firstReboot := -1, -1
	for i, line := range runner.lines() {
		if firstPing < 0 && strings.HasPrefix(line, "ping ") {
			firstPing = i
		}
		if firstReboot < 0 && strings.Contains(line, "sudo reboot") {
			firstReboot = i
		}
	}
	if firstPing < 0 || firstReboot < 0 {
		t.Fatalf("expected both a ping and a reboot, got ping=%d reboot=%d", firstPing, firstReboot)
	}
	if firstPing > firstReboot {
		t.Errorf("first ping at %d came after the reboot at %d; the drop would go unobserved",
			firstPing, firstReboot)
	}
}

func TestRunConfirmsAMarkerlessHostFromTheObservedCycle(t *testing.T) {
	// A switch or appliance exposing neither boot_id nor uptime used to be
	// unverifiable in either direction. Watching it go down and come back is
	// the only evidence available, and it is enough.
	f := newFleet("10.0.0.9")
	f.markerless["10.0.0.9"] = true
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: f.runner(), Clock: newFakeClock(), Out: &out}
	result, err := orch.Run(context.Background(), newPlan(t, []string{"switch1,addr=10.0.0.9"}))
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Verifications) != 1 {
		t.Fatalf("got %d verifications, want 1", len(result.Verifications))
	}
	got := result.Verifications[0]
	if got.Status != StatusConfirmed {
		t.Errorf("Status = %v (%s), want the observed cycle to confirm the reboot", got.Status, got.Detail)
	}
	if !strings.Contains(got.Detail, "seen to go down") {
		t.Errorf("Detail = %q, want it to cite the observed cycle", got.Detail)
	}
	if !strings.Contains(out.String(), "[back] switch1 is back") {
		t.Errorf("output = %q, want the return logged", out.String())
	}
}

func TestRunReportsAHostThatNeverLeftTheNetwork(t *testing.T) {
	// The host accepts the reboot command, never goes down, and has no marker
	// to contradict itself with. Answering every probe is the proof.
	f := newFleet("10.0.0.9")
	f.markerless["10.0.0.9"] = true
	f.ignoresReboot["10.0.0.9"] = true
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: f.runner(), Clock: newFakeClock(), Out: &out}
	result, err := orch.Run(context.Background(), newPlan(t, []string{"switch1,addr=10.0.0.9"}))
	if err != nil {
		t.Fatal(err)
	}

	if got := result.NotRebooted(); len(got) != 1 {
		t.Fatalf("NotRebooted() = %v, want the host reported as never rebooted", got)
	}
	if !strings.Contains(out.String(), "[warn] switch1 answered every probe") {
		t.Errorf("output = %q, want the never-dropped warning", out.String())
	}
}
