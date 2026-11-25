package cdreason

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// releasedReasons records every reason a core-provider release carried, with the release that first
// carried it (core-provider#667). It is what a removal is checked against: the registry itself cannot
// remember a reason deleted from it, and CI checks out no tags.
const releasedReasons = "testdata/released-reasons.txt"

const releasedHeader = `# Every reason a core-provider release carried, one per line: condition/status/reason/writer, a tab,
# and the release that first carried it (the reason's Since). Read by TestReleasedReasonsAreStillListed.
# A line is never removed: a reason that leaves Table must be named in Deprecated, because consumers
# key on the reason string and a rename or removal is breaking for them.
# The release that ships an unreleased reason replaces its Since with the tag and appends it here:
# RELEASE_TAG=X.Y.Z UPDATE_READY_REASONS=1 go test ./internal/cdreason/ -run '^TestReasonTablesAreReleaseReady$'
# (docs/release.md). release-tag.yaml refuses a tag while any reason is unreleased or missing here.
`

// releaseTag is a release tag as release-tag.yaml triggers on it: plain X.Y.Z.
var releaseTag = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func reasonKey(condType string, status, reason, writer string) string {
	return condType + "/" + status + "/" + reason + "/" + writer
}

func readReleased(t *testing.T) (map[string]string, []string) {
	t.Helper()
	raw, err := os.ReadFile(releasedReasons)
	if err != nil {
		if os.IsNotExist(err) && os.Getenv("UPDATE_READY_REASONS") == "1" {
			return map[string]string{}, nil
		}
		t.Fatalf("%v (run with UPDATE_READY_REASONS=1 to create it)", err)
	}
	since := map[string]string{}
	var order []string
	for i, line := range strings.Split(string(raw), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "\t")
		if !ok || !releaseTag.MatchString(v) {
			t.Fatalf("%s:%d: want <key>\\t<release tag>, got %q", releasedReasons, i+1, line)
		}
		if _, dup := since[k]; dup {
			t.Fatalf("%s:%d: %s listed twice", releasedReasons, i+1, k)
		}
		since[k] = v
		order = append(order, k)
	}
	return since, order
}

