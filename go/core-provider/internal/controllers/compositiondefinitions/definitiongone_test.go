package compositiondefinitions

import "testing"

// The truth table behind Observe's "this definition's external resource is gone", which is what
// lets the reconciler drop the finalizer.
//
// #205: Delete keeps the shared CRD whenever ANY live definition still needs the Kind, while
// Observe only reported gone when a successor held this exact Kind+VERSION. A sibling on another
// version kept the CRD standing while Observe went on calling it this definition's — finalizer
// never dropped, Delete re-ran forever.
func TestDefinitionGone_TruthTable(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		remote                   bool
		hasVersionSuccessor      bool
		successorSharesNamespace bool
		kindHeldByAnother        bool
		ownControllerPresent     bool
		want                     bool
		why                      string
	}{
		{
			name:              "#205: sibling on another version, own controller removed",
			kindHeldByAnother: true, ownControllerPresent: false, want: true,
			why: "the CRD is standing for the sibling's version, not for this definition; without " +
				"this the finalizer never drops and Delete loops forever",
		},
		{
			name:              "#205: sibling on another version, own controller still up",
			kindHeldByAnother: true, ownControllerPresent: true, want: false,
			why: "ordering: the first Observe of a deletion must report exists so Delete runs and " +
				"removes the controller — returning gone here short-circuits the cleanup",
		},
		{
			name:                "version successor in another namespace, own controller removed",
			hasVersionSuccessor: true, ownControllerPresent: false, want: true,
			why: "#203/#212's hand-over, unchanged",
		},
		{
			name:                "version successor in another namespace, own controller still up",
			hasVersionSuccessor: true, ownControllerPresent: true, want: false,
			why: "same ordering rule as above",
		},
		{
			name:                "version successor sharing this namespace",
			hasVersionSuccessor: true, successorSharesNamespace: true, ownControllerPresent: true, want: true,
			why: "two definitions in one namespace share every controller object by name, so nothing " +
				"belongs to the leaving one alone and there is nothing to wait for",
		},
		{
			name:                 "nothing else holds the Kind at all",
			ownControllerPresent: false, want: false,
			why: "the genuinely last definition: the CRD is deleted, and Observe finds no CRD by a " +
				"different path. Reporting gone HERE would drop the finalizer before teardown",
		},
		{
			name:   "remote target, everything else says gone",
			remote: true, hasVersionSuccessor: true, kindHeldByAnother: true, ownControllerPresent: false,
			want: false,
			why: "a remote target's CRD and cdc are dedicated to it on its spoke and always go; the " +
				"management cluster's accounting must not shortcut that teardown",
		},
		{
			name:                "sibling holds the Kind AND a version successor exists",
			hasVersionSuccessor: true, kindHeldByAnother: true, ownControllerPresent: false, want: true,
			why: "the version successor settles it; the Kind-level branch is only a fallback",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := definitionGone(tc.remote, tc.hasVersionSuccessor, tc.successorSharesNamespace,
				tc.kindHeldByAnother, tc.ownControllerPresent)
			if got != tc.want {
				t.Errorf("definitionGone = %v, want %v — %s", got, tc.want, tc.why)
			}
		})
	}
}

// The property that makes #205 a defect rather than a difference of opinion: Delete's decision to
// KEEP the shared CRD and Observe's decision that the CRD is NOT this definition's must agree.
//
// Delete keeps the CRD exactly when kindReferencedElsewhere says another definition needs the Kind.
// So for every state where Delete keeps it, Observe must eventually report gone — otherwise the
// finalizer is waiting on a CRD nobody will ever remove.
func TestDefinitionGone_AgreesWithDeletesKeepDecision(t *testing.T) {
	// "Delete kept the CRD" is exactly kindHeldByAnother, since that is the same predicate
	// (kindReferencedElsewhere) Delete uses for skipCRD.
	for _, hasVersionSuccessor := range []bool{false, true} {
		deleteKeptTheCRD := true // kindHeldByAnother
		// Once teardown has run, this definition's own controller is gone.
		gone := definitionGone(false, hasVersionSuccessor, false, deleteKeptTheCRD, false)
		if !gone {
			t.Errorf("Delete kept the CRD (hasVersionSuccessor=%v) but Observe never reports gone: "+
				"the finalizer waits forever on a CRD that is deliberately not being removed",
				hasVersionSuccessor)
		}
	}

	// And the converse must hold too, or definitions would terminate before their teardown: when
	// nothing holds the Kind, Delete removes the CRD and Observe must NOT pre-emptively say gone.
	if definitionGone(false, false, false, false, false) {
		t.Error("the last definition on a Kind reported gone from this path; the finalizer could " +
			"drop before the CRD and instances are torn down")
	}
}
