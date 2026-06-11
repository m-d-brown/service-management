// Command update-container-manifest bumps pinned container image versions in an
// Ansible-style manifest (a flat `images:` map of name -> full image reference,
// e.g. an ansible/images.yml loaded via vars_files) to the newest stable
// version within each image's current major version.
//
// If a newer major exists it is left untouched but annotated with a
// `# newer major: X.Y.Z` comment so the jump stays a deliberate hand-edit.
// Images pinned by digest (those whose registry publishes no usable version
// tag) have their :latest manifest digest re-resolved instead.
//
// "Stable" = a tag whose components are all numeric (optionally a leading "v"),
// separated by "." or "-", and whose component count matches the current pin.
// That admits semver (2.11.3), calver (2026.5.4) and date tags (2026-05-22)
// while rejecting latest/edge/rc/beta/-alpine variants and registry build-id
// tags (e.g. a CI build-id suffix like 1.2.3-4567890123).
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/google/go-containerregistry/pkg/crane"
)

type entry struct {
	indent  string // leading whitespace
	name    string // map key, e.g. "postgres"
	repo    string // e.g. "docker.io/postgres" (verbatim from the file)
	tag     string // "" when digest-pinned
	digest  string // "" when tag-pinned (includes the "sha256:" prefix)
	lineIdx int    // index into the file's line slice
}

type result struct {
	name       string
	oldRef     string
	newRef     string
	newerMajor string // tag of a newer major, if any
	note       string // error/skip note
}

// registry abstracts the container registry lookups, enabling mocking in tests.
type registry interface {
	ListTags(repo string) ([]string, error)
	LatestDigest(repo string) (string, error)
}

// craneRegistry is the real implementation using go-containerregistry.
type craneRegistry struct{}

func (craneRegistry) ListTags(repo string) ([]string, error) { return crane.ListTags(repo) }
func (craneRegistry) LatestDigest(repo string) (string, error) {
	return crane.Digest(repo + ":latest")
}

func main() {
	file := flag.String("file", "ansible/images.yml", "path to the images manifest")
	dryRun := flag.Bool("dry-run", false, "print proposed changes without writing")
	flag.Parse()

	lines, err := readLines(*file)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	entries := parseEntries(lines)
	if len(entries) == 0 {
		fmt.Fprintln(os.Stderr, "no image entries found under `images:` in", *file)
		os.Exit(1)
	}

	var results []result
	changed := 0
	for _, e := range entries {
		r := resolve(craneRegistry{}, e)
		results = append(results, r)
		if r.note == "" && r.newRef != r.oldRef {
			changed++
		}
		lines[e.lineIdx] = formatLine(e, r)
	}

	printTable(results)

	if *dryRun {
		fmt.Printf("\n(dry run) %d image(s) would change. No files written.\n", changed)
		return
	}
	if err := writeLines(*file, lines); err != nil {
		fmt.Fprintln(os.Stderr, "error writing:", err)
		os.Exit(1)
	}
	fmt.Printf("\nWrote %s — %d image(s) changed.\n", *file, changed)
}

var entryRe = regexp.MustCompile(`^(\s+)([A-Za-z0-9_]+):\s+(\S+)(?:\s+#.*)?\s*$`)

// parseEntries finds `key: ref` lines in the `images:` block.
func parseEntries(lines []string) []entry {
	var out []entry
	inImages := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, "images:") {
			inImages = true
			continue
		}
		if !inImages {
			continue
		}
		// A non-indented, non-blank line ends the block.
		if strings.TrimSpace(ln) != "" && !strings.HasPrefix(ln, " ") {
			break
		}
		m := entryRe.FindStringSubmatch(ln)
		if m == nil {
			continue
		}
		ref := m[3]
		if !strings.Contains(ref, "/") { // not an image reference
			continue
		}
		e := entry{indent: m[1], name: m[2], lineIdx: i}
		if at := strings.Index(ref, "@"); at >= 0 {
			e.repo, e.digest = ref[:at], ref[at+1:]
		} else if c := strings.LastIndex(ref, ":"); c > strings.LastIndex(ref, "/") {
			e.repo, e.tag = ref[:c], ref[c+1:]
		} else {
			e.repo = ref
		}
		out = append(out, e)
	}
	return out
}

