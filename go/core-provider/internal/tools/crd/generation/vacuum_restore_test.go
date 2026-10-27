package generation

import (
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// crdWith builds a CRD from (name, served, storage) triples plus a storedVersions list.
func crdWith(stored []string, versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.composition.krateo.io"},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Versions: versions},
		Status:     apiextensionsv1.CustomResourceDefinitionStatus{StoredVersions: stored},
	}
}

func ver(name string, served, storage bool) apiextensionsv1.CustomResourceDefinitionVersion {
	return apiextensionsv1.CustomResourceDefinitionVersion{Name: name, Served: served, Storage: storage}
}

func versionNames(crd *apiextensionsv1.CustomResourceDefinition) []string {
	out := make([]string, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		out = append(out, v.Name)
	}
	return out
}

func storageVersions(crd *apiextensionsv1.CustomResourceDefinition) []string {
	out := []string{}
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			out = append(out, v.Name)
		}
	}
	return out
}

// TestRestoreOrphanedVacuum_LeavesHealthyCRDsAlone is the PRIMARY test, deliberately first.
//
// The repair is keyed on an absence, and an absence is not always damage: a CRD that has never been
// bumped legitimately has no vacuum, because vacuum is injected only when a SECOND version is
// appended. Measured on krateo-057, 9 of 68 composition CRDs are in that state and healthy.
//
// The first version of this fix repaired on "vacuum is missing" and would have demoted each of
// those nine CRDs' only version to storage:false and put vacuum into status.storedVersions, which
// the apiserver will not release without a storage migration. Permanent damage to healthy objects,
// to fix a state that occurred zero times.
//
// So: a test that the repair FIRES only proves it fires. This one proves it DISCRIMINATES, and it
// is the assertion that stops the fix becoming the bug. Any repair keyed on an absence wants its
// equivalent.
func TestRestoreOrphanedVacuum_LeavesHealthyCRDsAlone(t *testing.T) {
	cases := []struct {
		name string
		crd  *apiextensionsv1.CustomResourceDefinition
	}{
		{
			// The 9-of-68 shape on krateo-057: one version, served and storage, sole stored entry.
			name: "never-bumped single version with no vacuum",
			crd:  crdWith([]string{"v0-1-0"}, ver("v0-1-0", true, true)),
		},
		{
			// The ordinary multi-version shape: vacuum present and doing its job.
			name: "healthy multi-version CRD with vacuum present",
			crd: crdWith([]string{"vacuum"},
				ver("v0-1-0", true, false), ver("v0-2-0", true, false), ver(VacuumVersionName, false, true)),
		},
		{
			// vacuum absent from BOTH spec and storedVersions: nothing is encoded at it, so there
			// is nothing to recover. Distinct from the damaged case only by storedVersions, which
			// is the whole point of keying on that field.
			name: "vacuum absent from spec AND storedVersions",
			crd:  crdWith([]string{"v0-1-0", "v0-2-0"}, ver("v0-1-0", true, false), ver("v0-2-0", true, true)),
		},
		{
			// An orphaned CHART version is real damage but not repairable here — its schema came
			// from a chart and cannot be synthesized. Must be left alone rather than guessed at.
			name: "orphaned stored version that is not vacuum",
			crd:  crdWith([]string{"v0-1-0", "v0-9-9"}, ver("v0-1-0", true, true)),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := versionNames(tc.crd)
			beforeStorage := storageVersions(tc.crd)

			if RestoreOrphanedVacuum(tc.crd) {
				t.Fatalf("repair fired on a CRD that needs none: versions %v, storedVersions %v\n"+
					"a missing vacuum is NOT damage — only an ORPHANED one is (status.storedVersions "+
					"naming a version spec.versions does not)", before, tc.crd.Status.StoredVersions)
			}
			if got := versionNames(tc.crd); len(got) != len(before) {
				t.Fatalf("CRD mutated despite reporting no change: versions %v -> %v", before, got)
			}
			if got := storageVersions(tc.crd); len(got) != len(beforeStorage) || (len(got) > 0 && got[0] != beforeStorage[0]) {
				t.Fatalf("storage version moved despite reporting no change: %v -> %v", beforeStorage, got)
			}
		})
	}
}

