package compositiondefinitions

import (
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

func poolCD(ns, name string, age int, version, pool string) compositiondefinitionsv1alpha1.CompositionDefinition {
	cd := ownerCD(ns, name, age)
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://example/" + name, Version: version}
	cd.Status.ApiVersion = "composition.krateo.io/" + version
	cd.Status.Kind = "Page"
	cd.Status.Resource = "pages"
	if pool != "" {
		p := pool
		cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{Pool: &p}
	}
	return cd
}
