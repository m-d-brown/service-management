package main

import (
	"errors"
	"strings"
	"testing"
)

// fakeRegistry serves canned tags/digests per repo so tests never touch a real
// registry. A non-nil err makes every lookup fail, simulating network or auth
// problems.
type fakeRegistry struct {
	tags    map[string][]string
	digests map[string]string
	err     error
}

func (f fakeRegistry) ListTags(repo string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tags[repo], nil
}

func (f fakeRegistry) LatestDigest(repo string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.digests[repo], nil
}

// TestParseVersion pins down which tag styles count as versions at all. This
// gate is the tool's main safety filter: anything rejected here can never be
// selected as an upgrade target, so the rejected rows matter as much as the
// accepted ones.
func TestParseVersion(t *testing.T) {
	tests := []struct {
		tag  string
		want []int
		ok   bool
	}{
		// Accepted shapes: plain semver, a "v" prefix, calver, date tags with
		// "-" separators, and a single bare number.
		{"2.11.3", []int{2, 11, 3}, true},
		{"v0.107.74", []int{0, 107, 74}, true},
		{"2026.5.4", []int{2026, 5, 4}, true},
		{"2026-05-22", []int{2026, 5, 22}, true},
		{"7", []int{7}, true},
		// Rejected: rolling/named tags and anything with a non-numeric
		// component. "1.2.3-rc1" and "13.0.1-ubuntu" are real-world styles for
		// prereleases and OS variants — selecting those would downgrade
		// stability or switch base images.
		{"latest", nil, false},
		{"stable", nil, false},
		{"1.2.3-rc1", nil, false},
		{"13.0.1-ubuntu", nil, false},
		{"v", nil, false},
		{"", nil, false},
		{"1..2", nil, false}, // empty component must not parse as zero
	}
	for _, tt := range tests {
		got, ok := parseVersion(tt.tag)
		if ok != tt.ok {
			t.Errorf("parseVersion(%q) ok = %v, want %v", tt.tag, ok, tt.ok)
			continue
		}
		if !ok {
			continue
		}
		if len(got.components) != len(tt.want) {
			t.Errorf("parseVersion(%q) = %v, want %v", tt.tag, got.components, tt.want)
			continue
		}
		for i := range tt.want {
			if got.components[i] != tt.want[i] {
				t.Errorf("parseVersion(%q) = %v, want %v", tt.tag, got.components, tt.want)
				break
			}
		}
	}
}

// TestVersionLess verifies component-wise numeric ordering. The 1.9 < 1.10
// case is the classic trap: lexicographic string comparison would invert it
// and the tool would forever consider 1.9.0 the "newest" release.
func TestVersionLess(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"1.2.3", "1.2.4", true},
		{"1.2.4", "1.2.3", false},
		{"1.2.3", "1.2.3", false},      // equal versions: not less in either direction
		{"1.9.0", "1.10.0", true},      // numeric, not lexicographic ("9" > "10" as strings)
		{"2.11", "2.11.1", true},       // missing components compare as zero
		{"2026.5.4", "2026.6.1", true}, // calver orders the same way
	}
	for _, tt := range tests {
		va, _ := parseVersion(tt.a)
		vb, _ := parseVersion(tt.b)
		if got := va.less(vb); got != tt.want {
			t.Errorf("less(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

// sampleManifest exercises the parser's boundaries in one document: header
// comments, tag and digest pins, an already-annotated line, a non-image
// scalar, and an indented image-like line *after* the images: block that must
// not be picked up.
const sampleManifest = `# Pinned container image versions.
#
# Comment lines are ignored.
images:
  nginx: docker.io/library/nginx:1.25.3
  postgres: docker.io/library/postgres:15.6  # newer major: 17.2
  statuspage: docker.io/example/statuspage@sha256:aaaabbbbcccc
  not_an_image: 42

other_top_level: ignored
  looks_like: docker.io/library/nope:1.0
`

// TestParseEntries verifies that exactly the image lines inside the images:
// block are found, that tag vs digest pins are split into the right fields,
// and that the block ends at the next top-level key (so unrelated YAML later
// in the file can never be rewritten).
func TestParseEntries(t *testing.T) {
	entries := parseEntries(strings.Split(sampleManifest, "\n"))

	// 3, not 4: "not_an_image: 42" has no "/" so it cannot be an image
	// reference; and "looks_like" sits below other_top_level, outside the
	// images: block.
	if len(entries) != 3 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.name)
		}
		t.Fatalf("got %d entries (%v), want 3", len(entries), names)
	}

	// Tag pin: reference splits into repo + tag, digest stays empty.
	nginx := entries[0]
	if nginx.name != "nginx" || nginx.repo != "docker.io/library/nginx" || nginx.tag != "1.25.3" || nginx.digest != "" {
		t.Errorf("nginx parsed as %+v", nginx)
	}

	// An existing "# newer major:" annotation (written by a previous run) must
	// be stripped during parsing. If it leaked into the tag, every run would
	// append another copy of the comment.
	postgres := entries[1]
	if postgres.tag != "15.6" {
		t.Errorf("postgres tag = %q, want 15.6 (trailing comment must be stripped)", postgres.tag)
	}

	// Digest pin: everything after "@" lands in digest, tag stays empty. This
	// is what routes the entry to LatestDigest instead of ListTags.
	statuspage := entries[2]
	if statuspage.digest != "sha256:aaaabbbbcccc" || statuspage.tag != "" {
		t.Errorf("statuspage parsed as %+v, want digest pin", statuspage)
	}
}

