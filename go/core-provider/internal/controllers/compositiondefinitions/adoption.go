package compositiondefinitions

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/clusterkube"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// Orphaned instances and their adoption (#215).
//
// An instance names the definition that owns it in its definition-ref labels
// (krateo.io/composition-definition-name/-namespace). Everything per-definition selects by them:
// the version migration, and Observe's check that drives it. A hand-over (controllerowner.go)
// moves a Kind+version's controller to another definition and deletes the original, but leaves
// the instances alone, so afterwards their labels name a definition that no longer exists. No
// definition considers them its own. The next chart bump of the surviving definition then retired
// the old version's controller, migrated nothing (its owner selector matched none of them) and
// reported up to date: the instances stayed on the old version label with no controller selecting
// them, Ready, and no longer reconciled.
//
// So the definition that inherits the Kind inherits the instances: an instance whose labels name
// a definition that does not exist is adopted, i.e. its labels are re-pointed at the definition
// adopterFor elects. Adoption runs in Update before old-version controllers are retired and before
// the migration, which then moves the adopted instances like any others; Observe reports not up to
// date while an adoptable orphan exists, which is what heals a cluster where the hand-over has
// already happened.
//
// Every read here is LIVE, never from an informer cache. "Does not exist" is the one judgement
// that must not come from a view that may lag: a definition created a moment ago that a cache has
// not seen yet would make its own, correctly labelled, instances look orphaned and hand them to
// another definition. The same holds for the election: an owner that has not reconciled yet has
// no status, so it is also recognised from its spec (see targetsOrServed).

// definitionKey identifies a definition by namespace/name.
type definitionKey struct{ namespace, name string }

func keyOf(cd *compositiondefinitionsv1alpha1.CompositionDefinition) definitionKey {
	return definitionKey{namespace: cd.GetNamespace(), name: cd.GetName()}
}

// kindCharts are the charts (url and repo, without the version) known to generate a Kind,
// gathered from the status of every definition that has served it. With them a definition that
// has not reconciled yet is recognised from its spec alone: the generated Kind is a function of
// the chart, and the generated version a function of the chart's version (crdVersion).
type kindCharts map[chartRef]bool

func chartsOfKind(cds []compositiondefinitionsv1alpha1.CompositionDefinition, gk schema.GroupKind) kindCharts {
	out := kindCharts{}
	for i := range cds {
		m := cds[i].Status.Managed
		if m.Group != gk.Group || m.Kind != gk.Kind {
			continue
		}
		for _, vi := range m.VersionInfo {
			if vi.Chart != nil {
				out[chartRef{url: vi.Chart.Url, repo: vi.Chart.Repo}] = true
			}
		}
	}
	return out
}

// crdVersion is the generated CRD version for a chart version ("0.2.45" -> "v0-2-45"), as chartfs
// derives it.
func crdVersion(chartVersion string) string {
	return "v" + strings.ReplaceAll(chartVersion, ".", "-")
}

// specOfKind reports whether cd's spec names a chart known to generate the Kind.
//
// The known set comes from live definitions' status, so the spec clause leans on status one level
// removed, and that is sound for the case it exists for. It matters only when an unreconciled
// definition would otherwise lose the election to a reconciled one of the same Kind, and that
// reconciled definition's own versionInfo lists the chart's url and repo (they do not change with
// the version), so the set is populated exactly when it is needed. When no live definition of the
// Kind has reconciled, the set is empty and NOBODY is elected: adoption waits for the first one to
// reconcile, it never goes to a wrong definition. The residual gap is the same as targetsVersion's:
// the same chart under another url or repo (a mirror) is not recognised.
func specOfKind(cd *compositiondefinitionsv1alpha1.CompositionDefinition, known kindCharts) bool {
	c := cd.Spec.Chart
	return c != nil && c.Url != "" && known[chartRef{url: c.Url, repo: c.Repo}]
}

