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
		// A guest is power-cycled by its hypervisor whether or not anyone
		// asked, which regenerates its boot id exactly as its own reboot would
		// and clears whatever it was waiting on a reboot to apply. That is the
		// free reboot a declared hosting relationship lets the run credit.
		f.downFor[guest] = rebootDownSamples
		f.reboots[guest]++
		f.bootIDs[guest] = fmt.Sprintf("boot-%s-%d", guest, f.reboots[guest])
		delete(f.pending, guest)
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
	if !strings.Contains(out.String(), "vm-a: skipping — no longer needs a reboot") {
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

func TestRunDoesNotWarnAboutDependentsItNeverRebooted(t *testing.T) {
	// vm-a reboots after hv1, so it is watched while hv1 goes down — but this
	// run targets hv1 alone, and hv1's reboot leaves vm-a running. Nothing was
	// asked of vm-a, so there is nothing to warn about.
	f := newFleet("10.0.0.5", "10.0.0.21")
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: f.runner(), Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,after=hv1",
	}, "hv1")

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.NotRebooted()) != 0 {
		t.Errorf("NotRebooted() = %v, want none", result.NotRebooted())
	}
	// It is watched — the run waits for it — but not judged by whether it moved.
	if !strings.Contains(out.String(), "Watching hv1, vm-a") {
		t.Errorf("output = %q, want the dependent watched alongside its parent", out.String())
	}
	if strings.Contains(out.String(), "[warn]") {
		t.Errorf("output = %q, want no warning about a dependent nobody rebooted", out.String())
	}
}

func TestRunLeadsEveryLineCarryingAnAddressWithItsHost(t *testing.T) {
	// Commands carry an address, because an address is what ssh and ping take.
	// Unless the host leads the line, a transcript is a column of IP addresses
	// the reader has to map back to the fleet by hand.
	f := newFleet("10.0.0.5", "10.0.0.21")
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: f.runner(), Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,after=hv1",
	})
	if _, err := orch.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	names := map[string]string{"10.0.0.5": "hv1", "10.0.0.21": "vm-a"}
	addressed := 0
	for _, line := range strings.Split(out.String(), "\n") {
		for addr, name := range names {
			if !strings.Contains(line, addr) {
				continue
			}
			addressed++
			// Indentation may still set the line under the step it belongs to,
			// but nothing except whitespace may come before the name.
			if !strings.HasPrefix(strings.TrimLeft(line, " "), name+": ") {
				t.Errorf("line %q carries %s without leading with %q", line, addr, name)
			}
		}
	}
	// Guard the guard: a run that echoed no addresses would pass vacuously.
	if addressed == 0 {
		t.Error("no line carried an address; the check proved nothing")
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
	if !strings.Contains(out.String(), "switch1: [back] is back") {
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
	if !strings.Contains(out.String(), "switch1: [warn] answered every probe") {
		t.Errorf("output = %q, want the never-dropped warning", out.String())
	}
}

func TestRunCreditsACarriedReboot(t *testing.T) {
	// The reason runs-on exists. The hypervisor's reboot power-cycles the guest,
	// so by the time the guest's tier comes up it restarted two minutes ago and
	// has the changed boot id to prove it. Rebooting it again applies nothing
	// and costs the fleet a second outage.
	f := newFleet("10.0.0.5", "10.0.0.21")
	f.guests["10.0.0.5"] = []string{"10.0.0.21"}
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,runs-on=hv1",
	})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	// One reboot command for two rebooted hosts.
	equalStrings(t, "reboot commands", rebootedDestinations(runner), []string{"10.0.0.5"})

	// The guest is still reported, and reported as confirmed: it did reboot,
	// and the run proved it rather than assuming it from the declaration.
	if len(result.Verifications) != 2 {
		t.Fatalf("got %d verifications, want both hosts accounted for", len(result.Verifications))
	}
	var guest *RebootVerification
	for i, v := range result.Verifications {
		if v.Host == "vm-a" {
			guest = &result.Verifications[i]
		}
	}
	if guest == nil {
		t.Fatal("vm-a is missing from the summary; a credited reboot is still a reboot")
	}
	if guest.Status != StatusConfirmed {
		t.Errorf("vm-a status = %v (%s), want confirmed", guest.Status, guest.Detail)
	}
	if !strings.Contains(guest.Detail, "rebooted with hv1") {
		t.Errorf("detail = %q, want the reboot attributed to its host", guest.Detail)
	}
	if len(result.NotRebooted()) != 0 {
		t.Errorf("NotRebooted() = %v, want none", result.NotRebooted())
	}

	for _, want := range []string{
		"Recording boot state of the hosts this tier will carry down",
		"Checking the hosts this tier carried down",
		"vm-a: skipping — already rebooted with hv1",
		"Tier 2 was rebooted by the tier hosting it",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output = %q, want it to contain %q", out.String(), want)
		}
	}
}