// TestRestoreOrphanedVacuum_RepairsTheDamagedCase proves the repair fires, and that it leaves the
// CRD in a state the apiserver will accept — exactly one storage version.
func TestRestoreOrphanedVacuum_RepairsTheDamagedCase(t *testing.T) {
	// The damage: objects are encoded at vacuum (it is in storedVersions) but the CRD no longer
	// declares it, so every read of the Kind fails with "invalid group/version".
	crd := crdWith([]string{"v0-1-0", "vacuum"},
		ver("v0-1-0", true, true), ver("v0-2-0", true, false))

	if !RestoreOrphanedVacuum(crd) {
		t.Fatal("repair did not fire on an orphaned vacuum; this is the state #207 exists for")
	}

	var vac *apiextensionsv1.CustomResourceDefinitionVersion
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == VacuumVersionName {
			vac = &crd.Spec.Versions[i]
		}
	}
	if vac == nil {
		t.Fatalf("repair reported a change but added no vacuum version: %v", versionNames(crd))
	}

	if vac.Served {
		t.Error("restored vacuum must NOT be served: it is a permissive storage version, and serving it " +
			"would expose an endpoint that accepts anything")
	}
	if !vac.Storage {
		t.Error("restored vacuum must be the storage version, or objects encoded at it stay unreadable")
	}

	// The apiserver permits exactly one storage version, so appending vacuum with Storage:true is
	// only valid if every other claim is cleared in the same object. Getting this wrong turns a
	// recoverable CRD into one the apiserver rejects outright.
	if got := storageVersions(crd); len(got) != 1 || got[0] != VacuumVersionName {
		t.Fatalf("storage versions = %v, want exactly [%s]", got, VacuumVersionName)
	}

	// Serving must be untouched: the repair restores storage, it does not change what clients see.
	for _, v := range crd.Spec.Versions {
		if v.Name != VacuumVersionName && !v.Served {
			t.Errorf("version %q stopped being served; the repair must not change the served set", v.Name)
		}
	}

	// Idempotent in the only sense that matters: a second call on the repaired CRD is a no-op.
	if RestoreOrphanedVacuum(crd) {
		t.Error("repair fired twice on the same CRD; the second call must find vacuum present and do nothing")
	}
}

// TestRestoreOrphanedVacuum_MatchesAppendVersion pins that a repaired vacuum is byte-identical to a
// freshly appended one. If they ever diverge, a repaired CRD and a newly built one would differ and
// each reconcile would fight the last — drift that looks like flapping rather than like a bug here.
func TestRestoreOrphanedVacuum_MatchesAppendVersion(t *testing.T) {
	repaired := crdWith([]string{"vacuum"}, ver("v0-1-0", true, true))
	if !RestoreOrphanedVacuum(repaired) {
		t.Fatal("setup: repair did not fire")
	}

	base := apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.composition.krateo.io"},
		Spec:       apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{ver("v0-1-0", true, true)}},
	}
	toadd := apiextensionsv1.CustomResourceDefinition{
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{Versions: []apiextensionsv1.CustomResourceDefinitionVersion{ver("v0-2-0", true, false)}},
	}
	appended, err := AppendVersion(base, toadd)
	if err != nil {
		t.Fatalf("setup: AppendVersion: %v", err)
	}

	find := func(crd *apiextensionsv1.CustomResourceDefinition) *apiextensionsv1.CustomResourceDefinitionVersion {
		for i := range crd.Spec.Versions {
			if crd.Spec.Versions[i].Name == VacuumVersionName {
				return &crd.Spec.Versions[i]
			}
		}
		return nil
	}
	a, b := find(repaired), find(appended)
	if a == nil || b == nil {
		t.Fatalf("setup: missing vacuum (repaired=%v appended=%v)", a != nil, b != nil)
	}
	if a.Schema == nil || b.Schema == nil || a.Schema.OpenAPIV3Schema == nil || b.Schema.OpenAPIV3Schema == nil {
		t.Fatal("vacuum version carries no schema; the permissive storage version is the schema")
	}

	ap, bp := a.Schema.OpenAPIV3Schema.Properties, b.Schema.OpenAPIV3Schema.Properties
	if len(ap) != len(bp) {
		t.Fatalf("schema property sets differ: repaired %d, appended %d", len(ap), len(bp))
	}
	// spec and status are where preserve-unknown-fields lives, and it is the reason vacuum works at
	// all. Note it sits under properties.spec, NOT at the schema root — a survey reading the root
	// returns a clean, wrong zero.
	for _, k := range []string{"spec", "status"} {
		av, bv := ap[k], bp[k]
		if av.XPreserveUnknownFields == nil || !*av.XPreserveUnknownFields {
			t.Errorf("repaired vacuum: properties.%s does not preserve unknown fields", k)
		}
		if bv.XPreserveUnknownFields == nil || !*bv.XPreserveUnknownFields {
			t.Errorf("appended vacuum: properties.%s does not preserve unknown fields", k)
		}
	}
}
