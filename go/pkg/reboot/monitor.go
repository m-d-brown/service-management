package reboot

import (
	"context"
	"io"
	"slices"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

// A reboot is best proven by watching it happen. Boot markers settle the
// question afterwards, but only for hosts that expose one: an appliance or a
// switch with neither boot_id nor /proc/uptime leaves nothing to compare, and
// its reboot has until now been unverifiable. Seeing a host leave the network
// and come back is independent evidence, available on any host that answers at
// all.
//
// Catching that requires sampling to be running before the host is told to go
// down and to keep running while it does, which is why the monitor owns a
// goroutine rather than being a phase of the run. How briefly a host can be
// gone is the whole reason: a fast guest can be away and back inside what used
// to be a single blind fifteen-second sleep.

// monitorConcurrency bounds how many hosts the monitor probes at once, so a
// large tier does not open a connection per host on every sample.
const monitorConcurrency = 8

// Watchlist is what one tier asks the monitor to watch.
//
// All three groups are sampled identically. They differ only in what a host
// answering throughout is worth, which is precisely what the fleet's three
// relationships disagree about:
//
//   - A target was told to reboot, so never leaving the network is evidence it
//     did not.
//   - A carried host runs on a target, which is a claim that the target's
//     reboot restarts it. Never leaving the network while its parent went down
//     contradicts that claim, and the claim is the more likely thing to be
//     wrong: a guest that has been migrated elsewhere looks exactly like this.
//   - A dependent is ordered after a target and nothing more. It is watched
//     only in case it goes down, because its After edge never promised it
//     would, so a dependent that stays up says nothing at all.
type Watchlist struct {
	// Targets are the hosts this tier issues a reboot command to.
	Targets []Host
	// Carried are hosts hosted by a target, transitively, whose reboot the
	// target's own reboot delivers.
	Carried []Host
	// Dependents are hosts watched only because a target's reboot may take
	// them down.
	Dependents []Host
}

// watchRole is why a host is being watched, which is what its behaviour is
// worth as evidence. See Watchlist.
type watchRole int

const (
	// roleTarget was issued a reboot command.
	roleTarget watchRole = iota
	// roleCarried is hosted by a target and expected to go down with it.
	roleCarried
	// roleDependent is ordered after a target and nothing more.
	roleDependent
)

// Cycle is what the monitor saw of one host's power cycle.
//
// Watched and DropWaitElapsed exist so the zero value cannot be mistaken for
// evidence: a host nobody watched has not been seen to stay up, and neither has
// one whose drop wait had not run out when the run gave up on it.
type Cycle struct {
	// Host is the host that was watched.
	Host string
	// Watched reports that the monitor actually sampled this host.
	Watched bool
	// DropWaitElapsed reports that the drop wait ran out while watching, so a
	// host that never dropped had its full chance to. It is a fact about the
	// run rather than this host: one drop wait covers the whole tier.
	DropWaitElapsed bool
	// Dropped reports whether the host was seen to stop answering at all.
	Dropped bool
	// Returned reports whether it answered SSH again after dropping.
	Returned bool
	// DownAt is when it was first seen to be gone.
	DownAt time.Time
	// BackAt is when it first answered SSH again.
	BackAt time.Time
}

// Complete reports a whole power cycle: the host went away and came back. This
// is the observation that proves a reboot without reading any boot marker.
func (c Cycle) Complete() bool { return c.Watched && c.Dropped && c.Returned }

// StayedUp reports that the host answered every sample for the whole drop
// wait. Nothing was ever asked of it that it did not answer, so it cannot have
// restarted in that time.
func (c Cycle) StayedUp() bool { return c.Watched && c.DropWaitElapsed && !c.Dropped }

// DownFor is how long the host was unreachable, or zero if it never completed
// a cycle.
func (c Cycle) DownFor() time.Duration {
	if !c.Complete() {
		return 0
	}
	return c.BackAt.Sub(c.DownAt)
}

// watch is the running record for one host.
type watch struct {
	// host is what to probe.
	host Host
	// role is why this host is watched, and so what its never dropping means.
	role watchRole
	// up records whether the last sample was answered.
	up bool
	// dropped records that the host has been seen to stop answering.
	dropped bool
	// pingBack records that ping started answering again, which happens well
	// before SSH does and is worth reporting on its own.
	pingBack bool
	// returned records that SSH answered again after the drop.
	returned bool
	// warned records that the never-dropped warning has been printed, so it is
	// said once rather than on every remaining sample.
	warned bool
	// notReady records that this host's readiness command has been reported as
	// failing, which is likewise said once rather than every sample.
	notReady bool
	// downAt is when the host was first seen to be gone.
	downAt time.Time
	// backAt is when SSH first answered again.
	backAt time.Time
}

// Monitor watches hosts for the power cycle a reboot should produce.
//
// It samples with ICMP until a host stops answering, which is the cheapest way
// to catch the drop, and then confirms the return over SSH. The two are not
// interchangeable: the kernel answers ping as soon as its network stack is up,
// often half a minute before sshd will accept a connection, so a host declared
// back on ping alone would be handed to the next tier before it can be used.
//
// The drop wait is how long the monitor keeps watching for a host to stop
// answering, counted from StartDropWait — the moment the tier's commands are
// away. It bounds the waiting only: a drop is recorded whenever it is seen,
// early or late. What running out settles is the other direction, where there
// is nothing to see: a host that answered every probe for the whole drop wait
// is taken to have stayed up, and the run stops waiting for a drop that is not
// coming. One drop wait covers the tier, not one per host.
type Monitor struct {
	out         io.Writer
	runner      Runner
	clock       Clock
	interval    time.Duration
	pingTimeout time.Duration
	sshTimeout  time.Duration
	dropWait    time.Duration

	mu    sync.Mutex
	order []string
	seen  map[string]*watch
	// dropDeadline is when the drop wait runs out, after which a host that is
	// still answering stops being waited for. It is zero until StartDropWait,
	// which is not the same as elapsed.
	dropDeadline time.Time

	stop     chan struct{}
	stopOnce sync.Once
	exited   chan struct{}

	settled     chan struct{}
	settledOnce sync.Once
}

// StartMonitor begins watching hosts and returns once the first sample is in.
//
// The first sample is taken synchronously so the caller knows the baseline is
// recorded before it powers anything down; sampling then continues in the
// background until Stop. dropWait bounds how long a host that never stops
// answering is waited for before the run accepts that it is not going to drop —
// counted from StartDropWait, not from here.
func StartMonitor(
	ctx context.Context,
	out io.Writer,
	runner Runner,
	clock Clock,
	list Watchlist,
	interval, pingTimeout, sshTimeout, dropWait time.Duration,
) *Monitor {
	m := &Monitor{
		out:         out,
		runner:      runner,
		clock:       clock,
		interval:    interval,
		pingTimeout: pingTimeout,
		sshTimeout:  sshTimeout,
		dropWait:    dropWait,
		seen:        make(map[string]*watch, len(list.Targets)+len(list.Carried)+len(list.Dependents)),
		stop:        make(chan struct{}),
		exited:      make(chan struct{}),
		settled:     make(chan struct{}),
	}
	// Added strongest role first, so a host qualifying under several is watched
	// as the most that is known about it: a host told to reboot is a target
	// however else it is related, and a guest of this tier is carried even if
	// something also merely orders itself after it.
	m.add(list.Targets, roleTarget)
	m.add(list.Carried, roleCarried)
	m.add(list.Dependents, roleDependent)
	slices.Sort(m.order)

	if len(m.order) > 0 {
		report(out, "Watching %s for the reboot (sampling every %s)...\n",
			joinNames(m.hosts()), interval)
		for _, host := range m.hosts() {
			reportHost(out, host.Name, "$ %s", FormatCommand("ping", pingArgs(host.Target(), pingTimeout)...))
		}
	}

	m.sweep(ctx, clock.Now())
	go m.loop(ctx)
	return m
}

// add begins watching hosts, skipping any already watched. It runs before the
// sampler starts, so it takes no lock.
func (m *Monitor) add(hosts []Host, role watchRole) {
	for _, host := range hosts {
		if _, ok := m.seen[host.Name]; ok {
			continue
		}
		m.seen[host.Name] = &watch{host: host, role: role, up: true}
		m.order = append(m.order, host.Name)
	}
}

// loop samples until stopped or the context is cancelled.
func (m *Monitor) loop(ctx context.Context) {
	defer close(m.exited)
	ticks, stopTicker := m.clock.Ticker(m.interval)
	defer stopTicker()
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.stop:
			return
		case now := <-ticks:
			m.sweep(ctx, now)
		}
	}
}