// targetsOrServed reports whether cd is, or has been, on gk at version: its status says so, its
// managed versions include it, or its spec names a chart of the Kind at that version. The spec
// clause is what sees an owner that has not reconciled yet (#210's window): matching on status
// alone, the election would pass it over and hand its instances to another definition.
//
// The managed versions are what keep the election stable through a bump. The definition being
// bumped has status v<old> until its Update completes and v<new> after, and Update adopts BEFORE
// that; on status alone it would stop "targeting" the orphans' version halfway through its own
// reconcile and the election would fall through to a different branch depending on when it is
// asked. It lists v<old> among its managed versions on both sides of that write.
func targetsOrServed(cd *compositiondefinitionsv1alpha1.CompositionDefinition, gk schema.GroupKind, version string, known kindCharts) bool {
	if cd.Status.CurrentGVK() == gk.WithVersion(version) {
		return true
	}
	if m := cd.Status.Managed; m.Group == gk.Group && m.Kind == gk.Kind {
		for _, vi := range m.VersionInfo {
			if vi.Version == version {
				return true
			}
		}
	}
	return specOfKind(cd, known) && crdVersion(cd.Spec.Chart.Version) == version
}

// ofKind reports whether cd generates gk at any version, by status or by spec.
func ofKind(cd *compositiondefinitionsv1alpha1.CompositionDefinition, gk schema.GroupKind, known kindCharts) bool {
	if m := cd.Status.Managed; m.Group == gk.Group && m.Kind == gk.Kind {
		return true
	}
	if g := cd.Status.CurrentGVK(); g.Group == gk.Group && g.Kind == gk.Kind {
		return true
	}
	return specOfKind(cd, known)
}

// oldestLiveLocal returns the oldest definition in cds matching keep that is not being deleted and
// not remote, or nil.
func oldestLiveLocal(cds []compositiondefinitionsv1alpha1.CompositionDefinition, keep func(*compositiondefinitionsv1alpha1.CompositionDefinition) bool) *compositiondefinitionsv1alpha1.CompositionDefinition {
	var oldest *compositiondefinitionsv1alpha1.CompositionDefinition
	for i := range cds {
		cd := &cds[i]
		if cd.GetDeletionTimestamp() != nil || clusterkube.IsRemote(cd.Spec.Deploy) || !keep(cd) {
			continue
		}
		if oldest == nil || olderDefinition(cd, oldest) {
			oldest = cd
		}
	}
	return oldest
}

// adopterFor elects the definition that adopts an orphaned instance of gk labelled version: the
// oldest live local definition that targets or has served that version (the one that owns, or
// owned, its controller), else the oldest live local definition of the Kind, whose migration then
// moves the instance onto a version it serves. nil when no live local definition has the Kind.
//
// Every definition computes the same answer from the same live list, so exactly one of them
// writes; adopt makes the write conditional besides, so a definition acting on a stale view
// loses the race instead of overwriting the winner.
func adopterFor(cds []compositiondefinitionsv1alpha1.CompositionDefinition, gk schema.GroupKind, version string) *compositiondefinitionsv1alpha1.CompositionDefinition {
	known := chartsOfKind(cds, gk)
	if a := oldestLiveLocal(cds, func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
		return targetsOrServed(cd, gk, version, known)
	}); a != nil {
		return a
	}
	return oldestLiveLocal(cds, func(cd *compositiondefinitionsv1alpha1.CompositionDefinition) bool {
		return ofKind(cd, gk, known)
	})
}

// orphanedBy returns the instances that adopter adopts: those whose definition-ref labels name a
// definition absent from cds (definitions being deleted are present: they hand over first), whose
// version label names a version in served, and for which adopterFor elects adopter.
//
// An instance with no definition-ref labels, or no usable version label, is not an orphan in this
// sense; the served-version prune accounts for those (getters.GetUnaccountedCompositions).
func orphanedBy(adopter *compositiondefinitionsv1alpha1.CompositionDefinition, cds []compositiondefinitionsv1alpha1.CompositionDefinition, instances []unstructured.Unstructured, gk schema.GroupKind, served map[string]bool) []unstructured.Unstructured {
	exists := make(map[definitionKey]bool, len(cds))
	for i := range cds {
		exists[keyOf(&cds[i])] = true
	}
	var out []unstructured.Unstructured
	for _, u := range instances {
		lbl := u.GetLabels()
		ref := definitionKey{namespace: lbl[deploy.CompositionDefinitionNamespaceLabel], name: lbl[deploy.CompositionDefinitionNameLabel]}
		version := lbl[deploy.CompositionVersionLabel]
		if ref.name == "" || ref.namespace == "" || exists[ref] || !served[version] {
			continue
		}
		if a := adopterFor(cds, gk, version); a != nil && keyOf(a) == keyOf(adopter) {
			out = append(out, u)
		}
	}
	return out
}

