package deploy

import (
	"fmt"
	"strings"
	"time"

	"k8s.io/client-go/tools/leaderelection"
)

// OwnershipLeaseTimings are the composition controllers' ownership Lease timings (docs/design/
// cdc-pooling.md, step 2; #660): client-go's leaseDuration, renewDeadline and retryPeriod, which every
// controller passes to unstructured-runtime's Ownership. Every wait core-provider derives from a
// controller's Lease is derived from these, never from a constant of its own, so a cluster that sets
// longer timings (renewals are the controllers' largest write load at rest) moves them all together.
//
// They are ONE chart value, cdc.ownershipLease, rendered under the same three env names into every
// controller's env ConfigMap and into core-provider's own (OwnershipLeaseTimingsFromEnv), so the two
// binaries read one source. core-provider takes them once at startup, and passes them down through
// options: compositiondefinitions.Options.LeaseTimings, then PoolOptions and UndeployOptions.
type OwnershipLeaseTimings struct {
	Duration      time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// The env names of the three timings, the same in core-provider and in every controller
// (composition-dynamic-controller internal/ownership). helm/core-provider renders them from
// cdc.ownershipLease.{duration,renewDeadline,retryPeriod}.
const (
	EnvOwnershipLeaseDuration      = "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION"
	EnvOwnershipLeaseRenewDeadline = "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE"
	EnvOwnershipLeaseRetryPeriod   = "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD"
)

// DefaultOwnershipLeaseTimings are the chart's default (#660), which a controller also
// runs with when its env sets none: 30s, 20s, 5s. One renewal per controller every 5s (0.2/s, from 0.5/s
// at unstructured-runtime's 15s/10s/2s).
func DefaultOwnershipLeaseTimings() OwnershipLeaseTimings {
	return OwnershipLeaseTimings{Duration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}
}

func (t OwnershipLeaseTimings) String() string {
	return fmt.Sprintf("%s=%s %s=%s %s=%s", EnvOwnershipLeaseDuration, t.Duration,
		EnvOwnershipLeaseRenewDeadline, t.RenewDeadline, EnvOwnershipLeaseRetryPeriod, t.RetryPeriod)
}

// Validate is client-go's leader-election rule for the set, the one every controller's elector applies:
// duration > renewDeadline > retryPeriod x 1.2, every one greater than zero. A zero set (timings nobody
// passed down) is refused like any other invalid one: a zero Quiet would read a Lease as released the
// moment it is unheld.
func (t OwnershipLeaseTimings) Validate() error {
	bad := func(rule string) error {
		return fmt.Errorf("invalid ownership lease timings %s: %s (helm/core-provider cdc.ownershipLease)", t, rule)
	}
	switch {
	case t.Duration <= 0 || t.RenewDeadline <= 0 || t.RetryPeriod <= 0:
		return bad("every one must be greater than zero")
	case t.Duration <= t.RenewDeadline:
		return bad("duration must be greater than renewDeadline")
	case t.RenewDeadline <= time.Duration(leaderelection.JitterFactor*float64(t.RetryPeriod)):
		return bad(fmt.Sprintf("renewDeadline must be greater than retryPeriod x %v (client-go's JitterFactor)", leaderelection.JitterFactor))
	}
	return nil
}

// OwnershipLeaseTimingsFromEnv reads the three timings, as Go durations, through lookup (os.LookupEnv).
// None set is DefaultOwnershipLeaseTimings (a hand-run binary). They are a set: some but not all set, a
// value that is not a duration, or a set Validate refuses is an error naming the three, and core-provider
// refuses to start, never falling back to a default for the value it could not read.
func OwnershipLeaseTimingsFromEnv(lookup func(string) (string, bool)) (OwnershipLeaseTimings, error) {
	keys := []string{EnvOwnershipLeaseDuration, EnvOwnershipLeaseRenewDeadline, EnvOwnershipLeaseRetryPeriod}
	var set, unset []string
	raw := map[string]string{}
	for _, k := range keys {
		if v, ok := lookup(k); ok {
			set, raw[k] = append(set, k), strings.TrimSpace(v)
		} else {
			unset = append(unset, k)
		}
	}
	if len(set) == 0 {
		return DefaultOwnershipLeaseTimings(), nil
	}
	if len(unset) > 0 {
		return OwnershipLeaseTimings{}, fmt.Errorf("the ownership lease timings are a set: %s set, %s unset; set all three (helm/core-provider renders them from cdc.ownershipLease)",
			strings.Join(set, ", "), strings.Join(unset, ", "))
	}
	parsed := make([]time.Duration, len(keys))
	for i, k := range keys {
		d, err := time.ParseDuration(raw[k])
		if err != nil {
			return OwnershipLeaseTimings{}, fmt.Errorf("%s=%q is not a Go duration: %w (the ownership lease timings are %s=%q %s=%q %s=%q)",
				k, raw[k], err, keys[0], raw[keys[0]], keys[1], raw[keys[1]], keys[2], raw[keys[2]])
		}
		parsed[i] = d
	}
	t := OwnershipLeaseTimings{Duration: parsed[0], RenewDeadline: parsed[1], RetryPeriod: parsed[2]}
	if err := t.Validate(); err != nil {
		return OwnershipLeaseTimings{}, err
	}
	return t, nil
}

// Quiet is how long a Lease must stay unheld before its controller counts as stopped: longer than the
// elector's longest wait between attempts, retryPeriod jittered by client-go's JitterFactor (1.2) to
// 2.2 x retryPeriod, so a successor that was only waiting has taken it by then (and is waited for in
// turn). 2.5 x retryPeriod: 12.5s at the default 5s.
func (t OwnershipLeaseTimings) Quiet() time.Duration { return t.RetryPeriod * 5 / 2 }

// MemberGrace is G (#653): how long a pool member's Lease may be unheld before its definition reads
// Unavailable. A successor takes a Lease within Quiet of its release, and within a full leaseDuration
// plus Quiet of a holder that died without releasing it (client-go waits out the duration first). So a
// same-key replacement, or a pool pod's rolling update, never reads Unavailable. 42.5s at the default
// timings (30s + 12.5s).
func (t OwnershipLeaseTimings) MemberGrace() time.Duration { return t.Duration + t.Quiet() }
