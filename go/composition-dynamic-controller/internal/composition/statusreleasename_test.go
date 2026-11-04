package composition

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// status.releaseName exists so that RESOLUTION and PERSISTENCE can be told apart from the object
// alone, and the two arms below are the two states it has to separate. Asserted together in one
// test because the value of either depends entirely on the other coming out differently: an
// implementation that always wrote the key would pass the first arm, and one that never wrote it
// would pass the second.
//
// The requirement came from the openstack-ironic-operator-kog session, which could only answer
// "never written, or written and then lost?" by counting log lines -- 11 "Adopting the existing
// helm release" on its probe against 0 in production -- because managedFields is stripped on these
// objects, reading zero entries both on the probe and on a production object that demonstrably
// carries the label. An inference from log volume stops working the moment the log line is
// reworded, which is why this is a status field and not a better log message.
func TestSetStatus_ReleaseNameIsPersistedWhenResolvedAndAbsentWhenNot(t *testing.T) {
	readReleaseName := func(t *testing.T, mg *unstructured.Unstructured) (string, bool) {
		t.Helper()
		v, found, err := unstructured.NestedString(mg.Object, "status", "releaseName")
		if err != nil {
			t.Fatalf("status.releaseName is not a string: %v", err)
		}
		return v, found
	}

	// ARM 1: a resolved release name is persisted. Without this the field cannot report anything.
	t.Run("resolved name is written", func(t *testing.T) {
		h := &handler{}
		mg := fireworksMg(1)

		if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
			chartURL: "u", chartVersion: "v", releaseName: "lab-ha-54xd86b6",
			conditionType: ConditionTypeAvailable,
		}); err != nil {
			t.Fatalf("setStatus: %v", err)
		}

		got, found := readReleaseName(t, mg)
		if !found {
			t.Fatal("status.releaseName is ABSENT after a reconcile that resolved one. The field " +
				"is the only observable that distinguishes 'adoption resolved a name and the write " +
				"landed' from 'resolved in memory and never persisted', which is the #335 defect " +
				"this exists to make visible")
		}
		if got != "lab-ha-54xd86b6" {
			t.Errorf("status.releaseName = %q, want %q", got, "lab-ha-54xd86b6")
		}
	})

	// ARM 2: THE ONE THAT MATTERS, and the one a test written only for the happy path skips.
	//
	// Absence is a meaningful third state. If setStatus writes "" when it has no release name, the
	// apiserver materialises the key on every composition and "resolved but never persisted"
	// becomes indistinguishable from "resolved and persisted as empty" -- the field would then
	// always be present and could never report that anything was missing. This arm fails if the
	// non-empty guard in setStatus is removed, and it is the reason status.schema.json declares the
	// field with no `default`.
	t.Run("an unresolved name leaves the key absent rather than empty", func(t *testing.T) {
		h := &handler{}
		mg := fireworksMg(1)

		if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
			chartURL: "u", chartVersion: "v", releaseName: "",
			conditionType: ConditionTypeAvailable,
		}); err != nil {
			t.Fatalf("setStatus: %v", err)
		}

		got, found := readReleaseName(t, mg)
		if found {
			t.Fatalf("status.releaseName is PRESENT as %q after a reconcile with no resolved name. "+
				"An always-present key cannot report an absent value: the three states this field "+
				"separates (label present / label absent plus name persisted / label absent and "+
				"name lost) collapse to two, and the third is the defect", got)
		}
	})

	// THE DIFFERENTIAL. Both arms above assert against a fixed expectation, so both would pass if
	// setStatus ignored opts.releaseName and something else in the harness happened to supply the
	// values. This asserts the two outcomes differ as a direct consequence of the one input that
	// changed, which is the claim the field actually rests on.
	t.Run("the two arms differ because of releaseName alone", func(t *testing.T) {
		run := func(name string) (string, bool) {
			h := &handler{}
			mg := fireworksMg(1)
			if err := h.setStatus(context.Background(), mg, &statusManagerOpts{
				chartURL: "u", chartVersion: "v", releaseName: name,
				conditionType: ConditionTypeAvailable,
			}); err != nil {
				t.Fatalf("setStatus(releaseName=%q): %v", name, err)
			}
			return readReleaseName(t, mg)
		}

		withName, withFound := run("lab-ha-54xd86b6")
		withoutName, withoutFound := run("")

		if withFound == withoutFound {
			t.Fatalf("presence of status.releaseName did not change with opts.releaseName: "+
				"found=%v both times (values %q and %q). setStatus is not reading the field, so "+
				"both arms above are vacuous", withFound, withName, withoutName)
		}
	})
}
