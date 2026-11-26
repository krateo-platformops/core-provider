// Package writtenby is core-provider's half of core-provider#669: the stamp it puts on every
// CompositionDefinition status it writes, so a reader of the object (who may not be allowed to read
// core-provider's Deployment) knows which release's reason table describes its conditions.
//
// The composition-dynamic-controller has the other half, for compositions, in its own module.
package writtenby

import (
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

// Name is status.writtenBy.name on a CompositionDefinition. docs/cd-ready-reasons.json is the table
// for it, and its notice says so (TestWrittenByNamesMapToOneTableEach, internal/cdreason).
const Name = "core-provider"

// New is this binary's stamp from its build version and commit, written as the build carries them:
// a release build's bare tag (2.13.135), or "dev"/"unknown" on a build that was not stamped.
func New(version, commit string) compositiondefinitionsv1alpha1.WrittenBy {
	return compositiondefinitionsv1alpha1.WrittenBy{Name: Name, Version: version, Commit: commit}
}

// Set puts w on cd's in-memory status. It never writes: it is called where a writer builds the status
// it is about to write anyway, so a pass that changes nothing else carries the stored value back and
// the apiserver records nothing. A zero w (a writer built without a stamp, as in tests) leaves the
// field as it is.
func Set(cd *compositiondefinitionsv1alpha1.CompositionDefinition, w compositiondefinitionsv1alpha1.WrittenBy) {
	if cd == nil || w == (compositiondefinitionsv1alpha1.WrittenBy{}) {
		return
	}
	stamp := w
	cd.Status.WrittenBy = &stamp
}