// StartDropWait starts the clock on hosts that never leave the network.
//
// The drop wait cannot start when sampling does. Sampling deliberately starts
// first, before anything has been asked to go down, so that the drop is
// observed rather than inferred — but that leaves the tier's reboot commands
// still to be dispatched, behind a synchronous first sweep that probes every
// host it is watching. A drop wait started there runs out while the run is
// still issuing the very commands it is timing, which reads as every host
// having answered every probe — and, worse, lets the run conclude a tier has
// settled before a single host has gone anywhere.
func (m *Monitor) StartDropWait() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dropDeadline = m.clock.Now().Add(m.dropWait)
}

// dropWaitElapsed reports whether the drop wait has started and since run out.
// A drop wait that never started has not elapsed: nothing has been asked to
// reboot yet, so a host still answering says nothing. Callers hold mu.
func (m *Monitor) dropWaitElapsed(at time.Time) bool {
	return !m.dropDeadline.IsZero() && at.After(m.dropDeadline)
}

// Stop ends sampling and waits for the sampler to finish.
func (m *Monitor) Stop() {
	m.stopOnce.Do(func() { close(m.stop) })
	<-m.exited
}

// WaitForReturn blocks until every watched host has come back, or until the
// hosts that never dropped have been given the full drop wait.
func (m *Monitor) WaitForReturn(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-m.exited:
		// The sampler is gone, so nothing further will ever be observed.
		// Report whatever it managed to see rather than blocking forever.
		return nil
	case <-m.settled:
		return nil
	}
}

