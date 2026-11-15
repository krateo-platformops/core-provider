package metrics

import (
	"context"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/chartinspector"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/rbacgen"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
)

// WrapChartInspector wraps a ChartInspector with metrics collection
func WrapChartInspector(inspector chartinspector.ChartInspectorInterface) chartinspector.ChartInspectorInterface {
	return &metricChartInspector{wrapped: inspector}
}

// WrapChartInspectorStruct wraps a ChartInspector struct with metrics collection
func WrapChartInspectorStruct(inspector chartinspector.ChartInspector) chartinspector.ChartInspectorInterface {
	return &metricChartInspector{wrapped: &inspector}
}

type metricChartInspector struct {
	wrapped chartinspector.ChartInspectorInterface
}

func (m *metricChartInspector) Resources(ctx context.Context, params chartinspector.Parameters) ([]chartinspector.Resource, error) {
	timer := NewTimer()

	resources, err := m.wrapped.Resources(ctx, params)

	metrics := FromContext(ctx)
	if metrics != nil {
		metrics.RecordChartInspectorDuration(ctx, timer.Elapsed())
		metrics.IncChartInspectorTotal(ctx)
		if err != nil {
			metrics.IncChartInspectorErrors(ctx)
		}
	}

	return resources, err
}

// WrapRBACGen wraps RBACGen with metrics collection
func WrapRBACGen(gen rbacgen.RBACGenInterface) rbacgen.RBACGenInterface {
	return &metricRBACGen{wrapped: gen}
}

type metricRBACGen struct {
	wrapped rbacgen.RBACGenInterface
}

func (m *metricRBACGen) WithBaseName(name string) rbacgen.RBACGenInterface {
	m.wrapped = m.wrapped.WithBaseName(name)
	return m
}

func (m *metricRBACGen) Generate(ctx context.Context, params rbacgen.Parameters) (*rbac.RBAC, error) {
	timer := NewTimer()

	result, err := m.wrapped.Generate(ctx, params)

	metrics := FromContext(ctx)
	if metrics != nil {
		metrics.RecordRBACGenerationDuration(ctx, timer.Elapsed())
		metrics.IncRBACGenerationTotal(ctx)
		if err != nil {
			metrics.IncRBACGenerationErrors(ctx)
		}
	}

	return result, err
}
