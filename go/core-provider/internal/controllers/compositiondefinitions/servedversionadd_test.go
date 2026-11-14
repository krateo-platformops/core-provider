package compositiondefinitions

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestVacatedBeforeAddExcludesOnlyTheDefinitionsOwnCurrentVersion(t *testing.T) {
	for _, tc := range []struct {
		prunable []string
		own      string
		want     string
	}{
		{[]string{"v0-3-634"}, "v0-3-633", "[v0-3-634]"},             // 057: own current not eligible anyway
		{[]string{"v0-3-633", "v0-3-634"}, "v0-3-633", "[v0-3-634]"}, // own current eligible (no instances): excluded
		{[]string{"v1"}, "", "[v1]"},                                 // no status yet: nothing excluded
		{nil, "v1", "[]"},
	} {
		if got := fmt.Sprint(vacatedBeforeAdd(tc.prunable, tc.own)); got != tc.want {
			t.Errorf("vacatedBeforeAdd(%v, %q) = %s, want %s", tc.prunable, tc.own, got, tc.want)
		}
	}
}

func TestServedListVersionPrefersTheDefinitionsOwnServedVersion(t *testing.T) {
	crd := func(vs ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
		return &apiextensionsv1.CustomResourceDefinition{Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: vs}}
	}
	v := func(name string, served bool) apiextensionsv1.CustomResourceDefinitionVersion {
		return apiextensionsv1.CustomResourceDefinitionVersion{Name: name, Served: served}
	}
	for _, tc := range []struct {
		name string
		crd  *apiextensionsv1.CustomResourceDefinition
		own  string
		want string
	}{
		{"own served", crd(v("vacuum", false), v("a", true), v("b", true)), "b", "b"},
		{"own not served: first served", crd(v("vacuum", false), v("a", true), v("b", false)), "b", "a"},
		{"vacuum is never listed through", crd(v("vacuum", true), v("a", true)), "", "a"},
		{"nothing served", crd(v("vacuum", false)), "a", ""},
	} {
		if got := servedListVersion(tc.crd, tc.own); got != tc.want {
			t.Errorf("%s: servedListVersion = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestServedVersionAddWaitsSaysWhatItWaitsForAndUntilWhen(t *testing.T) {
	due := time.Date(2026, 11, 13, 21, 30, 0, 0, time.UTC)
	w := &servedVersionAddWaits{
		gvr:     schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-3-635", Resource: "installers"},
		waiting: []string{"v0-3-634"}, dueAt: due,
	}
	msg := w.Error()
	for _, want := range []string{"v0-3-635", "installers", "v0-3-634", servedVersionPruneGrace.String(), "2026-11-13T21:30:00Z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not name %q", msg, want)
		}
	}
}

// Update and Create end through settleAddWait: the wait is a success that stops the operation, and
// every other error passes through unchanged. Wrapped, because the wait reaches Update through
// deployController.
func TestSettleAddWait(t *testing.T) {
	log := logging.NewNopLogger()
	wait := &servedVersionAddWaits{waiting: []string{"v1"}}
	boom := errors.New("boom")
	for _, tc := range []struct {
		name     string
		in       error
		wantStop bool
		wantErr  error
	}{
		{"the wait", wait, true, nil},
		{"the wait, wrapped", fmt.Errorf("deploying: %w", wait), true, nil},
		{"a real failure", boom, false, boom},
		{"no error", nil, false, nil},
	} {
		stop, err := settleAddWait(log, tc.in)
		if stop != tc.wantStop || !errors.Is(err, tc.wantErr) || (tc.wantErr == nil && err != nil) {
			t.Errorf("%s: settleAddWait = (%v, %v), want (%v, %v)", tc.name, stop, err, tc.wantStop, tc.wantErr)
		}
	}
}
