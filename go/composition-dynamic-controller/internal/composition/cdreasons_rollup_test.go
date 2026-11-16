package composition

import (
	"os"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// cdReadyReasonsDoc is core-provider's rendered table of a CompositionDefinition's reasons, in the same
// monorepo (go/core-provider internal/cdreason).
const cdReadyReasonsDoc = "../../../../docs/cd-ready-reasons.md"

// A CompositionDefinition is a child this rollup reads: installer's umbrella composition renders most
// of a cluster's definitions (69 of 76 on krateo-057), and classifyChildEval sends every *.krateo.io
// child to krateoEval. core-provider declares, for each Ready reason a definition can carry, the
// parent verdict it expects; this test reads that file and fails when the rollup disagrees. That is
// what stops a new definition reason from falling to krateoEval's converging default and holding the
// umbrella at Creating for as long as it lasts.
//
// The file is read, not a Go value imported: the cdc does not depend on core-provider's module, and
// the file is what consumers vendor, so it is the thing to hold the rollup to. The rows are found by
// column NAME, as the consumers parse them, and their count is printed beside a floor, because a
// parser that matched no row would pass.
func TestCompositionDefinitionReasonsRollUpAsDeclared(t *testing.T) {
	raw, err := os.ReadFile(cdReadyReasonsDoc)
	if err != nil {
		t.Fatalf("reading %s: %v", cdReadyReasonsDoc, err)
	}
	var col map[string]int
	checked := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if col == nil {
			col = map[string]int{}
			for i, c := range cells {
				col[c] = i
			}
			for _, name := range []string{"condition", "reason", "status", "writer", "parent verdict"} {
				if _, ok := col[name]; !ok {
					t.Fatalf("%s: no %q column in the header %q", cdReadyReasonsDoc, name, line)
				}
			}
			continue
		}
		if strings.HasPrefix(cells[0], "---") || cells[col["condition"]] != "Ready" {
			continue
		}
		reason := strings.Trim(cells[col["reason"]], "`")
		status := cells[col["status"]]
		want := cells[col["parent verdict"]]
		child := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
			"status": map[string]any{"conditions": []any{map[string]any{
				"type": "Ready", "status": status, "reason": reason,
			}}},
		}}
		got := verdictWord(t, classifyChildEval("core.krateo.io", child).state)
		if got != want {
			t.Errorf("Ready=%s/%s (%s): core-provider declares parent verdict %q, this rollup reads %q",
				status, reason, cells[col["writer"]], want, got)
		}
		checked++
	}
	t.Logf("checked %d Ready rows of %s", checked, cdReadyReasonsDoc)
	if checked < 7 {
		t.Fatalf("only %d Ready rows parsed from %s: the parser is not reading the table", checked, cdReadyReasonsDoc)
	}
}

// verdictWord is the parent-verdict word for a state, as both tables print it.
func verdictWord(t *testing.T, s childState) string {
	t.Helper()
	switch s {
	case childHealthy:
		return "healthy"
	case childConverging:
		return "converging"
	case childFailed:
		return "failed"
	case childUnevaluated:
		return "unevaluated (rolls up as healthy)"
	}
	t.Fatalf("state %d has no verdict word", s)
	return ""
}
