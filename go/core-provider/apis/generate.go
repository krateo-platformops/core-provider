//go:build generate
// +build generate

// Remove existing CRDs
//go:generate rm -rf ../crds

// Generate deepcopy methodsets and CRD manifests
//go:generate go run -tags generate sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile=../hack/boilerplate.go.txt paths=./... crd:crdVersions=v1 output:artifacts:config=../crds
// Sync the two copies of the generated CRDs, so they cannot drift from crds/ (#435):
//   - the core-provider-crds chart, which is what users install (TestShippedCRDChartIsTheGeneratedCRDs);
//   - the CompositionDefinition CRD core-provider projects onto REMOTE targets, embedded from
//     internal/tools/deploy/assets because //go:embed cannot reach outside its package. Until #435 it
//     was hand-kept and had not changed since the monorepo fold, so spokes pruned every newer field.
//go:generate sh -c "cp ../crds/*.yaml ../../../helm/core-provider-crds/templates/"
//go:generate cp ../crds/core.krateo.io_compositiondefinitions.yaml ../internal/tools/deploy/assets/compositiondefinitions-crd.yaml

package apis

import (
	_ "sigs.k8s.io/controller-tools/cmd/controller-gen" //nolint:typecheck
)
