package composition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/rbac"
	helmutils "github.com/krateo-platformops/plumbing/helm/utils"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
)

// #697. Every Observe asked chart-inspector for the composition's derived RBAC, and chart-inspector
// answers by a helm Install(DryRunServer) that GETs every rendered object: one GET per child per pass
// (60 s), measured at ~16/s on an idle cluster, plus a full chart render, to recompute an answer that
// had not changed.
//
// The cache holds, per composition UID, the rules last computed and the key they were computed under.
// A pass whose key is unchanged does NOT call chart-inspector; it re-applies the cached rules exactly
// as a fresh answer would be applied (rbac.ApplyRBAC is idempotent), so a deleted or edited grant is
// still restored on that pass. A pass whose key differs calls chart-inspector as before, and a failure
// of that call behaves as it always did: nothing stale is applied for a changed key.
//
// THE KEY IS EVERY INPUT OF chart-inspector's ANSWER THAT THE cdc CAN SEE, deliberately wide: a key
// too wide costs one extra call, one too narrow grants the wrong RBAC. See derivedRBACKeyFor for each
// input and where chart-inspector reads it.
//
// TWO INPUTS ARE NOT IN THE KEY BECAUSE THEY ARE EXACT ELSEWHERE:
//
//   - A chart's `lookup`. The render is a server-side dry run, so a template that calls `lookup` reads
//     cluster state nothing in the key covers. A composition whose chart (or any subchart, helper or
//     default values file) mentions `lookup` is NEVER cached: every pass asks. See chartLookupScanner.
//   - chart-inspector's own version, which is in the key (inspectorVersions).
//
// COMPOSITION_CONTROLLER_DERIVED_RBAC_CACHE=false turns the cache off.

const derivedRBACCacheEnvvar = "COMPOSITION_CONTROLLER_DERIVED_RBAC_CACHE"

// derivedRBACVersionProbeEvery is how often the cdc asks chart-inspector for its version when no render
// has told it: an operational throttle on a request that costs no render, not a bound on correctness.
// An upgraded inspector is noticed within this interval, and by the first call that reaches it sooner.
const derivedRBACVersionProbeEvery = 15 * time.Second

// The reasons a pass is counted under (composition_rbac_generation_decisions_total{reason}).
const (
	rbacDecisionSkipped    = "skipped"     // unchanged key: cached rules re-applied, chart-inspector not asked
	rbacDecisionLookup     = "lookup"      // the chart calls `lookup`: never cached, asked
	rbacDecisionKeyChanged = "key_changed" // no entry or a different key: asked
	rbacDecisionUnscanned  = "unscannable" // the key or the chart could not be read: asked, never assumed
)

// derivedRBACKey is comparable: two keys are the same computation exactly when they are ==.
type derivedRBACKey struct {
	// Which composition. The cache is also indexed by UID; these are in the key so a UID reused in a
	// test, or an object recreated under one UID by a restore, can never match across kinds or names.
	GVK       string
	Namespace string
	Name      string
	UID       types.UID

	// The helm release this derives for, and its current revision. The revision moves on every
	// install, upgrade and rollback, i.e. whenever the chart's children change under the release,
	// which is what a template `lookup` of them would see.
	ReleaseName string
	Revision    int

	// The chart. What this controller renders (#461), and what chart-inspector renders (the
	// definition's CURRENT spec.chart) are not always the same chart, so both are keyed.
	ChartURL          string
	ChartVersion      string
	ChartRepo         string
	DefinitionVersion string
	InsecureSkipTLS   bool
	AuthDigest        string // sha256 of username+password; the secret itself is never kept

	// The definition chart-inspector fetches: identity, and its generation, which moves with every
	// spec change (chart url, repo, version, credentials reference, TLS setting).
	DefinitionName       string
	DefinitionNamespace  string
	DefinitionGVR        string
	DefinitionGeneration int64
	DefinitionUID        types.UID

	// The values chart-inspector renders with: ValuesFromSpec + InjectGlobalValues of the composition
	// it reads, the same two calls the cdc makes, digested whole.
	ValuesDigest    string
	KrateoNamespace string

	// Who the generated bindings subject, and which chart-inspector answered.
	SAName       string
	SANamespace  string
	InspectorURL string
	// InspectorVersion is the build version chart-inspector last reported; "" for one that sends none.
	InspectorVersion string
}

type derivedRBACEntry struct {
	key   derivedRBACKey
	rules *rbac.RBAC
}

// derivedRBACCache is safe for concurrent use: a pooled process runs several compositions at once.
// A nil cache, or one built disabled, never hits and never stores.
type derivedRBACCache struct {
	mu      sync.Mutex
	entries map[types.UID]derivedRBACEntry
	on      bool
	// versions learns chart-inspector's build version. nil: every version reads "".
	versions *inspectorVersions
	// lookups says whether a chart calls `lookup`. nil: nothing is known, so nothing is cached.
	lookups *chartLookupScanner
}

// newDerivedRBACCache returns a cache; enabled false makes it a no-op.
func newDerivedRBACCache(enabled bool) *derivedRBACCache {
	return &derivedRBACCache{entries: map[types.UID]derivedRBACEntry{}, on: enabled}
}

// newProductionDerivedRBACCache is the cache NewHandler installs: the real chart scanner and a
// version probe throttled to derivedRBACVersionProbeEvery.
func newProductionDerivedRBACCache(enabled bool) *derivedRBACCache {
	c := newDerivedRBACCache(enabled)
	c.versions = &inspectorVersions{every: derivedRBACVersionProbeEvery}
	c.lookups = newChartLookupScanner()
	return c
}

