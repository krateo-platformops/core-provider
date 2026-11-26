package ownership

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
)

func TestOption(t *testing.T) {
	valid := Timings{LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}
	if opt, err := Option("", "", "", valid); opt != nil || err != nil {
		t.Errorf("no lease name (core-provider before step 2): got options %v, error %v; want neither", opt != nil, err)
	}
	if opt, err := Option("v1-0-0.fireworksapps.composition.krateo.io", "krateo-system", "fireworksapps-v1-0-0-controller-abc", valid); len(opt) != 2 || err != nil {
		t.Errorf("lease configured: got %d options, error %v; want the Lease and its timings", len(opt), err)
	}
	for _, tc := range []struct{ ns, pod string }{{"", "p"}, {"krateo-system", ""}} {
		opt, err := Option("v1-0-0.fireworksapps.composition.krateo.io", tc.ns, tc.pod, valid)
		if opt != nil || err == nil || !strings.Contains(err.Error(), "refusing to reconcile without it") {
			t.Errorf("lease name with namespace %q, pod %q: got option %v, error %v; want a refusal", tc.ns, tc.pod, opt != nil, err)
		}
	}
	// #660: no timings is refused, never unstructured-runtime's 15s/10s/2s, which core-provider does not
	// wait by; and an invalid set is refused naming the three.
	for name, tm := range map[string]Timings{
		"zero":                     {},
		"renewDeadline = duration": {LeaseDuration: 20 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second},
	} {
		opt, err := Option("v1-0-0.fireworksapps.composition.krateo.io", "krateo-system", "pod", tm)
		if opt != nil || err == nil || !strings.Contains(err.Error(), "invalid ownership lease timings") {
			t.Errorf("%s timings: got options %v, error %v; want a refusal", name, opt != nil, err)
		}
	}
}

// The env the chart renders (helm/core-provider: cdc.ownershipLease -> core-provider.cdcProcessEnvTail),
// read the way main reads it. Every name and value is a literal: these are the chart's keys, which
// core-provider's TestOwnershipLeaseTimingsRenderOnceForBothBinaries holds the chart to.
func TestTimingsFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	full := map[string]string{
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION":       "41s",
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "27s",
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD":   "7s",
	}
	got, err := TimingsFromEnv(env(full))
	if err != nil || got != (Timings{LeaseDuration: 41 * time.Second, RenewDeadline: 27 * time.Second, RetryPeriod: 7 * time.Second}) {
		t.Fatalf("the three set: got %+v, %v; want 41s/27s/7s", got, err)
	}
	// None set (a hand-run binary): the chart's default, 30s/20s/5s, not unstructured-runtime's.
	got, err = TimingsFromEnv(env(nil))
	if err != nil || got != (Timings{LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}) {
		t.Fatalf("none set: got %+v, %v; want 30s/20s/5s", got, err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		says []string
	}{
		{"renewDeadline = duration", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "5s",
		}, []string{"leaseDuration must be greater than renewDeadline", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION=20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE=20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD=5s"}},
		{"renewDeadline above duration", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "25s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "5s",
		}, []string{"leaseDuration must be greater than renewDeadline"}},
		{"retryPeriod x 1.2 = renewDeadline", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "30s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "6s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "5s",
		}, []string{"renewDeadline must be greater than retryPeriod x 1.2"}},
		{"zero", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "30s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "0s",
		}, []string{"greater than zero"}},
		{"not a duration", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "30", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "5s",
		}, []string{`COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION="30" is not a Go duration`}},
		{"part of the set", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "60s",
		}, []string{"are a set", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE, COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD unset"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := TimingsFromEnv(env(c.env))
			if err == nil {
				t.Fatalf("accepted as %+v", got)
			}
			for _, s := range c.says {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not say %q", err, s)
				}
			}
		})
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
