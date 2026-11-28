package compositiondefinitions

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/core-provider/internal/tools/restactionrbac"
	"github.com/krateo-platformops/plumbing/apiref"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// maxParallelRBACCalls bounds how many /rbac requests one pass has in flight.
const maxParallelRBACCalls = 8

// restActionsGVR is where a RESTAction's generation is read, to key the cache of a resolution.
var restActionsGVR = schema.GroupVersionResource{Group: "templates.krateo.io", Version: "v1", Resource: "restactions"}

// notDetermined is a plan that could not be computed because something outside the definition failed:
// snowplow could not be asked or answered with a server error, or the compositions could not be
// listed. It is NOT an empty plan. Whoever receives it leaves every Role exactly as it is, and the
// pass is retried (#691: cannot determine is not empty).
type notDetermined struct{ err error }

func (e *notDetermined) Error() string {
	return "which namespaces need a Role is not determined: " + e.err.Error()
}
func (e *notDetermined) Unwrap() error { return e.err }

// classifyResolveError says what a failed /rbac call means. The 422 (a stage that cannot be
// enumerated) is kept as restactionrbac's StageUnresolvable error exactly as the definition-level
// resolution reports it: it is not a transient failure and it is not an empty read-set. All the
// handling of that status is in restactionrbac.fromResponse, one function.
func classifyResolveError(composition types.NamespacedName, err error) error {
	err = fmt.Errorf("composition %s: %w", composition, err)
	if e, ok := restactionrbac.AsError(err); ok {
		switch e.Cause {
		case restactionrbac.CauseSnowplowUnavailable, restactionrbac.CauseSnowplowError:
			return &notDetermined{err: err}
		}
		return err // a refusal with its own reason: ExtrasTooLarge, StageUnresolvable, NotFound, ...
	}
	return &notDetermined{err: err}
}

// apiRefPlan is a definition's resolved apiRef read-sets, one per composition.
type apiRefPlan struct {
	Compositions [][]restactionrbac.Resource
	digest       string
}

func (p *apiRefPlan) clusterWide() []restactionrbac.ClusterWide {
	return restactionrbac.Compose(nil, p.Compositions, "", "").ClusterWide
}

// apiRefPlanner resolves, for each composition of a definition, the read-set of its apiRef with that
// composition's extras (apiref.CompositionExtras, the builder the composition controller also sends
// to /call), and caches each answer. It is shared by the definition's reconciler, which grants the
// ClusterRole, and the Roles reconciler, so one composition is resolved once however many ask.
type apiRefPlanner struct {
	dyn dynamic.Interface
	// inspect is the /rbac client; a function so a test can point it at an httptest snowplow.
	inspect func() *restactionrbac.Client

	mu    sync.Mutex
	cache map[planKey][]restactionrbac.Resource
	// applied is the digest of the plan whose Roles were last applied, per definition.
	applied map[types.NamespacedName]string
	// kick asks the Roles reconciler to look at a definition again.
	kick func(types.NamespacedName)
}

// planKey is what a resolution depends on: the definition, the RESTAction and its generation, and the
// extras it is asked with (which carry the composition's name, namespace and UID).
type planKey struct {
	definition types.NamespacedName
	ref        types.NamespacedName
	generation int64
	extras     string
}

func newAPIRefPlanner(dyn dynamic.Interface, inspect func() *restactionrbac.Client) *apiRefPlanner {
	return &apiRefPlanner{dyn: dyn, inspect: inspect,
		cache: map[planKey][]restactionrbac.Resource{}, applied: map[types.NamespacedName]string{}}
}

// restActionGeneration is the RESTAction's generation, or 0 when it cannot be read: a resolution is
// then not cached, because nothing says when the RESTAction changed.
func (p *apiRefPlanner) restActionGeneration(ctx context.Context, ns, name string) int64 {
	u, err := p.dyn.Resource(restActionsGVR).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return 0
	}
	return u.GetGeneration()
}