// A reason a release carried is still in Table, with the same Since, or Deprecated names it with the
// release it was removed in. Every released reason in Table is recorded, and every Since is a release
// tag or Unreleased. CONTROL: the baseline must hold at least the rows the registry had when it began
// (52), or a pass would compare nothing.
func TestReleasedReasonsAreStillListed(t *testing.T) {
	released, order := readReleased(t)
	current := map[string]Reason{}
	for _, r := range Table() {
		current[reasonKey(string(r.Type()), string(r.Status()), r.String(), string(r.Writer()))] = r
	}
	if os.Getenv("UPDATE_READY_REASONS") == "1" {
		var added []string
		for k, r := range current {
			if _, ok := released[k]; !ok && r.Since() != Unreleased {
				added = append(added, k)
			}
		}
		sort.Strings(added)
		var b strings.Builder
		b.WriteString(releasedHeader)
		for _, k := range order {
			fmt.Fprintf(&b, "%s\t%s\n", k, released[k])
		}
		for _, k := range added {
			fmt.Fprintf(&b, "%s\t%s\n", k, current[k].Since())
			released[k] = current[k].Since()
		}
		if err := os.WriteFile(releasedReasons, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d released reasons recorded, %d in Table, %d deprecated", len(released), len(current), len(Deprecated()))
	if len(released) < 52 {
		t.Fatalf("control: only %d released reasons read from %s", len(released), releasedReasons)
	}
	deprecated := map[string]Removed{}
	for _, d := range Deprecated() {
		k := reasonKey(string(d.Condition), string(d.Status), string(d.Reason), string(d.Writer))
		deprecated[k] = d
		if _, ok := released[k]; !ok {
			t.Errorf("Deprecated names %s, which no release carried (%s)", k, releasedReasons)
		}
		if _, ok := current[k]; ok {
			t.Errorf("Deprecated names %s, which Table still lists", k)
		}
		if d.RemovedIn != Unreleased && !releaseTag.MatchString(d.RemovedIn) {
			t.Errorf("Deprecated %s: RemovedIn %q is neither a release tag nor %q", k, d.RemovedIn, Unreleased)
		}
	}
	for k, v := range released {
		r, listed := current[k]
		_, gone := deprecated[k]
		switch {
		case !listed && !gone:
			t.Errorf("%s was carried by %s and is no longer in Table: removing or renaming a reason is breaking for consumers; name it in Deprecated with the release it is removed in", k, v)
		case listed && r.Since() != v:
			t.Errorf("%s: Since is %q, and %s records that %s first carried it", k, r.Since(), releasedReasons, v)
		}
	}
	for k, r := range current {
		if r.Since() != Unreleased && !releaseTag.MatchString(r.Since()) {
			t.Errorf("%s: Since %q is neither a release tag nor %q", k, r.Since(), Unreleased)
		}
		if _, ok := released[k]; !ok && r.Since() != Unreleased {
			t.Errorf("%s: Since %s is a release, and %s does not record it: run UPDATE_READY_REASONS=1 go test ./internal/cdreason/", k, r.Since(), releasedReasons)
		}
	}
}

// registrySource is the registry file whose Since and RemovedIn literals the release step rewrites.
const registrySource = "cdreason.go"

// TestReasonTablesAreReleaseReady is the release-time gate (#667), run by release-tag.yaml with
// RELEASE_TAG set to the pushed tag, and skipped otherwise. A tag must not publish a reason table
// that says "unreleased", or that holds a reason testdata/released-reasons.txt does not record:
// every consumer reading the tag would see a reason with no release, and the next removal of it
// would pass the stability test unnoticed.
//
// With UPDATE_READY_REASONS=1 it is the release step itself: it rewrites every `Since: Unreleased`
// (and `RemovedIn: Unreleased`) in the registry to RELEASE_TAG and appends the newly released
// reasons to testdata/released-reasons.txt. The JSON is then regenerated by the drift test.
func TestReasonTablesAreReleaseReady(t *testing.T) {
	tag := os.Getenv("RELEASE_TAG")
	if tag == "" {
		t.Skip("RELEASE_TAG is not set: this gate runs at release time (release-tag.yaml)")
	}
	if !releaseTag.MatchString(tag) {
		t.Fatalf("RELEASE_TAG %q is not a release tag (X.Y.Z)", tag)
	}
	released, order := readReleased(t)
	var unreleased, unrecorded []string
	rows := map[string]Reason{}
	for _, r := range Table() {
		k := reasonKey(string(r.Type()), string(r.Status()), r.String(), string(r.Writer()))
		rows[k] = r
		_, recorded := released[k]
		switch {
		case r.Since() == Unreleased && recorded:
			t.Fatalf("%s is recorded as released by %s and its Since is %q: a released reason's Since never goes back", k, released[k], Unreleased)
		case r.Since() == Unreleased:
			unreleased = append(unreleased, k)
		case !recorded:
			unrecorded = append(unrecorded, k)
		}
	}
	for _, d := range Deprecated() {
		if d.RemovedIn == Unreleased {
			unreleased = append(unreleased, "deprecated "+reasonKey(string(d.Condition), string(d.Status), string(d.Reason), string(d.Writer)))
		}
	}
	t.Logf("RELEASE_TAG %s: %d reasons, %d unreleased (Since or RemovedIn), %d not recorded in %s", tag, len(rows), len(unreleased), len(unrecorded), releasedReasons)
	if len(unreleased)+len(unrecorded) == 0 {
		return
	}
	if os.Getenv("UPDATE_READY_REASONS") != "1" {
		t.Fatalf("not release-ready for %s:\n  unreleased: %v\n  not in %s: %v\n"+
			"Before tagging, on a branch from the commit to be tagged, run in go/core-provider:\n"+
			"  RELEASE_TAG=%s UPDATE_READY_REASONS=1 go test ./internal/cdreason/ -run '^TestReasonTablesAreReleaseReady$'\n"+
			"  UPDATE_READY_REASONS=1 go test ./internal/cdreason/ -run TestCDReadyReasonTableIsCurrent\n"+
			"then merge that commit and tag it (docs/release.md). If this ran on a pushed tag, nothing was published: release the next version the same way.",
			tag, unreleased, releasedReasons, unrecorded, tag)
	}
	text, rewrote := rewriteUnreleased(t, tag)
	if want := len(unreleased); rewrote != want {
		t.Fatalf("%s: found %d Since/RemovedIn fields set to Unreleased, want %d: write each one as `Since: Unreleased` (or `RemovedIn: Unreleased`), then rerun", registrySource, rewrote, want)
	}
	if err := os.WriteFile(registrySource, text, 0o644); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	b.WriteString(releasedHeader)
	for _, k := range order {
		fmt.Fprintf(&b, "%s\t%s\n", k, released[k])
	}
	var added []string
	for k, r := range rows {
		if _, ok := released[k]; ok {
			continue
		}
		since := r.Since()
		if since == Unreleased {
			since = tag
		}
		added = append(added, k+"\t"+since)
	}
	sort.Strings(added)
	for _, line := range added {
		b.WriteString(line + "\n")
	}
	if err := os.WriteFile(releasedReasons, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("rewrote %d literals in %s to %s and appended %d reasons to %s; now run `UPDATE_READY_REASONS=1 go test ./internal/cdreason/ -run TestCDReadyReasonTableIsCurrent` and commit", rewrote, registrySource, tag, len(added), releasedReasons)
}

// rewriteUnreleased is registrySource with every Since or RemovedIn field whose value is Unreleased
// (or the literal "unreleased") set to tag, found in the syntax tree so a comment naming the field
// is left alone, and how many it rewrote.
func rewriteUnreleased(t *testing.T, tag string) ([]byte, int) {
	t.Helper()
	src, err := os.ReadFile(registrySource)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, registrySource, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var spans [][2]int
	ast.Inspect(file, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		if k, ok := kv.Key.(*ast.Ident); !ok || (k.Name != "Since" && k.Name != "RemovedIn") {
			return true
		}
		switch v := kv.Value.(type) {
		case *ast.Ident:
			if v.Name != "Unreleased" {
				return true
			}
		case *ast.BasicLit:
			if u, err := strconv.Unquote(v.Value); err != nil || u != Unreleased {
				return true
			}
		default:
			return true
		}
		spans = append(spans, [2]int{fset.Position(kv.Value.Pos()).Offset, fset.Position(kv.Value.End()).Offset})
		return true
	})
	out := append([]byte(nil), src...)
	for i := len(spans) - 1; i >= 0; i-- {
		out = append(out[:spans[i][0]], append([]byte(strconv.Quote(tag)), out[spans[i][1]:]...)...)
	}
	return out, len(spans)
}