func TestRunRebootsAGuestThatWasNotActuallyCarried(t *testing.T) {
	// Nothing is taken on the strength of the declaration itself. A guest that
	// has been migrated away is still declared to run here, but its boot id
	// does not change when this hypervisor restarts, so it keeps its own tier
	// and is rebooted there — the outcome the run would have had if hosting had
	// never been declared at all.
	f := newFleet("10.0.0.5", "10.0.0.21")
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,runs-on=hv1",
	})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	equalStrings(t, "reboot commands", rebootedDestinations(runner),
		[]string{"10.0.0.5", "10.0.0.21"})
	if len(result.NotRebooted()) != 0 {
		t.Errorf("NotRebooted() = %v, want none: the guest was rebooted in its own tier",
			result.NotRebooted())
	}
	// Not being carried is not a failure — nothing was asked of the guest at
	// that point — so it is reported without a verdict glyph and left alone.
	if !strings.Contains(out.String(), "vm-a: not credited, so it keeps its own tier") {
		t.Errorf("output = %q, want the uncredited guest explained", out.String())
	}
	// And the contradiction is surfaced: the hosting is the likeliest mistake.
	if !strings.Contains(out.String(), "vm-a: [warn] answered every probe while hv1 went down") {
		t.Errorf("output = %q, want the stale hosting reported", out.String())
	}
}

func TestRunCreditsThroughAHostingChain(t *testing.T) {
	// Hosting is transitive: rebooting the hypervisor restarts the guest and
	// everything inside the guest, so one command settles all three.
	f := newFleet("10.0.0.5", "10.0.0.21", "10.0.0.31")
	f.guests["10.0.0.5"] = []string{"10.0.0.21", "10.0.0.31"}
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,runs-on=hv1",
		"ctr-1,addr=10.0.0.31,runs-on=vm-a",
	})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	equalStrings(t, "reboot commands", rebootedDestinations(runner), []string{"10.0.0.5"})
	if len(result.Verifications) != 3 {
		t.Fatalf("got %d verifications, want all three hosts accounted for", len(result.Verifications))
	}
	for _, v := range result.Verifications {
		if v.Status != StatusConfirmed {
			t.Errorf("%s: status = %v (%s), want confirmed", v.Host, v.Status, v.Detail)
		}
	}
}

func TestRunDoesNotCreditWithoutVerification(t *testing.T) {
	// A reboot is skipped because it demonstrably already happened, never
	// because the topology said it should have. With nothing read from the
	// hosts there is no proof, so the guest is rebooted in its own tier.
	f := newFleet("10.0.0.5", "10.0.0.21")
	f.guests["10.0.0.5"] = []string{"10.0.0.21"}
	runner := f.runner()

	cfg := testConfig()
	cfg.VerifyBootState = false
	orch := &Orchestrator{Config: cfg, Runner: runner, Clock: newFakeClock(), Out: &bytes.Buffer{}}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,runs-on=hv1",
	})

	if _, err := orch.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	equalStrings(t, "reboot commands", rebootedDestinations(runner),
		[]string{"10.0.0.5", "10.0.0.21"})
}

func TestRunLeavesUntargetedGuestsUnprobed(t *testing.T) {
	// A guest no later tier means to reboot is watched all the same — the tier
	// is not finished until it is back — but nothing is asked of it. On a
	// hypervisor with forty guests, the alternative is forty connections spent
	// on a question no one asked.
	f := newFleet("10.0.0.5", "10.0.0.21")
	f.guests["10.0.0.5"] = []string{"10.0.0.21"}
	runner := f.runner()

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &bytes.Buffer{}}
	plan := newPlan(t, []string{
		"hv1,addr=10.0.0.5",
		"vm-a,addr=10.0.0.21,runs-on=hv1",
	}, "hv1")

	if _, err := orch.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	if got := runner.countMatching("ping -c 1 -W 1 10.0.0.21"); got == 0 {
		t.Error("the guest was never pinged, want it waited for")
	}
	if got := runner.countMatching("boot_id"); got != 2 {
		t.Errorf("read boot state %d times, want only the targeted hypervisor's before and after", got)
	}
}

func TestRunSeparatesExcludedHosts(t *testing.T) {
	// Neither host is ordered before the other; they simply may not go down
	// together, which for an HA pair is the one outcome to avoid.
	f := newFleet("10.0.0.41", "10.0.0.42")
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"dns1,addr=10.0.0.41,not-with=dns2",
		"dns2,addr=10.0.0.42",
	})

	result, err := orch.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}

	// Both reboot, in separate tiers, so one is always serving.
	equalStrings(t, "reboot order", rebootedDestinations(runner),
		[]string{"10.0.0.41", "10.0.0.42"})
	if len(result.Verifications) != 2 {
		t.Fatalf("got %d verifications, want both hosts rebooted", len(result.Verifications))
	}
	for _, i := range []int{1, 2} {
		if !strings.Contains(out.String(), fmt.Sprintf("=== Executing Tier: %d ===", i)) {
			t.Errorf("output is missing tier %d; the pair shared a tier", i)
		}
	}
}

func TestRunWaitsForTheReadinessCommandBeforeTheNextTier(t *testing.T) {
	// What a host ordered after a provider is waiting for is the service, not
	// the login. The tier boundary is where that distinction is cashed in.
	f := newFleet("10.0.0.41", "10.0.0.30")
	runner := f.runner()
	var out bytes.Buffer

	orch := &Orchestrator{Config: testConfig(), Runner: runner, Clock: newFakeClock(), Out: &out}
	plan := newPlan(t, []string{
		"dns1,addr=10.0.0.41,ready=systemctl is-active named",
		"web1,addr=10.0.0.30,after=dns1",
	})

	if _, err := orch.Run(context.Background(), plan); err != nil {
		t.Fatal(err)
	}

	if got := runner.countMatching("systemctl is-active named"); got == 0 {
		t.Error("the readiness command was never run, want it gating the tier")
	}
	// The host that declared nothing is still checked with a bare login.
	if got := runner.countMatching("10.0.0.30 true"); got == 0 {
		t.Error("web1 was never probed with the default login test")
	}
}
