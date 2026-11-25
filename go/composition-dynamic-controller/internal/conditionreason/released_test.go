package conditionreason

import (
	"fmt"
	"os"
	"regexp"
	"sort"
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
# The release that ships an unreleased reason replaces its Since and appends it here with
# UPDATE_READY_REASONS=1 go test ./internal/conditionreason/ (append only).
`

var releaseTag = regexp.MustCompile(`^2\.[0-9]+\.[0-9]+$`)

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
// (47), or a pass would compare nothing.
func TestReleasedReasonsAreStillListed(t *testing.T) {
	released, order := readReleased(t)
	current := map[string]Reason{}
	for _, r := range Table() {
		current[reasonKey(r.Type(), string(r.Status()), r.String(), string(r.Writer()))] = r
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
	if len(released) < 47 {
		t.Fatalf("control: only %d released reasons read from %s", len(released), releasedReasons)
	}
	deprecated := map[string]Removed{}
	for _, d := range Deprecated() {
		k := reasonKey(d.Condition, string(d.Status), d.Reason, string(d.Writer))
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
			t.Errorf("%s: Since %s is a release, and %s does not record it: run UPDATE_READY_REASONS=1 go test ./internal/conditionreason/", k, r.Since(), releasedReasons)
		}
	}
}
