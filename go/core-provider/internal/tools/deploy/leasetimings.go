package deploy

import "time"

// OwnershipLeaseTimings are the composition controllers' ownership Lease timings (docs/design/
// cdc-pooling.md, step 2): client-go's leaseDuration, renewDeadline and retryPeriod, which
// unstructured-runtime's Ownership passes to its elector. Every wait core-provider derives from a
// controller's Lease is derived from these, never from a constant of its own, so a cluster that sets
// longer timings (renewals are the controllers' largest write load at rest) moves them all together.
type OwnershipLeaseTimings struct {
	Duration      time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// DefaultOwnershipLeaseTimings are unstructured-runtime's defaults, which every controller runs with
// while the chart sets none: 15s, 10s, 2s.
func DefaultOwnershipLeaseTimings() OwnershipLeaseTimings {
	return OwnershipLeaseTimings{Duration: 15 * time.Second, RenewDeadline: 10 * time.Second, RetryPeriod: 2 * time.Second}
}

// LeaseTimings are the timings the controllers run with. Set once at startup from the same chart value
// the controllers read; the defaults until then.
var LeaseTimings = DefaultOwnershipLeaseTimings()

// Quiet is how long a Lease must stay unheld before its controller counts as stopped: longer than the
// elector's longest wait between attempts, retryPeriod jittered by client-go's JitterFactor (1.2) to
// 2.2 x retryPeriod, so a successor that was only waiting has taken it by then (and is waited for in
// turn). 2.5 x retryPeriod: 5s at the defaults.
func (t OwnershipLeaseTimings) Quiet() time.Duration { return t.RetryPeriod * 5 / 2 }

// MemberGrace is G (#653): how long a pool member's Lease may be unheld before its definition reads
// Unavailable. A successor takes a Lease within Quiet of its release, and within a full leaseDuration
// plus Quiet of a holder that died without releasing it (client-go waits out the duration first). So a
// same-key replacement, or a pool pod's rolling update, never reads Unavailable. 20s at the defaults.
func (t OwnershipLeaseTimings) MemberGrace() time.Duration { return t.Duration + t.Quiet() }
