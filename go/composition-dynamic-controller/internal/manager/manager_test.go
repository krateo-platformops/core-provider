package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func testProcess() Process {
	return Process{
		Config:                  &rest.Config{Host: "http://127.0.0.1:1"},
		Log:                     logging.NewNopLogger(),
		ProviderName:            "composition-dynamic-controller",
		ResyncInterval:          time.Minute,
		MinErrorRetryInterval:   time.Second,
		MaxErrorRetryInterval:   time.Minute,
		MaxErrorRetries:         5,
		GracefulShutdownTimeout: time.Second,
		Workers:                 1,
	}
}

var (
	fireworks = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"}
	portals   = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-3-0", Resource: "portals"}
)

// A process hosting nothing, or one Kind x version twice, is refused before anything runs: two
// members for one Kind x version would reconcile it twice in one process, which is what the
// ownership Lease prevents across processes.
func TestBuild_RefusesNoMemberAndADuplicate(t *testing.T) {
	if _, err := Build(context.Background(), testProcess(), nil); err == nil {
		t.Fatal("no member: want an error")
	}
	_, err := Build(context.Background(), testProcess(), []Member{{GVR: fireworks}, {GVR: fireworks}})
	if err == nil || !strings.Contains(err.Error(), "hosted twice") {
		t.Fatalf("a duplicate member: want 'hosted twice', got %v", err)
	}
}

// Two members build two controllers, one per Kind x version (control for the duplicate arm above).
func TestBuild_OneControllerPerMember(t *testing.T) {
	built, err := Build(context.Background(), testProcess(), []Member{{GVR: fireworks}, {GVR: portals}})
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 2 || built[0].Member.GVR != fireworks || built[1].Member.GVR != portals || built[0].Controller == built[1].Controller {
		t.Fatalf("want two distinct controllers for fireworksapps and portals, got %+v", built)
	}
}

// /readyz for the process is ready only when EVERY member is, and names the one that is not. Built
// controllers that have not run have not listed, so the first member is reported.
func TestReadiness_NamesTheMemberNotReady(t *testing.T) {
	built, err := Build(context.Background(), testProcess(), []Member{{GVR: fireworks}, {GVR: portals}})
	if err != nil {
		t.Fatal(err)
	}
	err = Readiness(built).ReadyzCheck(nil)
	if err == nil || !strings.Contains(err.Error(), fireworks.String()) {
		t.Fatalf("not-listed members: want an error naming %s, got %v", fireworks, err)
	}
}
