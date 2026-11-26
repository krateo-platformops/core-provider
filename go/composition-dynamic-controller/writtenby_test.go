package main

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/writtenby"
)

// core-provider#669: the stamp put on every composition status is this binary's build stamp, as
// -ldflags -X set it, and not a value written anywhere else. The sentinel values are ones no code
// path could produce, so a stamp built from anything but buildVersion/buildCommit fails.
func TestWrittenByIsTheBuildStamp(t *testing.T) {
	v, c := buildVersion, buildCommit
	t.Cleanup(func() { buildVersion, buildCommit = v, c })
	buildVersion, buildCommit = "9.9.9-sentinel-669", "c0ffee669"

	if got, want := writtenBy(), (writtenby.Stamp{Version: "9.9.9-sentinel-669", Commit: "c0ffee669"}); got != want {
		t.Errorf("writtenBy() = %+v, want %+v", got, want)
	}
}