// plan resolves every composition of cd at gvr. An error is either a *notDetermined or a refusal
// carrying its own restactionrbac cause; in neither case is any plan returned.
func (p *apiRefPlanner) plan(ctx context.Context, cd *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource) (*apiRefPlan, error) {
	if p.inspect == nil || SnowplowURL == "" {
		return nil, restactionrbac.NotConfigured()
	}
	static, err := decodeStaticExtras(cd)
	if err != nil {
		return nil, err
	}
	comps, err := getters.GetCompositions(ctx, p.dyn, gvr)
	if err != nil {
		return nil, &notDetermined{err: err}
	}
	def := client.ObjectKeyFromObject(cd)
	ref := types.NamespacedName{Namespace: apiRefNamespace(cd), Name: apiRefName(cd)}
	gen := p.restActionGeneration(ctx, ref.Namespace, ref.Name)
	cli := p.inspect()

	n := len(comps.Items)
	sets := make([][]restactionrbac.Resource, n)
	errs := make([]error, n)
	used := make([]planKey, n)
	sem := make(chan struct{}, maxParallelRBACCalls)
	var wg sync.WaitGroup
	for i := range comps.Items {
		c := &comps.Items[i]
		extras := apiref.CompositionExtras(static, c.GetName(), c.GetNamespace(), string(c.GetUID()))
		raw, merr := json.Marshal(extras) // sorted keys: a stable digest
		if merr != nil {
			return nil, merr
		}
		sum := sha256.Sum256(raw)
		key := planKey{definition: def, ref: ref, generation: gen, extras: hex.EncodeToString(sum[:])}
		used[i] = key
		if gen != 0 {
			p.mu.Lock()
			rows, hit := p.cache[key]
			p.mu.Unlock()
			if hit {
				sets[i] = rows
				continue
			}
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, key planKey, name types.NamespacedName, extras map[string]any) {
			defer wg.Done()
			defer func() { <-sem }()
			rows, err := cli.Inspect(ctx, restactionrbac.Params{APIRefName: ref.Name, APIRefNamespace: ref.Namespace, Extras: extras})
			if err != nil {
				errs[i] = classifyResolveError(name, err)
				return
			}
			sets[i] = rows
			if gen != 0 {
				p.mu.Lock()
				p.cache[key] = rows
				p.mu.Unlock()
			}
		}(i, key, client.ObjectKeyFromObject(c), extras)
	}
	wg.Wait()
	if err := firstError(errs); err != nil {
		return nil, err
	}

	// Entries of this definition no composition used this pass are gone with their compositions.
	live := map[planKey]bool{}
	for _, k := range used {
		live[k] = true
	}
	p.mu.Lock()
	for k := range p.cache {
		if k.definition == def && !live[k] {
			delete(p.cache, k)
		}
	}
	p.mu.Unlock()

	out := &apiRefPlan{Compositions: sets}
	out.digest = planDigest(sets)
	return out, nil
}

// firstError returns the most telling failure: a refusal with its own reason before a transient one,
// so the condition names what an operator can act on.
func firstError(errs []error) error {
	var transient error
	for _, e := range errs {
		if e == nil {
			continue
		}
		var nd *notDetermined
		if !errors.As(e, &nd) {
			return e
		}
		if transient == nil {
			transient = e
		}
	}
	return transient
}

// planDigest is a stable digest of what the plan grants: the resolved rows, order-independent.
func planDigest(sets [][]restactionrbac.Resource) string {
	var lines []string
	for _, set := range sets {
		for _, r := range set {
			b, _ := json.Marshal(r)
			lines = append(lines, string(b))
		}
	}
	sort.Strings(lines)
	h := sha256.New()
	for _, l := range lines {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// namespaces is the sorted set of namespaces a plan's concrete, grantable rows land in.
func (p *apiRefPlan) namespaces() []string {
	return restactionrbac.Compose(nil, p.Compositions, "g", "n").Namespaces()
}

func (p *apiRefPlanner) markApplied(def types.NamespacedName, digest string) {
	p.mu.Lock()
	p.applied[def] = digest
	p.mu.Unlock()
}

// noteObserved asks the Roles reconciler to look at def again when the plan the definition's own pass
// just computed is not the one whose Roles were last applied (a RESTAction or composition changed
// since). The Roles reconciler is event-driven; this is how a change nothing watches reaches it.
func (p *apiRefPlanner) noteObserved(def types.NamespacedName, plan *apiRefPlan) {
	p.mu.Lock()
	same := p.applied[def] == plan.digest
	p.mu.Unlock()
	if !same && p.kick != nil {
		p.kick(def)
	}
}

// decodeStaticExtras is the definition's apiRef.extras as a map.
func decodeStaticExtras(cd *compositiondefinitionsv1alpha1.CompositionDefinition) (map[string]any, error) {
	s := encodeApiRefExtras(cd)
	if s == "" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil, fmt.Errorf("decoding apiRef extras: %w", err)
	}
	return m, nil
}

// usesPerNamespaceRBAC reports whether cd's apiRef reads are granted per namespace: a definition with
// an apiRef on the management cluster. A remote definition's compositions are on another cluster.
func usesPerNamespaceRBAC(cd *compositiondefinitionsv1alpha1.CompositionDefinition, remote bool) bool {
	return apiRefName(cd) != "" && !remote
}

// inspector is the production /rbac client.
func inspector() *restactionrbac.Client { return deploy.ApiRefInspector(SnowplowURL, AuthnURL) }
