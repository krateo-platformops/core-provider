package ownership

import (
	"fmt"
	"strings"
	"time"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
)

// The ownership Lease's timings (core-provider#660), rendered by core-provider's chart from ONE value,
// cdc.ownershipLease.{duration,renewDeadline,retryPeriod}, into every controller's env (dedicated and
// pool, core-provider.cdcProcessEnvTail) and into core-provider's own, under these same names: core-provider
// derives every wait on these Leases from them (its deploy.OwnershipLeaseTimings), so the two binaries
// read one source and cannot disagree.
const (
	EnvLeaseDuration = "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_DURATION"
	EnvRenewDeadline = "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RENEW_DEADLINE"
	EnvRetryPeriod   = "COMPOSITION_CONTROLLER_OWNERSHIP_LEASE_RETRY_PERIOD"
)

// Timings are client-go's leader-election timings for the ownership Lease. A holder renews every
// RetryPeriod, so 1/RetryPeriod is each controller's Lease write rate at rest; a successor waits out
// LeaseDuration after a holder that died without releasing.
type Timings struct {
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// DefaultTimings are what a controller runs with when its env sets none (a hand-run binary): the chart's
// default, Diego 2026-11-25 for #660, and core-provider's DefaultOwnershipLeaseTimings. Not
// unstructured-runtime's 15s/10s/2s, so a controller and core-provider agree even then.
func DefaultTimings() Timings {
	return Timings{LeaseDuration: 30 * time.Second, RenewDeadline: 20 * time.Second, RetryPeriod: 5 * time.Second}
}

func (t Timings) String() string {
	return fmt.Sprintf("%s=%s %s=%s %s=%s", EnvLeaseDuration, t.LeaseDuration, EnvRenewDeadline, t.RenewDeadline, EnvRetryPeriod, t.RetryPeriod)
}

// Validate is client-go's rule for the set (leaseDuration > renewDeadline > retryPeriod x 1.2), with
// every value greater than zero: zero is no timing here, never unstructured-runtime's default.
func (t Timings) Validate() error {
	if t.LeaseDuration <= 0 || t.RenewDeadline <= 0 || t.RetryPeriod <= 0 {
		return fmt.Errorf("invalid ownership lease timings %s: every one must be greater than zero", t)
	}
	if err := controller.ValidateOwnershipTimings(t.LeaseDuration, t.RenewDeadline, t.RetryPeriod); err != nil {
		return fmt.Errorf("invalid ownership lease timings %s: %w", t, err)
	}
	return nil
}

// TimingsFromEnv reads the three timings, as Go durations, through lookup (os.LookupEnv). None set is
// DefaultTimings. They are a set: some but not all of them set, a value that is not a duration, or a set
// Validate refuses is an error naming the three, and the controller refuses to start, never falling
// back to a default for the value it could not read.
func TimingsFromEnv(lookup func(string) (string, bool)) (Timings, error) {
	keys := []string{EnvLeaseDuration, EnvRenewDeadline, EnvRetryPeriod}
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
		return DefaultTimings(), nil
	}
	if len(unset) > 0 {
		return Timings{}, fmt.Errorf("the ownership lease timings are a set: %s set, %s unset; set all three (core-provider's chart renders them from cdc.ownershipLease)",
			strings.Join(set, ", "), strings.Join(unset, ", "))
	}
	parsed := make([]time.Duration, len(keys))
	for i, k := range keys {
		d, err := time.ParseDuration(raw[k])
		if err != nil {
			return Timings{}, fmt.Errorf("%s=%q is not a Go duration: %w (the ownership lease timings are %s=%q %s=%q %s=%q)",
				k, raw[k], err, keys[0], raw[keys[0]], keys[1], raw[keys[1]], keys[2], raw[keys[2]])
		}
		parsed[i] = d
	}
	t := Timings{LeaseDuration: parsed[0], RenewDeadline: parsed[1], RetryPeriod: parsed[2]}
	if err := t.Validate(); err != nil {
		return Timings{}, err
	}
	return t, nil
}
