package reboot

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseBootProbe(t *testing.T) {
	at := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		output     string
		wantID     string
		wantUptime time.Duration
		wantHas    bool
	}{
		{
			name:       "both markers",
			output:     "boot_id=abc-123\nuptime=3600.55\n",
			wantID:     "abc-123",
			wantUptime: 3600550 * time.Millisecond,
			wantHas:    true,
		},
		{
			// A host without a boot_id still reports uptime; the probe emits
			// both keys and leaves the unreadable one empty.
			name:       "uptime only",
			output:     "boot_id=\nuptime=42.0\n",
			wantUptime: 42 * time.Second,
			wantHas:    true,
		},
		{
			name:   "boot id only",
			output: "boot_id=abc-123\nuptime=\n",
			wantID: "abc-123",
		},
		{
			name:   "neither marker",
			output: "boot_id=\nuptime=\n",
		},
		{
			name:   "unparseable uptime is discarded",
			output: "boot_id=abc-123\nuptime=not-a-number\n",
			wantID: "abc-123",
		},
		{
			name:   "unrelated output is ignored",
			output: "Welcome to the machine\nboot_id=abc-123\n",
			wantID: "abc-123",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := parseBootProbe(tt.output, at)
			if state.BootID != tt.wantID {
				t.Errorf("BootID = %q, want %q", state.BootID, tt.wantID)
			}
			if state.HasUptime != tt.wantHas {
				t.Errorf("HasUptime = %v, want %v", state.HasUptime, tt.wantHas)
			}
			if tt.wantHas && state.Uptime != tt.wantUptime {
				t.Errorf("Uptime = %v, want %v", state.Uptime, tt.wantUptime)
			}
		})
	}
}