func resolve(reg registry, e entry) result {
	r := result{name: e.name, oldRef: currentRef(e)}
	if e.digest != "" {
		dig, err := reg.LatestDigest(e.repo)
		if err != nil {
			r.newRef, r.note = r.oldRef, oneLine(err.Error())
			return r
		}
		r.newRef = e.repo + "@" + dig
		return r
	}

	tags, err := reg.ListTags(e.repo)
	if err != nil {
		r.newRef, r.note = r.oldRef, oneLine(err.Error())
		return r
	}
	cur, ok := parseVersion(e.tag)
	if !ok {
		r.newRef, r.note = r.oldRef, "current tag not a parseable version"
		return r
	}
	// Shape-match: only consider tags with the same number of components as the
	// current pin. This rejects rolling tags (2.11 vs 2.11.4) and registry
	// build-id tags that would otherwise masquerade as huge versions / new majors.
	curLen := len(cur.components)
	var bestSame, bestOverall string
	var vSame, vOverall version
	for _, t := range tags {
		v, ok := parseVersion(t)
		if !ok || len(v.components) != curLen {
			continue
		}
		if bestOverall == "" || vOverall.less(v) {
			bestOverall, vOverall = t, v
		}
		if v.components[0] == cur.components[0] && (bestSame == "" || vSame.less(v)) {
			bestSame, vSame = t, v
		}
	}
	if bestSame == "" {
		bestSame = e.tag // nothing found; keep current
	}
	r.newRef = e.repo + ":" + bestSame
	if bestOverall != "" && vOverall.components[0] > cur.components[0] {
		r.newerMajor = bestOverall
	}
	return r
}

func currentRef(e entry) string {
	switch {
	case e.digest != "":
		return e.repo + "@" + e.digest
	case e.tag != "":
		return e.repo + ":" + e.tag
	default:
		return e.repo
	}
}

// version is an ordered list of numeric components (major first).
type version struct{ components []int }

func (a version) less(b version) bool {
	for i := 0; i < len(a.components) || i < len(b.components); i++ {
		var x, y int
		if i < len(a.components) {
			x = a.components[i]
		}
		if i < len(b.components) {
			y = b.components[i]
		}
		if x != y {
			return x < y
		}
	}
	return false
}

var sepRe = regexp.MustCompile(`[.\-]`)

// parseVersion accepts tags whose components are all numeric (optional leading
// "v"), e.g. 2.11.3, v0.107.74, 2026.5.4, 2026-05-22. Anything else (latest,
// 1.2.3-rc1, 13.0.1-ubuntu, edge, ...) is rejected.
func parseVersion(tag string) (version, bool) {
	t := strings.TrimPrefix(tag, "v")
	if t == "" {
		return version{}, false
	}
	parts := sepRe.Split(t, -1)
	v := version{}
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return version{}, false
		}
		v.components = append(v.components, n)
	}
	return v, true
}

func formatLine(e entry, r result) string {
	line := fmt.Sprintf("%s%s: %s", e.indent, e.name, r.newRef)
	if r.newerMajor != "" {
		line += "  # newer major: " + r.newerMajor
	}
	return line
}

func printTable(results []result) {
	// Render via tabwriter into a builder (whose Write never fails), then print.
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "IMAGE\tCURRENT\tNEW\tNOTE")
	for _, r := range results {
		status := ""
		switch {
		case r.note != "":
			status = "! " + r.note
		case r.newerMajor != "":
			status = "newer major: " + r.newerMajor
		}
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.name, refTail(r.oldRef), refTail(r.newRef), status)
	}
	_ = w.Flush()
	fmt.Print(sb.String())
}

// refTail trims the registry/repo prefix for compact display (the tag/digest).
func refTail(ref string) string {
	if i := strings.Index(ref, "@"); i >= 0 {
		d := ref[i+1:] // e.g. sha256:0a21e0...
		if len(d) > 17 {
			d = d[:17] + "…"
		}
		return "@" + d
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		return ":" + ref[i+1:]
	}
	return ref
}

func readLines(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only; close error is unactionable
	var lines []string
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 0, 1024*1024), 1024*1024)
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	return lines, s.Err()
}

func writeLines(path string, lines []string) error {
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

func oneLine(s string) string {
	return strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
}