var compositionDefinitionsGVR = schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}

// liveDefinitions lists every CompositionDefinition straight from the apiserver.
func (e *external) liveDefinitions(ctx context.Context) ([]compositiondefinitionsv1alpha1.CompositionDefinition, error) {
	ul, err := e.mgmtDynamic.Resource(compositionDefinitionsGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing CompositionDefinitions: %w", err)
	}
	out := make([]compositiondefinitionsv1alpha1.CompositionDefinition, len(ul.Items))
	for i := range ul.Items {
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(ul.Items[i].Object, &out[i]); err != nil {
			return nil, fmt.Errorf("decoding CompositionDefinition %s/%s: %w", ul.Items[i].GetNamespace(), ul.Items[i].GetName(), err)
		}
	}
	return out, nil
}

// adoptableOrphans returns the orphaned instances of gvr's Kind that cr adopts. It is the single
// predicate behind both Observe's drive and Update's write, so Observe never asks for an adoption
// Update would decline (no Observe<->Update ping-pong).
func (e *external) adoptableOrphans(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	if clusterkube.IsRemote(cr.Spec.Deploy) {
		return nil, nil // a remote definition's instances live on its spoke, with a dedicated controller
	}
	crd, err := crdclient.Get(ctx, e.kube, gvr.GroupResource())
	if err != nil {
		return nil, fmt.Errorf("fetching CRD: %w", err)
	}
	if crd == nil {
		return nil, nil
	}
	served := map[string]bool{}
	for _, v := range crd.Spec.Versions {
		if v.Served {
			served[v.Name] = true
		}
	}
	cds, err := e.liveDefinitions(ctx)
	if err != nil {
		return nil, err
	}
	instances, err := e.dynamic.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing compositions: %w", err)
	}
	return orphanedBy(cr, cds, instances.Items, gvk.GroupKind(), served), nil
}

// adoptOrphans re-points cr's adoptable orphans' definition-ref labels at cr.
//
// The write goes through the endpoint of the instance's OWN version label, which is the version it
// is encoded at, and that is load-bearing. The apiserver prunes against the REQUEST version's
// schema on writes and on reads, metadata-only patches included: through a narrower version this
// label change would silently drop every spec field that version does not declare (#188). Writing
// at the encoded version loses nothing. It also leaves the composition-version label as it is,
// since the policy stamps the request version; moving the instance to another version is the
// migration's job, with its field-loss detection, not this one's. Do not "simplify" this to the
// current served version.
//
// The patch carries the resourceVersion that was read, so it applies only to the object that was
// judged orphaned: if anything changed it in between, it is a conflict, skipped here and judged
// afresh on the next reconcile.
func (e *external) adoptOrphans(ctx context.Context, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvk schema.GroupVersionKind, gvr schema.GroupVersionResource, log logging.Logger) error {
	orphans, err := e.adoptableOrphans(ctx, cr, gvk, gvr)
	if err != nil {
		return fmt.Errorf("finding orphaned compositions: %w", err)
	}
	for _, u := range orphans {
		lbl := u.GetLabels()
		from := lbl[deploy.CompositionDefinitionNamespaceLabel] + "/" + lbl[deploy.CompositionDefinitionNameLabel]
		version := lbl[deploy.CompositionVersionLabel]
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"resourceVersion": u.GetResourceVersion(),
			"labels": map[string]string{
				deploy.CompositionDefinitionNameLabel:      cr.GetName(),
				deploy.CompositionDefinitionNamespaceLabel: cr.GetNamespace(),
			},
		}})
		if err != nil {
			return err
		}
		at := gvr
		at.Version = version
		_, err = e.dynamic.Resource(at).Namespace(u.GetNamespace()).Patch(ctx, u.GetName(), types.MergePatchType, patch, metav1.PatchOptions{})
		switch {
		case apierrors.IsConflict(err) || apierrors.IsNotFound(err):
			log.Info("Orphaned composition changed before adoption; re-evaluating next reconcile",
				"composition", u.GetNamespace()+"/"+u.GetName(), "error", err.Error())
		case err != nil:
			return fmt.Errorf("adopting composition %s/%s: %w", u.GetNamespace(), u.GetName(), err)
		default:
			log.Info("Adopted orphaned composition",
				"composition", u.GetNamespace()+"/"+u.GetName(), "from", from, "version", version)
		}
	}
	return nil
}
