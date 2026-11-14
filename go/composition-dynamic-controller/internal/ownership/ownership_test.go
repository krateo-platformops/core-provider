package ownership

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
)

func TestOption(t *testing.T) {
	if opt, err := Option("", "", ""); opt != nil || err != nil {
		t.Errorf("no lease name (core-provider before step 2): got option %v, error %v; want neither", opt != nil, err)
	}
	if opt, err := Option("v1-0-0.fireworksapps.composition.krateo.io", "krateo-system", "fireworksapps-v1-0-0-controller-abc"); opt == nil || err != nil {
		t.Errorf("lease configured: got option %v, error %v; want an option", opt != nil, err)
	}
	for _, tc := range []struct{ ns, pod string }{{"", "p"}, {"krateo-system", ""}} {
		opt, err := Option("v1-0-0.fireworksapps.composition.krateo.io", tc.ns, tc.pod)
		if opt != nil || err == nil || !strings.Contains(err.Error(), "refusing to reconcile without it") {
			t.Errorf("lease name with namespace %q, pod %q: got option %v, error %v; want a refusal", tc.ns, tc.pod, opt != nil, err)
		}
	}
}

func TestLost(t *testing.T) {
	if !Lost(fmt.Errorf("run: %w", controller.ErrOwnershipLost)) {
		t.Error("a wrapped ErrOwnershipLost must read as lost")
	}
	if Lost(errors.New("failed to wait for informers caches to sync")) || Lost(nil) {
		t.Error("another error, or none, must not read as lost")
	}
}