// Cycles returns what was observed for each host, keyed by host name.
func (m *Monitor) Cycles() map[string]Cycle {
	m.mu.Lock()
	defer m.mu.Unlock()
	elapsed := m.dropWaitElapsed(m.clock.Now())
	out := make(map[string]Cycle, len(m.seen))
	for name, w := range m.seen {
		out[name] = Cycle{
			Host:            name,
			Watched:         true,
			DropWaitElapsed: elapsed,
			Dropped:         w.dropped,
			Returned:        w.returned,
			DownAt:          w.downAt,
			BackAt:          w.backAt,
		}
	}
	return out
}

// hosts returns every watched host, in name order.
func (m *Monitor) hosts() []Host {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Host, 0, len(m.order))
	for _, name := range m.order {
		out = append(out, m.seen[name].host)
	}
	return out
}

// sweep takes one sample of every host still worth probing.
//
// The whole sweep is stamped with the instant it began rather than each probe
// reading the clock as it finishes, so a tier that went down together is
// recorded as having gone down together instead of smeared across however long
// the probes took.
//
// Probes run concurrently but their output is held back and printed in host
// order afterwards, so a run's transcript reads the same way twice and a tier
// of thirty hosts does not interleave its lines.
func (m *Monitor) sweep(ctx context.Context, at time.Time) {
	pending := m.pending()
	if len(pending) > 0 {
		lines := make([][]string, len(pending))
		group := new(errgroup.Group)
		group.SetLimit(monitorConcurrency)
		for i, w := range pending {
			group.Go(func() error {
				lines[i] = m.probe(ctx, w, at)
				return nil
			})
		}
		// probe folds every failure into the watch it updates, so there is no
		// error to collect: a host that cannot be reached is the signal.
		_ = group.Wait()
		for _, batch := range lines {
			for _, line := range batch {
				report(m.out, "%s", line)
			}
		}
	}

	for _, line := range m.warnNeverDropped(at) {
		report(m.out, "%s", line)
	}
	if m.allSettled(at) {
		m.settledOnce.Do(func() { close(m.settled) })
	}
}

// pending returns the watches still worth probing, in name order.
func (m *Monitor) pending() []*watch {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*watch
	for _, name := range m.order {
		if w := m.seen[name]; !w.returned {
			out = append(out, w)
		}
	}
	return out
}

// probe takes one sample of a single host and records what changed, returning
// any lines the sweep should print.
func (m *Monitor) probe(ctx context.Context, w *watch, at time.Time) []string {
	m.mu.Lock()
	host, dropped, pingBack := w.host, w.dropped, w.pingBack
	m.mu.Unlock()

	if !dropped {
		if PingHost(ctx, m.runner, host, m.pingTimeout) {
			m.mu.Lock()
			w.up = true
			m.mu.Unlock()
			return nil
		}
		// The first sample a host misses is the moment it left the network.
		// Nothing is retried here: a reboot is the expected cause, and a false
		// drop only means the host is reported back a sample later.
		m.mu.Lock()
		w.up, w.dropped, w.downAt = false, true, at
		m.mu.Unlock()
		return []string{hostLine(host.Name, "[down] stopped answering at %s", stamp(at))}
	}

	if !PingHost(ctx, m.runner, host, m.pingTimeout) {
		return nil
	}

	var lines []string
	if !pingBack {
		m.mu.Lock()
		w.pingBack = true
		m.mu.Unlock()
		lines = append(lines, hostLine(host.Name,
			"[ping] answers ping again at %s; waiting for %s", stamp(at), waitingFor(host)))
	}

	if err := m.checkReady(ctx, host); err != nil {
		if line := m.noteNotReady(w, host, err); line != "" {
			lines = append(lines, line)
		}
		return lines
	}

	m.mu.Lock()
	w.up, w.returned, w.backAt = true, true, at
	down := at.Sub(w.downAt)
	m.mu.Unlock()
	return append(lines,
		hostLine(host.Name, "[back] is back at %s, after %s down", stamp(at), formatUptime(down)))
}