func TestVerifyReboot(t *testing.T) {
	base := time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC)
	after := base.Add(2 * time.Minute)

	tests := []struct {
		name       string
		before     *BootState
		after      *BootState
		cycle      Cycle
		wantStatus VerificationStatus
		wantDetail string
	}{
		{
			name:       "boot id changed",
			before:     &BootState{BootID: "old", CapturedAt: base},
			after:      &BootState{BootID: "new", CapturedAt: after},
			wantStatus: StatusConfirmed,
			wantDetail: "boot_id changed (old -> new)",
		},
		{
			// The case the whole verification step exists for: the host
			// answered ping throughout and never restarted.
			name:       "boot id unchanged",
			before:     &BootState{BootID: "same", CapturedAt: base},
			after:      &BootState{BootID: "same", CapturedAt: after},
			wantStatus: StatusNotRebooted,
			wantDetail: "never went down",
		},
		{
			name:       "uptime reset",
			before:     &BootState{Uptime: 72 * time.Hour, HasUptime: true, CapturedAt: base},
			after:      &BootState{Uptime: 30 * time.Second, HasUptime: true, CapturedAt: after},
			wantStatus: StatusConfirmed,
			wantDetail: "uptime reset (3d -> 30s)",
		},
		{
			name:       "uptime kept climbing",
			before:     &BootState{Uptime: 72 * time.Hour, HasUptime: true, CapturedAt: base},
			after:      &BootState{Uptime: 72*time.Hour + 2*time.Minute, HasUptime: true, CapturedAt: after},
			wantStatus: StatusNotRebooted,
			wantDetail: "uptime kept climbing",
		},
		{
			// A host whose uptime is under the elapsed window rebooted even
			// though the raw number rose, which happens when the baseline was
			// taken moments after an earlier boot.
			name:       "uptime below elapsed window",
			before:     &BootState{Uptime: time.Second, HasUptime: true, CapturedAt: base},
			after:      &BootState{Uptime: 90 * time.Second, HasUptime: true, CapturedAt: after},
			wantStatus: StatusConfirmed,
			wantDetail: "uptime reset",
		},
		{
			name:       "boot id wins over uptime",
			before:     &BootState{BootID: "old", Uptime: time.Hour, HasUptime: true, CapturedAt: base},
			after:      &BootState{BootID: "new", Uptime: 2 * time.Hour, HasUptime: true, CapturedAt: after},
			wantStatus: StatusConfirmed,
			wantDetail: "boot_id changed",
		},
		{
			name:       "no baseline",
			before:     nil,
			after:      &BootState{BootID: "new", CapturedAt: after},
			wantStatus: StatusUnknown,
			wantDetail: "no baseline",
		},
		{
			name:       "unreadable afterwards",
			before:     &BootState{BootID: "old", CapturedAt: base},
			after:      nil,
			wantStatus: StatusUnknown,
			wantDetail: "could not be read afterwards",
		},
		{
			name:       "unreadable throughout",
			before:     nil,
			after:      nil,
			wantStatus: StatusUnknown,
			wantDetail: "before or after",
		},
		{
			name:       "no comparable marker",
			before:     &BootState{CapturedAt: base},
			after:      &BootState{CapturedAt: after},
			wantStatus: StatusUnknown,
			wantDetail: "no boot_id or uptime marker",
		},
		{
			// One side missing a boot_id falls through to uptime rather than
			// comparing an empty string against a real one.
			name:       "boot id appears only afterwards",
			before:     &BootState{Uptime: time.Hour, HasUptime: true, CapturedAt: base},
			after:      &BootState{BootID: "new", Uptime: time.Minute, HasUptime: true, CapturedAt: after},
			wantStatus: StatusConfirmed,
			wantDetail: "uptime reset",
		},
		{
			// The case this exists for: an appliance with no marker at all was
			// previously unverifiable in either direction.
			name:       "no marker but a cycle was observed",
			before:     &BootState{CapturedAt: base},
			after:      &BootState{CapturedAt: after},
			cycle:      watchedCycle(base.Add(4*time.Second), base.Add(47*time.Second)),
			wantStatus: StatusConfirmed,
			wantDetail: "seen to go down",
		},
		{
			name:   "no marker and the host never went down",
			before: &BootState{CapturedAt: base},
			after:  &BootState{CapturedAt: after},
			cycle: Cycle{
				Host: "web1", Watched: true, FullWindow: true,
			},
			wantStatus: StatusNotRebooted,
			wantDetail: "never went down",
		},
		{
			// Not watching a host is not the same as watching it stay up, and
			// must not be read as evidence that it failed to reboot.
			name:       "no marker and no observation stays unknown",
			before:     &BootState{CapturedAt: base},
			after:      &BootState{CapturedAt: after},
			wantStatus: StatusUnknown,
			wantDetail: "no boot_id or uptime marker",
		},
		{
			// The drop window had not elapsed, so answering every sample so far
			// proves nothing yet.
			name:   "never dropped but the window had not elapsed",
			before: &BootState{CapturedAt: base},
			after:  &BootState{CapturedAt: after},
			cycle: Cycle{
				Host: "web1", Watched: true, FullWindow: false,
			},
			wantStatus: StatusUnknown,
			wantDetail: "no boot_id or uptime marker",
		},
		{
			name:       "an observed cycle rescues an unreadable probe",
			before:     &BootState{BootID: "old", CapturedAt: base},
			after:      nil,
			cycle:      watchedCycle(base.Add(2*time.Second), base.Add(50*time.Second)),
			wantStatus: StatusConfirmed,
			wantDetail: "seen to go down",
		},
		{
			// Markers stay authoritative: a boot_id that never changed is not
			// overturned by anything the monitor thought it saw.
			name:       "markers outrank the observation",
			before:     &BootState{BootID: "same", CapturedAt: base},
			after:      &BootState{BootID: "same", CapturedAt: after},
			cycle:      watchedCycle(base.Add(4*time.Second), base.Add(47*time.Second)),
			wantStatus: StatusNotRebooted,
			wantDetail: "never went down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := VerifyReboot("web1", tt.before, tt.after, tt.cycle)
			if got.Host != "web1" {
				t.Errorf("Host = %q, want web1", got.Host)
			}
			if got.Status != tt.wantStatus {
				t.Errorf("Status = %v, want %v (detail: %s)", got.Status, tt.wantStatus, got.Detail)
			}
			if !strings.Contains(got.Detail, tt.wantDetail) {
				t.Errorf("Detail = %q, want it to contain %q", got.Detail, tt.wantDetail)
			}
		})
	}
}