func (c *derivedRBACCache) enabled() bool { return c != nil && c.on }

// observeVersion records the chart-inspector version a render response carried. Safe on a nil cache.
func (c *derivedRBACCache) observeVersion(v string) {
	if c == nil {
		return
	}
	c.versions.observe(v)
}

// lookup returns a PRIVATE copy of the cached rules when the entry for uid was computed under exactly
// key.
func (c *derivedRBACCache) lookup(uid types.UID, key derivedRBACKey) (*rbac.RBAC, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[uid]
	if !ok || e.key != key {
		return nil, false
	}
	return e.rules.DeepCopy(), true
}

func (c *derivedRBACCache) store(uid types.UID, key derivedRBACKey, rules *rbac.RBAC) {
	if !c.enabled() || rules == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[uid] = derivedRBACEntry{key: key, rules: rules.DeepCopy()}
}

// forget drops uid's entry: the composition is gone, or its last computation failed.
func (c *derivedRBACCache) forget(uid types.UID) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, uid)
}

func (c *derivedRBACCache) size() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// derivedRBACKeyFor builds the key for this pass. A non-empty reason (an rbacDecision* constant) means
// the pass is not cacheable, and the caller then asks chart-inspector: not knowing is never a reason
// to apply cached rules.
//
// Inputs, and where chart-inspector (go/chart-inspector/internal/handlers/resources/get/resources.go)
// reads each:
//   - composition name, namespace, GVK, UID: the query parameters, and the live composition it GETs.
//   - releaseName: the query parameter it installs under.
//   - release revision: not read by chart-inspector, but cluster state its DryRunServer render sees
//     (lookup, and the GET of each rendered object).
//   - chart-inspector's build version (X-Chart-Inspector-Version), learned from /healthz and from every render.
//   - chart URL / version / repo / TLS / credentials: compositionDefinition.Spec.Chart.* and the
//     password Secret it reads. The cdc keys what it resolved for its own version AND the definition's
//     current version.
//   - definition identity and generation: the object it GETs.
//   - values: helmutils.ValuesFromSpec(composition) then InjectGlobalValues, which also puts the
//     composition's name, namespace, UID, apiVersion, kind, resource, the paused annotation and the
//     krateo namespace into the values.
func (h *handler) derivedRBACKeyFor(ctx context.Context, mg *unstructured.Unstructured, pkg *archive.Info, releaseName string, revision int) (key derivedRBACKey, reason string) {
	if pkg == nil || pkg.CompositionDefinitionInfo == nil || mg.GetUID() == "" {
		return derivedRBACKey{}, rbacDecisionUnscanned
	}
	values, err := helmutils.ValuesFromSpec(mg)
	if err != nil {
		return derivedRBACKey{}, rbacDecisionUnscanned
	}
	if err := values.InjectGlobalValues(mg, h.pluralizer, krateoNamespace); err != nil {
		return derivedRBACKey{}, rbacDecisionUnscanned
	}
	// json.Marshal writes map keys in sorted order, so equal values digest equally.
	raw, err := json.Marshal(values)
	if err != nil {
		return derivedRBACKey{}, rbacDecisionUnscanned
	}
	// A template that runs `tpl` over a value renders what the value holds, so a `lookup` written
	// into the composition's own spec reads cluster state just as one in the chart does.
	if lookupWord.Match(raw) {
		return derivedRBACKey{}, rbacDecisionLookup
	}
	valuesSum := sha256.Sum256(raw)

	var authSum string
	if pkg.Auth != nil {
		s := sha256.Sum256([]byte(pkg.Auth.Username + "\x00" + pkg.Auth.Password))
		authSum = hex.EncodeToString(s[:])
	}
	cd := pkg.CompositionDefinitionInfo
	return derivedRBACKey{
		GVK:                  mg.GroupVersionKind().String(),
		Namespace:            mg.GetNamespace(),
		Name:                 mg.GetName(),
		UID:                  mg.GetUID(),
		ReleaseName:          releaseName,
		Revision:             revision,
		ChartURL:             pkg.URL,
		ChartVersion:         pkg.Version,
		ChartRepo:            pkg.Repo,
		DefinitionVersion:    pkg.DefinitionVersion,
		InsecureSkipTLS:      pkg.InsecureSkipTLSverify,
		AuthDigest:           authSum,
		DefinitionName:       cd.Name,
		DefinitionNamespace:  cd.Namespace,
		DefinitionGVR:        cd.GVR.String(),
		DefinitionGeneration: cd.Generation,
		DefinitionUID:        cd.UID,
		ValuesDigest:         hex.EncodeToString(valuesSum[:]),
		KrateoNamespace:      krateoNamespace,
		SAName:               h.saName,
		SANamespace:          h.saNamespace,
		InspectorURL:         h.chartInspectorUrl,
		InspectorVersion:     h.derivedRBAC.versions.current(ctx, h.chartInspectorUrl),
	}, ""
}

// rbacCacheUse says an applyDerivedRBAC caller wants the cache, and what it knows that the function
// cannot: the release revision Observe just read. Only Observe passes one. Create, Update-time and
// the pre-teardown refresh always ask chart-inspector, as before.
type rbacCacheUse struct{ revision int }

func (k derivedRBACKey) String() string {
	return fmt.Sprintf("%s %s/%s release=%s rev=%d chart=%s@%s def=%s/%s gen=%d", k.GVK, k.Namespace, k.Name,
		k.ReleaseName, k.Revision, k.ChartURL, k.ChartVersion, k.DefinitionNamespace, k.DefinitionName, k.DefinitionGeneration)
}
