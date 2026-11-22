package compositiondefinitions

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// core-provider#616 against Observe: status.owner records the owner election that Observe makes on
// every pass. A follower carries its owner's namespace/name; the owner, a lone definition, and a
// follower that has just become the owner carry none.
//
// Each case asserts REACH before asserting the field, because Observe has many earlier returns and
// a field left unset by an early return would pass a "no owner" assertion vacuously:
//   - follower: ValuesSchemaAuthored=Unknown/OwnedByAnotherDefinition is set on the line after the
//     field, and only on that branch;
//   - owner: Ready is seeded with a follower's message, which readyOnBecomingOwner replaces only once
//     execution passes the election (#387).
func TestObserve_616_StatusOwnerFollowsTheElection(t *testing.T) {
	const ns = "krateo-system"
	tgz := versionedChartTGZ(t, "1.0.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)

	crd := &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "widgets.composition.krateo.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "composition.krateo.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Kind: "Widget", Plural: "widgets"},
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{
				{Name: "v1-0-0", Served: true, Storage: true},
			},
		},
		Status: apiextensionsv1.CustomResourceDefinitionStatus{
			Conditions: []apiextensionsv1.CustomResourceDefinitionCondition{
				{Type: apiextensionsv1.Established, Status: apiextensionsv1.ConditionTrue},
				{Type: apiextensionsv1.NamesAccepted, Status: apiextensionsv1.ConditionTrue},
			},
			StoredVersions: []string{"v1-0-0"},
		},
	}
	newCD := func(name string, created time.Time) *compositiondefinitionsv1alpha1.CompositionDefinition {
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		cd.SetName(name)
		cd.SetNamespace(ns)
		cd.SetCreationTimestamp(metav1.NewTime(created))
		cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: srv.URL + "/widget.tgz", Version: "1.0.0"}
		// GetCompositionDefinitionsWithVersion matches definitions by their status' GVK.
		cd.Status.ApiVersion = "composition.krateo.io/v1-0-0"
		cd.Status.Kind = "Widget"
		cd.Status.Resource = "widgets"
		return cd
	}
	observe := func(t *testing.T, self *compositiondefinitionsv1alpha1.CompositionDefinition, others ...client.Object) {
		t.Helper()
		objs := append([]client.Object{self, crd}, others...)
		kc := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(objs...).WithStatusSubresource(self).Build()
		e := &external{
			kube: kc, mgmt: kc, apiReader: kc,
			mgmtDynamic: dynfake.NewSimpleDynamicClient(scheme(t)),
			client:      k8sfake.NewSimpleClientset(),
			log:         logging.NewNopLogger(),
			pluralizer:  notFoundPluralizer{},
		}
		// Later steps (the follower's own observation, the owner's dry-run deploy) may fail in this
		// reduced fixture; the field is decided before them, and each case asserts reach.
		_, _ = e.Observe(context.Background(), self)
	}

	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := older.Add(time.Hour)

	t.Run("a follower records its owner", func(t *testing.T) {
		owner, follower := newCD("widget-a", older), newCD("widget-b", newer)
		observe(t, follower, owner)
		if !cdreason.ValuesSchemaOwnedElsewhere.Is(follower.GetCondition(cdreason.TypeValuesSchemaAuthored)) {
			t.Fatalf("premise: Observe did not reach the follower branch (ValuesSchemaAuthored = %+v)", follower.GetCondition(cdreason.TypeValuesSchemaAuthored))
		}
		want := compositiondefinitionsv1alpha1.DefinitionReference{Namespace: ns, Name: "widget-a"}
		if follower.Status.Owner == nil || *follower.Status.Owner != want {
			t.Fatalf("follower status.owner = %+v, want %+v", follower.Status.Owner, want)
		}
	})

	ownerCase := func(t *testing.T, self *compositiondefinitionsv1alpha1.CompositionDefinition, others ...client.Object) {
		t.Helper()
		// A follower's Ready, which readyOnBecomingOwner replaces once the election is passed.
		self.SetConditions(rtv1.Condition{Type: rtv1.TypeReady, Status: "True", Reason: rtv1.ReasonAvailable,
			Message: "composition controller for composition.krateo.io/v1-0-0, Resource=widgets" + followerMarker + ns + "/gone"})
		// A stale owner from an earlier election, which the owner path must clear.
		self.Status.Owner = &compositiondefinitionsv1alpha1.DefinitionReference{Namespace: ns, Name: "gone"}
		observe(t, self, others...)
		if strings.Contains(self.GetCondition(rtv1.TypeReady).Message, followerMarker) {
			t.Fatalf("premise: Observe did not pass the election (Ready still names an owner: %q)", self.GetCondition(rtv1.TypeReady).Message)
		}
		if self.Status.Owner != nil {
			t.Fatalf("status.owner = %+v on a definition that owns its Kind x version, want absent", self.Status.Owner)
		}
	}

	t.Run("the owner records none, even with a follower", func(t *testing.T) {
		owner, follower := newCD("widget-a", older), newCD("widget-b", newer)
		ownerCase(t, owner, follower)
	})

	t.Run("a lone definition records none", func(t *testing.T) {
		ownerCase(t, newCD("widget-a", older))
	})

	t.Run("hand-over: the owner is gone, the former follower becomes the owner and clears it", func(t *testing.T) {
		// Same object, two passes: first as a follower (field set), then with the owner deleted.
		owner, follower := newCD("widget-a", older), newCD("widget-b", newer)
		observe(t, follower, owner)
		if follower.Status.Owner == nil {
			t.Fatalf("premise: the first pass did not record the owner")
		}
		follower.SetConditions(rtv1.Condition{Type: rtv1.TypeReady, Status: "True", Reason: rtv1.ReasonAvailable,
			Message: "composition controller for composition.krateo.io/v1-0-0, Resource=widgets" + followerMarker + ns + "/widget-a"})
		observe(t, follower)
		if strings.Contains(follower.GetCondition(rtv1.TypeReady).Message, followerMarker) {
			t.Fatalf("premise: the second pass did not pass the election (Ready: %q)", follower.GetCondition(rtv1.TypeReady).Message)
		}
		if follower.Status.Owner != nil {
			t.Fatalf("status.owner = %+v after becoming the owner, want cleared", follower.Status.Owner)
		}
	})
}