// watchedCycle is a completed power cycle, as the monitor would report it.
func watchedCycle(down, back time.Time) Cycle {
	return Cycle{
		Host: "web1", Watched: true, FullWindow: true,
		Dropped: true, Returned: true, DownAt: down, BackAt: back,
	}
}

func TestFormatUptime(t *testing.T) {
	tests := []struct {
		duration time.Duration
		want     string
	}{
		{45 * time.Second, "45s"},
		{90 * time.Second, "1m"},
		{3 * time.Hour, "3h"},
		{72*time.Hour + 4*time.Hour + 12*time.Minute, "3d 4h 12m"},
		{0, "0s"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			if got := formatUptime(tt.duration); got != tt.want {
				t.Errorf("formatUptime(%v) = %q, want %q", tt.duration, got, tt.want)
			}
		})
	}
}

func TestCaptureBootState(t *testing.T) {
	runner := &fakeRunner{respond: func(call) (string, error) {
		return "boot_id=abc-123\nuptime=3600.0\n", nil
	}}
	clock := newFakeClock()
	var out bytes.Buffer

	host := Host{Name: "web1", Addr: "10.0.0.4", User: "ops"}
	state := CaptureBootState(context.Background(), &out, runner, clock, host, time.Second)

	if state == nil {
		t.Fatal("CaptureBootState() = nil, want a reading")
	}
	if state.BootID != "abc-123" {
		t.Errorf("BootID = %q, want abc-123", state.BootID)
	}
	if !state.CapturedAt.Equal(clock.Now()) {
		t.Errorf("CapturedAt = %v, want the clock's now %v", state.CapturedAt, clock.Now())
	}
	// The probe is announced as a copy-pasteable line before it runs.
	if !strings.Contains(out.String(), "$ ssh ") {
		t.Errorf("output = %q, want the ssh command echoed", out.String())
	}
	if got := runner.remotes(); len(got) != 1 || got[0] != bootProbeCommand {
		t.Errorf("remote commands = %v, want just the boot probe", got)
	}
}

func TestCaptureBootStateUnreachable(t *testing.T) {
	runner := &fakeRunner{respond: func(call) (string, error) {
		return "", errSSHFailed
	}}
	var out bytes.Buffer

	state := CaptureBootState(context.Background(), &out, runner, newFakeClock(),
		Host{Name: "web1"}, time.Second)

	// A host that cannot be probed is distinct from one that answered with no
	// markers: only the former means the reboot command will likely fail too.
	if state != nil {
		t.Errorf("CaptureBootState() = %+v, want nil for an unreachable host", state)
	}
	if !strings.Contains(out.String(), "probe for \"web1\" failed") {
		t.Errorf("output = %q, want a warning naming the host", out.String())
	}
}

func TestCaptureBootStateWarnsOnEmptyReading(t *testing.T) {
	runner := &fakeRunner{respond: func(call) (string, error) {
		return "boot_id=\nuptime=\n", nil
	}}
	var out bytes.Buffer

	state := CaptureBootState(context.Background(), &out, runner, newFakeClock(),
		Host{Name: "router1"}, time.Second)

	if state == nil {
		t.Fatal("CaptureBootState() = nil, want an empty reading rather than a failure")
	}
	if !strings.Contains(out.String(), "cannot be verified") {
		t.Errorf("output = %q, want a warning that verification is impossible", out.String())
	}
}