// checkReady runs the host's readiness command over SSH and reports what went
// wrong, or nil once the host is usable.
//
// For a host that declared nothing this is true, and what is really being
// tested is that a login completed at all — already far more than answering
// ping, which the kernel does long before sshd will take a connection. A host
// that declared a readiness command is tested against that instead, because
// what the tier behind it is waiting for is the service, and a machine accepts
// logins well before it is serving anything.
//
// The error is returned rather than folded into a boolean so a command that
// will never pass can say why once, instead of being indistinguishable from a
// service that has not finished starting.
func (m *Monitor) checkReady(ctx context.Context, host Host) error {
	ctx, cancel := context.WithTimeout(ctx, m.sshTimeout)
	defer cancel()
	_, err := m.runner.Run(ctx, "", "ssh", sshCommand(host, host.ReadyCommand())...)
	return err
}

// waitingFor names what still has to happen before a host counts as back.
//
// The line an operator stares at during a long wait should say what is being
// waited on. With a readiness command that is the command itself: a wait that
// never ends is far more often a mistyped check than a machine that never came
// back, and the two read identically until the check is on screen.
func waitingFor(h Host) string {
	if h.Ready == "" {
		return "SSH"
	}
	return "readiness: " + h.Ready
}

// noteNotReady reports the first failure of a host's readiness command, once.
//
// A command that will never pass — a typo, a binary that is not installed —
// is otherwise indistinguishable from a service still starting up: both are
// silence. One line saying why the first attempt failed turns an unexplained
// wait into something an operator can act on. Later failures are the ordinary
// shape of waiting and are not worth repeating. Hosts with no readiness command
// say nothing here at all, because for them a failure means sshd is not up yet,
// which is the expected middle of every reboot.
func (m *Monitor) noteNotReady(w *watch, host Host, err error) string {
	if host.Ready == "" {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.notReady {
		return ""
	}
	w.notReady = true
	return hostLine(host.Name, "[wait] not ready yet: %v", err)
}

// warnNeverDropped reports each host whose staying up contradicts something,
// once per host.
//
// Two things can be contradicted, and they are different findings. A target was
// sent a reboot and never went anywhere, which is about that machine. A carried
// host stayed up while the machine it claims to run on went down, which is
// about the topology: the likeliest explanation is that the guest no longer
// lives there. Both are worth a word; a dependent staying up is not, because
// its After edge never said it would go down, and warning there flagged the
// ordinary case while burying the two lines that mean something.
//
// The lines state only what was seen, not what it means. VerifyReboot weighs
// the same observation against the host's own boot markers and draws the
// conclusion; this is the evidence it draws it from.
func (m *Monitor) warnNeverDropped(at time.Time) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var lines []string
	for _, name := range m.order {
		w := m.seen[name]
		if w.dropped || w.warned || !m.dropWaitElapsed(at) {
			continue
		}
		switch w.role {
		case roleTarget:
			w.warned = true
			lines = append(lines,
				hostLine(name, "[warn] answered every probe; it never left the network"))
		case roleCarried:
			// Only worth saying once the parent has actually gone down. A
			// hypervisor that ignored its own reboot explains a still-answering
			// guest completely, and it already has a line of its own.
			parent, watched := m.seen[w.host.RunsOn]
			if !watched || !parent.dropped {
				continue
			}
			w.warned = true
			lines = append(lines, hostLine(name,
				"[warn] answered every probe while %s went down; it may no longer run there",
				w.host.RunsOn))
		case roleDependent:
		}
	}
	return lines
}

// allSettled reports whether every host has finished as far as it ever will:
// back from a drop, or answering after the drop wait ran out without one.
func (m *Monitor) allSettled(at time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.seen {
		if w.returned {
			continue
		}
		if w.dropped || !w.up || !m.dropWaitElapsed(at) {
			return false
		}
	}
	return true
}

// stamp renders an instant the way the rest of a run's output reads.
func stamp(t time.Time) string { return t.Format("15:04:05") }
