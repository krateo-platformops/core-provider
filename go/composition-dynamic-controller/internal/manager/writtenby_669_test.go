package manager

import (
	"testing"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/writtenby"
)

// core-provider#669: every member's handler is built with the process's build stamp, which main
// sets from the binary's -ldflags (TestWrittenByIsTheBuildStamp, package main).
func TestEveryMembersHandlerCarriesTheProcessStamp(t *testing.T) {
	stamp := writtenby.Stamp{Version: "2.13.135", Commit: "c0ffee135"}
	p := Process{WrittenBy: stamp}
	for _, m := range []Member{{}, {LeaseName: "pool-member"}} {
		if got := p.handlerOptions(m, nil, nil).WrittenBy; got != stamp {
			t.Errorf("member %+v: HandlerOptions.WrittenBy = %+v, want %+v", m, got, stamp)
		}
	}
}
