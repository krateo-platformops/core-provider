package crd

import (
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func establishedCRD(versions ...apiextensionsv1.CustomResourceDefinitionVersion) *apiextensionsv1.CustomResourceDefinition {
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "kubernetesclusters.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group:    "composition.krateo.io",
			Names:    apiextensionsv1.CustomResourceDefinitionNames{Plural: "kubernetesclusters"},
			Versions: versions,
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
				{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
			},
		},
	}
}

func version(name string, served bool) apiextensionsv1.CustomResourceDefinitionVersion {
	return apiextensionsv1.CustomResourceDefinitionVersion{Name: name, Served: served, Storage: served}
}

// Cases span the INPUT STATES, not the arms. The states a version bump can actually be in are:
// the version is served; the version has not appeared yet (our apply has not landed); the version
// is there and served=false (the apiserver has it and will not answer); the CRD is not Established;
// the CRD could not be read at all. Enumerating arms instead of input states is how #273's digest
// check ended up unreachable for the exact population it was written for.
func TestServedVersionDisagreement_SpansTheInputStates(t *testing.T) {
	cases := []struct {
		name     string
		crd      *apiextensionsv1.CustomResourceDefinition
		want     string // "" means agreement
		mentions string // required substring when a disagreement is expected
	}{
		{
			name: "the version is served — agreement",
			crd:  establishedCRD(version("v0-13-8", true)),
			want: "",
		},
		{
			// THE ARM THAT MATTERS. This is the state at a version-bump instant: the CRD exists and
			// is Established from the PREVIOUS version, and the new one is not there yet.
			name:     "the new version is absent — our apply has not landed",
			crd:      establishedCRD(version("v0-13-7", true)),
			mentions: "absent from spec.versions",
		},
		{
			name:     "the version is present but served=false",
			crd:      establishedCRD(version("v0-13-8", false)),
			mentions: "served=false",
		},
		{
			name: "not Established",
			crd: func() *apiextensionsv1.CustomResourceDefinition {
				c := establishedCRD(version("v0-13-8", true))
				c.Status.Conditions = nil
				return c
			}(),
			mentions: "not Established",
		},
		{
			// "Could not determine" must not read as "nothing to do": the caller's false answer
			// prunes live resources.
			name:     "a nil CRD is not agreement",
			crd:      nil,
			mentions: "could not be read",
		},
	}

	if len(cases) == 0 {
		t.Fatal("no cases — every assertion below would pass over an empty set")
	}

	for _, c := range cases {
		got := ServedVersionDisagreement(c.crd, "v0-13-8")
		if c.want == "" && c.mentions == "" {
			if got != "" {
				t.Errorf("%s: expected agreement, got disagreement %q", c.name, got)
			}
			continue
		}
		if got == "" {
			t.Errorf("%s: expected a disagreement mentioning %q, got agreement — a false "+
				"agreement here is what lets a bump proceed while the apiserver cannot answer "+
				"on the version, and installer's umbrella prunes live compositions on that answer",
				c.name, c.mentions)
			continue
		}
		if !strings.Contains(got, c.mentions) {
			t.Errorf("%s: disagreement %q does not mention %q. The reason has to name WHICH of the "+
				"failures it is: 'our apply has not landed' and 'the apiserver will not serve it' "+
				"are different problems and a timeout reports them identically", c.name, got, c.mentions)
		}
	}
}

// THE DIFFERENTIAL, and it is the whole claim that this is a fix rather than a second opinion.
//
// IsReady returns TRUE for the two states above that matter -- version absent, and version present
// but served=false -- because Established is already True and says nothing about versions. So the
// predicate the update path used to wait on was satisfied in exactly the states the wait existed to
// exclude. Without this arm, the table above would pass just as well against a world in which
// IsReady had always been sufficient, and would prove nothing about the change.
func TestIsReadyIsSatisfiedInTheStatesServedVersionRejects(t *testing.T) {
	vacuous := map[string]*apiextensionsv1.CustomResourceDefinition{
		"the new version is absent":        establishedCRD(version("v0-13-7", true)),
		"the version is present, unserved": establishedCRD(version("v0-13-8", false)),
	}

	if len(vacuous) == 0 {
		t.Fatal("no inputs — the differential would be vacuous")
	}

	for name, crd := range vacuous {
		if !IsReady(crd) {
			t.Errorf("%s: IsReady is FALSE here, so this input does not demonstrate the gap and "+
				"the differential is not measuring what it claims", name)
			continue
		}
		if ServedVersionDisagreement(crd, "v0-13-8") == "" {
			t.Errorf("%s: IsReady and ServedVersionDisagreement agree, so the new predicate "+
				"changes nothing for this state and the update path's wait is still vacuous", name)
		}
	}

	// AND THE OTHER DIRECTION: the two must agree where there is nothing wrong, or the new
	// predicate is simply stricter everywhere and would wedge the create path rather than fix the
	// update path.
	ok := establishedCRD(version("v0-13-8", true))
	if !IsReady(ok) || ServedVersionDisagreement(ok, "v0-13-8") != "" {
		t.Error("the two predicates disagree on a healthy CRD serving the version, which would " +
			"make every reconcile wait for a condition that is already satisfied")
	}
}
