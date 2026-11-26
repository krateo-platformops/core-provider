package cdreason

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
)

// writtenByPair is one `name "<name>" (<what>): docs/<table>.json` in a notice.
var writtenByPair = regexp.MustCompile(`name "([^"]+)" \([^)]*\): docs/([a-z-]+\.json)`)

// TestWrittenByNamesMapToOneTableEach: core-provider#669's contract, read from the committed tables as a
// consumer reads them. A consumer takes status.writtenBy.name from an object and must land on exactly one
// table; both tables' notices state the same pairs, so it does not matter which one it read first.
//
// This module's writer constant (writtenby.Name, the value core-provider stamps on every definition) must
// be a name in that set and map to THIS table. The composition-dynamic-controller module has the same
// test for its own constant, against the same two files.
func TestWrittenByNamesMapToOneTableEach(t *testing.T) {
	const thisTable = "cd-ready-reasons.json"
	files := []string{"../../../../docs/ready-reasons.json", cdReadyReasonsJSON}

	var first []string
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var f struct {
			Table  string `json:"table"`
			Notice string `json:"notice"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		matches := writtenByPair.FindAllStringSubmatch(f.Notice, -1)
		// The denominator: a pattern that matched nothing would make every check below pass over an
		// empty set.
		t.Logf("%s (%s): %d name-to-table pairs", path, f.Table, len(matches))
		if len(matches) != 2 {
			t.Errorf("%s: the notice states %d name-to-table pairs for status.writtenBy, want 2 (one per binary)", path, len(matches))
			continue
		}
		byName, byTable := map[string][]string{}, map[string][]string{}
		var pairs []string
		for _, m := range matches {
			byName[m[1]] = append(byName[m[1]], m[2])
			byTable[m[2]] = append(byTable[m[2]], m[1])
			pairs = append(pairs, m[1]+" -> "+m[2])
		}
		sort.Strings(pairs)
		for name, tables := range byName {
			if len(tables) != 1 {
				t.Errorf("%s: name %q maps to %d tables %v, want exactly one", path, name, len(tables), tables)
			}
		}
		for table, names := range byTable {
			if len(names) != 1 {
				t.Errorf("%s: table %s is named by %d binaries %v, want exactly one", path, table, len(names), names)
			}
		}
		if got := byName[writtenby.Name]; len(got) != 1 || got[0] != thisTable {
			t.Errorf("%s: core-provider stamps status.writtenBy.name %q, which the notice maps to %v, want [%s]", path, writtenby.Name, got, thisTable)
		}
		if first == nil {
			first = pairs
		} else if strings.Join(first, "; ") != strings.Join(pairs, "; ") {
			t.Errorf("the two notices disagree: %s says %v, %s says %v", files[0], first, path, pairs)
		}
	}
}