// TestResolve_WithinMajorBump is the happy path: among same-shape numeric
// tags of the current major, the highest wins; other majors' patches and
// suffixed variants (-alpine) are ignored.
func TestResolve_WithinMajorBump(t *testing.T) {
	reg := fakeRegistry{tags: map[string][]string{
		// 1.25.4 should win; "latest" and "-alpine" are unparseable, 1.24.9 is
		// an older major's patch line.
		"docker.io/library/nginx": {"latest", "1.25.3", "1.25.4", "1.24.9", "1.25.4-alpine"},
	}}
	e := entry{name: "nginx", repo: "docker.io/library/nginx", tag: "1.25.3"}

	r := resolve(reg, e)
	if r.newRef != "docker.io/library/nginx:1.25.4" {
		t.Errorf("newRef = %q, want :1.25.4", r.newRef)
	}
	if r.newerMajor != "" || r.note != "" {
		t.Errorf("unexpected newerMajor=%q note=%q", r.newerMajor, r.note)
	}
}

// TestResolve_NewerMajorAnnotatedNotApplied verifies the tool's core policy:
// upgrades stay within the current major (15.6 -> 15.8 here), and the newest
// other-major tag (17.2) is surfaced as an annotation only — a major jump must
// remain a deliberate hand-edit, never an automatic rewrite.
func TestResolve_NewerMajorAnnotatedNotApplied(t *testing.T) {
	reg := fakeRegistry{tags: map[string][]string{
		"docker.io/library/postgres": {"15.6", "15.8", "16.4", "17.2"},
	}}
	e := entry{name: "postgres", repo: "docker.io/library/postgres", tag: "15.6"}

	r := resolve(reg, e)
	if r.newRef != "docker.io/library/postgres:15.8" {
		t.Errorf("newRef = %q, want within-major :15.8", r.newRef)
	}
	if r.newerMajor != "17.2" {
		t.Errorf("newerMajor = %q, want 17.2", r.newerMajor)
	}
}

// TestResolve_ShapeMismatchRejected covers the failure mode that motivated
// shape-matching: registries often publish rolling tags ("1.25") and CI
// build-id tags ("1.25.4-9876543210", or a bare "9876543210"). All of those
// parse as numeric, so without comparing component counts against the current
// pin, a bare build number would look like a gigantic new major version.
func TestResolve_ShapeMismatchRejected(t *testing.T) {
	reg := fakeRegistry{tags: map[string][]string{
		"docker.io/library/nginx": {"1.25", "1.26", "1.25.4-9876543210", "9876543210", "1.25.4"},
	}}
	// Current pin has three components, so only three-component tags qualify.
	e := entry{name: "nginx", repo: "docker.io/library/nginx", tag: "1.25.3"}

	r := resolve(reg, e)
	if r.newRef != "docker.io/library/nginx:1.25.4" {
		t.Errorf("newRef = %q, want :1.25.4", r.newRef)
	}
	if r.newerMajor != "" {
		t.Errorf("newerMajor = %q, want none (build-id tags are not majors)", r.newerMajor)
	}
}

