package deploy

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// #660: the default is 30s/20s/5s, and what core-provider derives from it. Every
// expected value is a literal.
func TestDefaultOwnershipLeaseTimings(t *testing.T) {
	d := DefaultOwnershipLeaseTimings()
	if d != (OwnershipLeaseTimings{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}) {
		t.Fatalf("default = %s, want 30s/20s/5s", d)
	}
	if q := d.Quiet(); q != 12500*time.Millisecond {
		t.Errorf("Quiet = %s, want 12.5s (2.5 x 5s)", q)
	}
	if g := d.MemberGrace(); g != 42500*time.Millisecond {
		t.Errorf("G = %s, want 42.5s (30s + 12.5s)", g)
	}
}

// The env the chart renders into core-provider's ConfigMap (and every controller's), read the way main
// reads it. Names and values are literals: the chart's keys, which
// TestOwnershipLeaseTimingsRenderOnceForBothBinaries holds the chart to.
func TestOwnershipLeaseTimingsFromEnv(t *testing.T) {
	env := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	got, err := OwnershipLeaseTimingsFromEnv(env(map[string]string{
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION":       "41s",
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "27s",
		"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD":   " 7s ",
	}))
	if err != nil || got != (OwnershipLeaseTimings{Duration: 41 * time.Second, RenewDeadline: 27 * time.Second, RetryPeriod: 7 * time.Second}) {
		t.Fatalf("the three set: %s, %v; want 41s/27s/7s", got, err)
	}
	got, err = OwnershipLeaseTimingsFromEnv(env(nil))
	if err != nil || got != (OwnershipLeaseTimings{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}) {
		t.Fatalf("none set: %s, %v; want 30s/20s/5s", got, err)
	}
	for _, c := range []struct {
		name string
		env  map[string]string
		says []string
	}{
		{"renewDeadline = duration", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "5s",
		}, []string{"duration must be greater than renewDeadline", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION=20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE=20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD=5s", "cdc.ownershipLease"}},
		{"renewDeadline above duration", map[string]string{
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION": "20s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE": "25s", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "5s",
		}, []string{"duration must be greater than renewDeadline"}},
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
			"COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD": "15s",
		}, []string{"are a set", "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION, COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE unset"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := OwnershipLeaseTimingsFromEnv(env(c.env))
			if err == nil {
				t.Fatalf("accepted as %s", got)
			}
			for _, s := range c.says {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("error %q does not say %q", err, s)
				}
			}
		})
	}
}

// Validate accepts exactly what the controllers' elector accepts: each set is checked against a literal
// verdict and against leaderelection.NewLeaderElector, the independent source (the cdc's elector).
func TestOwnershipLeaseTimingsValidateIsClientGos(t *testing.T) {
	s := time.Second
	for _, c := range []struct {
		t  OwnershipLeaseTimings
		ok bool
	}{
		{OwnershipLeaseTimings{30 * s, 20 * s, 5 * s}, true},
		{OwnershipLeaseTimings{15 * s, 10 * s, 2 * s}, true},
		{OwnershipLeaseTimings{60 * s, 40 * s, 15 * s}, true},
		{OwnershipLeaseTimings{7 * s, 6100 * time.Millisecond, 5 * s}, true},
		{OwnershipLeaseTimings{20 * s, 20 * s, 5 * s}, false},
		{OwnershipLeaseTimings{20 * s, 30 * s, 5 * s}, false},
		{OwnershipLeaseTimings{7 * s, 6 * s, 5 * s}, false},
		{OwnershipLeaseTimings{30 * s, 20 * s, 20 * s}, false},
		{OwnershipLeaseTimings{30 * s, 20 * s, 0}, false},
		{OwnershipLeaseTimings{30 * s, 20 * s, -5 * s}, false},
		{OwnershipLeaseTimings{}, false},
	} {
		t.Run(fmt.Sprintf("%s/%s/%s", c.t.Duration, c.t.RenewDeadline, c.t.RetryPeriod), func(t *testing.T) {
			err := c.t.Validate()
			if (err == nil) != c.ok {
				t.Fatalf("Validate = %v, want ok=%v", err, c.ok)
			}
			_, cgErr := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
				Lock:          &resourcelock.LeaseLock{LockConfig: resourcelock.ResourceLockConfig{Identity: "pod-a"}},
				LeaseDuration: c.t.Duration, RenewDeadline: c.t.RenewDeadline, RetryPeriod: c.t.RetryPeriod,
				Callbacks: leaderelection.LeaderCallbacks{OnStartedLeading: func(context.Context) {}, OnStoppedLeading: func() {}},
			})
			if (cgErr == nil) != (err == nil) {
				t.Errorf("client-go says %v, Validate says %v", cgErr, err)
			}
		})
	}
}
