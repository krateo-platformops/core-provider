package metrics

import (
	"context"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// HelmMetrics wraps Helm client operations with timing
type HelmMetrics struct {
	ctx context.Context
}

func NewHelmMetrics(ctx context.Context) *HelmMetrics {
	return &HelmMetrics{ctx: ctx}
}

// RecordInstall records metrics for Helm install operation
func (h *HelmMetrics) RecordInstall(duration float64, err error) {
	m := FromContext(h.ctx)
	if m == nil {
		return
	}
	m.RecordHelmInstallDuration(h.ctx, duration)
	m.IncHelmInstallTotal(h.ctx)
	if err != nil {
		m.IncHelmInstallErrors(h.ctx)
	}
}

// RecordUpgrade records metrics for Helm upgrade operation
func (h *HelmMetrics) RecordUpgrade(duration float64, err error) {
	m := FromContext(h.ctx)
	if m == nil {
		return
	}
	m.RecordHelmUpgradeDuration(h.ctx, duration)
	m.IncHelmUpgradeTotal(h.ctx)
	if err != nil {
		m.IncHelmUpgradeErrors(h.ctx)
	}
}

// RecordUninstall records metrics for Helm uninstall operation
func (h *HelmMetrics) RecordUninstall(duration float64, err error) {
	m := FromContext(h.ctx)
	if m == nil {
		return
	}
	m.RecordHelmUninstallDuration(h.ctx, duration)
	m.IncHelmUninstallTotal(h.ctx)
	if err != nil {
		m.IncHelmUninstallErrors(h.ctx)
	}
}

// TimedInstall executes and times a Helm install operation
func (h *HelmMetrics) TimedInstall(fn func() error) error {
	timer := NewTimer()
	err := fn()
	h.RecordInstall(timer.Elapsed(), err)
	return err
}

// TimedUpgrade executes and times a Helm upgrade operation
func (h *HelmMetrics) TimedUpgrade(fn func() error) error {
	timer := NewTimer()
	err := fn()
	h.RecordUpgrade(timer.Elapsed(), err)
	return err
}

// TimedUninstall executes and times a Helm uninstall operation
func (h *HelmMetrics) TimedUninstall(fn func() error) error {
	timer := NewTimer()
	err := fn()
	h.RecordUninstall(timer.Elapsed(), err)
	return err
}

// TimedInstallWithResult executes and times a Helm install operation with result
func (h *HelmMetrics) TimedInstallWithResult(fn func() (*helmconfig.Release, error)) (*helmconfig.Release, error) {
	timer := NewTimer()
	result, err := fn()
	h.RecordInstall(timer.Elapsed(), err)
	return result, err
}

// TimedUpgradeWithResult executes and times a Helm upgrade operation with result
func (h *HelmMetrics) TimedUpgradeWithResult(fn func() (*helmconfig.Release, error)) (*helmconfig.Release, error) {
	timer := NewTimer()
	result, err := fn()
	h.RecordUpgrade(timer.Elapsed(), err)
	return result, err
}

// RecordRBAC records metrics for RBAC operations (apply/uninstall)
func (h *HelmMetrics) RecordRBAC(duration float64, err error) {
	m := FromContext(h.ctx)
	if m == nil {
		return
	}
	m.RecordRBACApplyDuration(h.ctx, duration)
	m.IncRBACApplyTotal(h.ctx)
	if err != nil {
		m.IncRBACApplyErrors(h.ctx)
	}
}

// TimedRBAC executes and times an RBAC operation
func (h *HelmMetrics) TimedRBAC(fn func() error) error {
	timer := NewTimer()
	err := fn()
	h.RecordRBAC(timer.Elapsed(), err)
	return err
}
