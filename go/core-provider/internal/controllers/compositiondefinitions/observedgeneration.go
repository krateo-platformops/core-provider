package compositiondefinitions

import (
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	"github.com/krateo-platformops/provider-runtime/pkg/meta"
)

// recordObservedGeneration records that the Ready this Observe just wrote was determined from cr's
// current spec (core-provider#644).
//
// WHAT IT MEANS. status.observedGeneration is the generation the Ready beside it was determined from.
// A reader compares it with metadata.generation to tell a verdict about the current spec from one
// about an earlier spec, whatever the reason the pass that would renew it has not: not run, failing
// before a verdict, paused, withheld by policy, or an update in progress.
//
// WHERE IT IS CALLED. At each place Observe itself DECIDES Ready, and where the remote seed refuses the
// chart (#649), which Update runs too: it decides Ready there, from the spec of the object it is recorded
// on. Never where Observe KEEPS an earlier one (markUpdatePending, #395): during a chart bump the
// definition keeps Ready=True about the old version, and while the add of the new version waits out a
// prune grace it keeps it with Synced=True for minutes (#484), so recording there would be the very false
// "processed" this field exists to rule out. Never on an error that leaves Ready=NotObserved. A place that
// forgets to call it only leaves the field behind, which reads as "not determined yet".
//
// WHY THE VALUE IS COPIED HERE and not read when the status is written. The runtime writes the object
// this pass holds, under its resourceVersion, so a spec edited mid-pass makes that write conflict and
// the whole status is dropped. Where the runtime replaces the object with the server's copy instead (it
// adds the finalizer, and writes the create annotations on a fresh read), the STORED status comes with
// it, so this value is replaced by the older stored one. Both leave the field behind, never ahead;
// metadata.generation read at write time would be the new generation beside conditions computed from the
// old one.
//
// Two exceptions record nothing:
//   - a definition being deleted: its deletion moved metadata.generation, and its teardown determines
//     Deleting, not whether the spec works. The field stays as it was.
//   - a pass whose clients were built for another target. Connect reads the cached spec, Observe the
//     live one; when spec.deploy.targetRef changed in between, what was observed is the OLD cluster.
func (e *external) recordObservedGeneration(cr *compositiondefinitionsv1alpha1.CompositionDefinition) {
	if meta.WasDeleted(cr) || targetNameOf(cr.Spec.Deploy) != e.connectedTarget {
		return
	}
	cr.Status.ObservedGeneration = cr.GetGeneration()
}

// targetNameOf is the KubernetesTarget a spec names, "" for a local definition.
func targetNameOf(d *compositiondefinitionsv1alpha1.DeploymentTarget) string {
	if !clusterkube.IsRemote(d) {
		return ""
	}
	return d.TargetRef.Name
}