// TestResolve_NoCandidatesKeepsCurrent: when the registry offers no tag of the
// pin's shape at all, the safe outcome is a no-op — keep the current pin
// rather than guessing.
func TestResolve_NoCandidatesKeepsCurrent(t *testing.T) {
	reg := fakeRegistry{tags: map[string][]string{
		"docker.io/library/nginx": {"latest", "mainline", "1.25"},
	}}
	e := entry{name: "nginx", repo: "docker.io/library/nginx", tag: "1.25.3"}

	r := resolve(reg, e)
	if r.newRef != r.oldRef {
		t.Errorf("newRef = %q, want unchanged %q", r.newRef, r.oldRef)
	}
}

// TestResolve_DigestPinReresolved covers images that publish no usable version
// tag and are therefore pinned by digest: the update re-resolves what :latest
// currently points to and swaps the digest, keeping the pin reproducible.
func TestResolve_DigestPinReresolved(t *testing.T) {
	reg := fakeRegistry{digests: map[string]string{
		"docker.io/example/statuspage": "sha256:ddddeeeeffff",
	}}
	e := entry{name: "statuspage", repo: "docker.io/example/statuspage", digest: "sha256:aaaabbbbcccc"}

	r := resolve(reg, e)
	if r.newRef != "docker.io/example/statuspage@sha256:ddddeeeeffff" {
		t.Errorf("newRef = %q, want re-resolved digest", r.newRef)
	}
}

// TestResolve_RegistryErrorKeepsCurrentWithNote verifies fail-safe behavior
// for both pin styles: a registry failure must never alter the pin (the
// manifest stays deployable) and must surface the error in the NOTE column
// instead of aborting the whole run.
func TestResolve_RegistryErrorKeepsCurrentWithNote(t *testing.T) {
	reg := fakeRegistry{err: errors.New("connection refused")}
	for _, e := range []entry{
		{name: "nginx", repo: "docker.io/library/nginx", tag: "1.25.3"},
		{name: "statuspage", repo: "docker.io/example/statuspage", digest: "sha256:aaaabbbbcccc"},
	} {
		r := resolve(reg, e)
		if r.newRef != r.oldRef {
			t.Errorf("%s: newRef = %q, want unchanged on error", e.name, r.newRef)
		}
		if !strings.Contains(r.note, "connection refused") {
			t.Errorf("%s: note = %q, want the registry error", e.name, r.note)
		}
	}
}

// TestResolve_UnparseableCurrentTagNoted: a pin like ":mainline" gives the
// tool no major version to stay within, so it must keep the pin untouched and
// say why, leaving the fix to a human.
func TestResolve_UnparseableCurrentTagNoted(t *testing.T) {
	reg := fakeRegistry{tags: map[string][]string{"docker.io/library/nginx": {"1.25.4"}}}
	e := entry{name: "nginx", repo: "docker.io/library/nginx", tag: "mainline"}

	r := resolve(reg, e)
	if r.newRef != r.oldRef || r.note == "" {
		t.Errorf("got newRef=%q note=%q, want unchanged ref with note", r.newRef, r.note)
	}
}

// TestFormatLine checks the exact written-back syntax: indentation preserved,
// and the newer-major annotation appended as a trailing YAML comment only
// when one exists.
func TestFormatLine(t *testing.T) {
	e := entry{indent: "  ", name: "postgres"}

	plain := formatLine(e, result{newRef: "docker.io/library/postgres:15.8"})
	if plain != "  postgres: docker.io/library/postgres:15.8" {
		t.Errorf("plain line = %q", plain)
	}

	annotated := formatLine(e, result{newRef: "docker.io/library/postgres:15.8", newerMajor: "17.2"})
	if annotated != "  postgres: docker.io/library/postgres:15.8  # newer major: 17.2" {
		t.Errorf("annotated line = %q", annotated)
	}
}

