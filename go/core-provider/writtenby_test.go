package main

import (
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
)

// core-provider#669: the stamp put on every CompositionDefinition status is this binary's build
// stamp, as -ldflags -X set it, and not a value written anywhere else. The sentinel values are ones
// no code path could produce, so a stamp built from anything but buildVersion/buildCommit fails.
func TestWrittenByIsTheBuildStamp(t *testing.T) {
	v, c := buildVersion, buildCommit
	t.Cleanup(func() { buildVersion, buildCommit = v, c })
	buildVersion, buildCommit = "9.9.9-sentinel-669", "c0ffee669"

	got := writtenBy()
	if got.Name != writtenby.Name || got.Version != "9.9.9-sentinel-669" || got.Commit != "c0ffee669" {
		t.Errorf("writtenBy() = %+v, want {%s 9.9.9-sentinel-669 c0ffee669}", got, writtenby.Name)
	}
}
