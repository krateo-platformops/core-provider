package compositiondefinitions

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis"
	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	"github.com/krateo-platformops/provider-runtime/pkg/resource/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The hand-over (compositiondefinitions.go, "A definition being deleted while another live definition
// takes its Kind+version over") reports ResourceExists=false so that provider-runtime removes the
// finalizer WITHOUT calling Delete: the CRD, the instances and the shared RBAC belong to the successor,
// and Delete would tear them down. That is provider-runtime's contract, not core-provider's code, so it
// is pinned here against the runtime core-provider actually pins (Diego, 2026-11-21: delete-on-absent
// stays per runtime; provider-runtime removes the finalizer). The real reconciler runs over a real
// CompositionDefinition; only the external client is faked.
//
// A differential: absent must release with ZERO Delete calls; the CONTROL, present, must reach Delete,
// so the zero is not an instrument that never sees one.
func TestHandover_RuntimeReleasesWithoutDeleteWhenObserveReportsAbsent(t *testing.T) {
	for _, tc := range []struct {
		name         string
		exists       bool
		wantReleased bool
		wantDeletes  int
	}{
		{"handed over: Observe reports absent, finalizer released, no Delete", false, true, 0},
		{"control: Observe reports present, Delete runs", true, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := runtime.NewScheme()
			if err := apis.AddToScheme(s); err != nil {
				t.Fatal(err)
			}
			now := metav1.NewTime(time.Now())
			cd := &compositiondefinitionsv1alpha1.CompositionDefinition{
				ObjectMeta: metav1.ObjectMeta{
					Name: "leaving", Namespace: "ns",
					// A second finalizer keeps the object readable after ours is removed: the fake client
					// deletes an object whose last finalizer goes.
					Finalizers:        []string{reconciler.FinalizerName, "test.krateo.io/hold"},
					DeletionTimestamp: &now,
				},
			}
			cli := clientfake.NewClientBuilder().WithScheme(s).WithObjects(cd).WithStatusSubresource(cd).Build()
			deletes := 0
			r := reconciler.NewReconciler(&fake.Manager{Client: cli, Scheme: s},
				resource.ManagedKind(compositiondefinitionsv1alpha1.CompositionDefinitionGroupVersionKind),
				reconciler.WithExternalConnecter(reconciler.ExternalConnectorFn(func(context.Context, resource.Managed) (reconciler.ExternalClient, error) {
					return reconciler.ExternalClientFns{
						ObserveFn: func(context.Context, resource.Managed) (reconciler.ExternalObservation, error) {
							return reconciler.ExternalObservation{ResourceExists: tc.exists, ResourceUpToDate: true}, nil
						},
						DeleteFn: func(context.Context, resource.Managed) error { deletes++; return nil },
					}, nil
				})),
			)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "ns", Name: "leaving"}}); err != nil {
				t.Logf("Reconcile returned %v", err)
			}
			got := &compositiondefinitionsv1alpha1.CompositionDefinition{}
			if err := cli.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "leaving"}, got); err != nil {
				t.Fatalf("premise: the second finalizer keeps the object readable: %v", err)
			}
			released := !slices.Contains(got.GetFinalizers(), reconciler.FinalizerName)
			if released != tc.wantReleased || deletes != tc.wantDeletes {
				t.Fatalf("finalizer released=%v (want %v), Delete calls=%d (want %d); finalizers now %v",
					released, tc.wantReleased, deletes, tc.wantDeletes, got.GetFinalizers())
			}
		})
	}
}
