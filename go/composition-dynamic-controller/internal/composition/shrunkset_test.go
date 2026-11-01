package composition

import (
	"strings"
	"testing"
)

// THE CASE THE FIX EXISTS FOR (#230). The inventory lists fewer children than before the last
// render, so the message must say the denominator moved. 122 -> 108 is indistinguishable from
// 122 -> 122 in the condition, and a post-roll gate read five minutes into a bump reported 14 of 17
// components as DISAPPEARED when they were in the prune window.
func TestWithShrunkSet_SaysTheDenominatorMoved(t *testing.T) {
	got := withShrunkSet("Composition is up-to-date", 108, 122)
	if !strings.Contains(got, "108 of 122") {
		t.Errorf("the message must carry both counts, got %q", got)
	}
	if !strings.HasPrefix(got, "Composition is up-to-date") {
		t.Errorf("the caveat must APPEND, not replace — it is a statement about the set examined, "+
			"not a competing claim about health; got %q", got)
	}
}

// THE CONTROLS. Without these, a withShrunkSet that appended unconditionally would pass the test
// above while labelling every healthy composition as shrunken.
func TestWithShrunkSet_NotAppendedWhenTheSetDidNotShrink(t *testing.T) {
	for _, c := range []struct {
		name                 string
		total, previousTotal int
	}{
		{"unchanged", 122, 122},
		{"GREW — the #273 direction, not this one", 122, 108},
		{"never recorded before (Create)", 108, 0},
		{"both zero", 0, 0},
	} {
		base := "Composition is up-to-date"
		if got := withShrunkSet(base, c.total, c.previousTotal); got != base {
			t.Errorf("%s: message must be untouched, got %q", c.name, got)
		}
	}
}

// A composition mid-bump can honestly have BOTH caveats: the set shrank AND some of what remains
// could not be health-evaluated. resolveReady must compose them rather than let one hide the other.
func TestResolveReady_BothCaveatsSurviveTogether(t *testing.T) {
	v := healthVerdict{
		ready:         true,
		reason:        "Available",
		total:         108,
		previousTotal: 122,
		unevaluated:   7,
	}
	out := resolveReady(projectedHealth{}, "Composition is up-to-date", v)
	if out.reason != "Available" {
		t.Errorf("neither caveat may change the verdict, got reason %q", out.reason)
	}
	if !strings.Contains(out.message, "108 of 122") {
		t.Errorf("the shrink caveat is missing: %q", out.message)
	}
	if !strings.Contains(out.message, "7 of 108") {
		t.Errorf("the coverage caveat is missing: %q", out.message)
	}
}

// THE TRAP THE PRECEDENT WARNS ABOUT, and the reason this is stored in status rather than compared
// in-flight.
//
// Observe stamps the Ready message on EVERY reconcile and passes recordManaged: false, so it has no
// `previous` of its own. A caveat composed only where the shrink is noticed would survive one
// reconcile and then be overwritten — which is exactly how #121's coverage caveat shipped
// unreachable, computed every time and discarded every time.
//
// This asserts the property that makes the stored field necessary: given only what Observe has (the
// current total and the STORED previous count), the caveat is still produced. If someone later
// "simplifies" this by comparing in-flight inside the recording branch, this test fails.
func TestWithShrunkSet_DerivableFromStoredStateAlone(t *testing.T) {
	// Exactly what an Observe has in hand: no access to the prior inventory, only the two numbers.
	const observeTotal, storedPrevious = 108, 122

	got := withShrunkSet("Composition is up-to-date", observeTotal, storedPrevious)
	if !strings.Contains(got, "108 of 122") {
		t.Fatalf("the caveat must be derivable from the stored count alone, or it cannot survive "+
			"the next Observe (#121's trap); got %q", got)
	}
}

// Once a render agrees with the one before it the caveat must clear on its own, with no reset. That
// is the whole reason this is a stored previous-count rather than a high-water mark: a chart that
// legitimately renders fewer children forever must converge, not carry the caveat indefinitely.
func TestWithShrunkSet_ClearsWithoutAReset(t *testing.T) {
	base := "Composition is up-to-date"

	// Bump in flight: 122 -> 108, caveat present.
	if withShrunkSet(base, 108, 122) == base {
		t.Fatal("the caveat should be present while the set is smaller")
	}
	// The next Update renders 108 again, so previousManagedCount becomes 108.
	if got := withShrunkSet(base, 108, 108); got != base {
		t.Errorf("a chart that settles at the smaller size must stop carrying the caveat, got %q", got)
	}
}