// TestAnnotationRoundTripIsIdempotent runs parse -> resolve -> format twice
// over an already-annotated, already-current line. Both passes must reproduce
// the line byte-for-byte: re-running the tool on an up-to-date manifest should
// be a no-op, not stack "# newer major:" comments or churn the diff.
func TestAnnotationRoundTripIsIdempotent(t *testing.T) {
	reg := fakeRegistry{tags: map[string][]string{
		"docker.io/library/postgres": {"15.8", "17.2"},
	}}
	line := "  postgres: docker.io/library/postgres:15.8  # newer major: 17.2"

	for i := 0; i < 2; i++ {
		entries := parseEntries([]string{"images:", line})
		if len(entries) != 1 {
			t.Fatalf("pass %d: got %d entries, want 1", i, len(entries))
		}
		line = formatLine(entries[0], resolve(reg, entries[0]))
		if want := "  postgres: docker.io/library/postgres:15.8  # newer major: 17.2"; line != want {
			t.Fatalf("pass %d: line = %q, want %q", i, line, want)
		}
	}
}

// TestRenderTable checks that updated images are highlighted: an explicit
// "updated" status in the NOTE column (so the marker survives piped, colorless
// output), composed with the newer-major note when both apply, and — in color
// mode — the whole row wrapped in green ANSI codes outside the tabwriter
// layout so column alignment is unaffected.
func TestRenderTable(t *testing.T) {
	results := []result{
		{name: "nginx", oldRef: "docker.io/library/nginx:1.25.3", newRef: "docker.io/library/nginx:1.25.5"},
		{name: "postgres", oldRef: "docker.io/library/postgres:15.8", newRef: "docker.io/library/postgres:15.8"},
		{name: "redis", oldRef: "docker.io/library/redis:6.2.14", newRef: "docker.io/library/redis:6.2.16", newerMajor: "7.4.1"},
		{name: "broken", oldRef: "docker.io/example/broken:1.0.0", newRef: "docker.io/example/broken:1.0.0", note: "listing tags: boom"},
	}

	plain := renderTable(results, false)
	for _, want := range []string{
		"updated\n",
		"updated; newer major: 7.4.1",
		"! listing tags: boom",
	} {
		if !strings.Contains(plain, want) {
			t.Errorf("plain table missing %q:\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "\x1b[") {
		t.Errorf("plain table contains ANSI codes:\n%s", plain)
	}
	// The unchanged row gets no status: after the NEW column the line is blank.
	for _, ln := range strings.Split(plain, "\n") {
		if strings.HasPrefix(ln, "postgres") && !strings.HasSuffix(strings.TrimRight(ln, " "), ":15.8") {
			t.Errorf("unchanged row should have empty NOTE: %q", ln)
		}
	}

	colored := renderTable(results, true)
	for _, ln := range strings.Split(colored, "\n") {
		updated := strings.Contains(ln, "nginx") || strings.Contains(ln, "redis")
		if got := strings.HasPrefix(ln, ansiGreen) && strings.HasSuffix(ln, ansiReset); got != updated {
			t.Errorf("row colored=%v, want %v: %q", got, updated, ln)
		}
	}
}

// TestRefTail checks the table's compact display form: tags shown as-is,
// digests truncated to a recognizable prefix, and a bare repo (no tag or
// digest) passed through unchanged.
func TestRefTail(t *testing.T) {
	tests := []struct {
		ref  string
		want string
	}{
		{"docker.io/library/nginx:1.25.3", ":1.25.3"},
		{"docker.io/example/statuspage@sha256:0123456789abcdef0123", "@sha256:0123456789…"},
		{"docker.io/library/nginx", "docker.io/library/nginx"},
	}
	for _, tt := range tests {
		if got := refTail(tt.ref); got != tt.want {
			t.Errorf("refTail(%q) = %q, want %q", tt.ref, got, tt.want)
		}
	}
}
