package composition

import (
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
// ONE INPUT CANNOT BE KEYED: cluster state read through a chart's `lookup`. The render is a server-side
// dry run, so a template may branch on what exists in the cluster, and the cdc cannot know that. The
// release revision covers the chart's own children changing; nothing covers an object the chart merely
// looks up. So an entry also expires after derivedRBACMaxAge, bounding that staleness (and a
// chart-inspector upgrade, whose version the cdc has no way to read without a call) to one interval.
// COMPOSITION_CONTROLLER_DERIVED_RBAC_MAX_AGE=0 turns the cache off.

const derivedRBACMaxAgeEnvvar = "COMPOSITION_CONTROLLER_DERIVED_RBAC_MAX_AGE"

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
}

type derivedRBACEntry struct {
	key   derivedRBACKey
	rules *rbac.RBAC
	at    time.Time
}

// derivedRBACCache is safe for concurrent use: a pooled process runs several compositions at once.
// A nil cache never hits and never stores, which is what a handler built without one gets.
type derivedRBACCache struct {
	mu      sync.Mutex
	entries map[types.UID]derivedRBACEntry
	maxAge  time.Duration
	now     func() time.Time
}

// newDerivedRBACCache returns a cache whose entries expire after maxAge. maxAge <= 0 disables it.
func newDerivedRBACCache(maxAge time.Duration, now func() time.Time) *derivedRBACCache {
	if now == nil {
		now = time.Now
	}
	return &derivedRBACCache{entries: map[types.UID]derivedRBACEntry{}, maxAge: maxAge, now: now}
}

func (c *derivedRBACCache) enabled() bool { return c != nil && c.maxAge > 0 }

// lookup returns a PRIVATE copy of the cached rules when the entry for uid was computed under exactly
// key and has not expired.
func (c *derivedRBACCache) lookup(uid types.UID, key derivedRBACKey) (*rbac.RBAC, bool) {
	if !c.enabled() {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[uid]
	if !ok || e.key != key || c.now().Sub(e.at) >= c.maxAge {
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
	c.entries[uid] = derivedRBACEntry{key: key, rules: rules.DeepCopy(), at: c.now()}
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

// derivedRBACKeyFor builds the key for this pass. ok is false when an input cannot be read, and the
// caller then asks chart-inspector: not knowing is never a reason to apply cached rules.
//
// Inputs, and where chart-inspector (go/chart-inspector/internal/handlers/resources/get/resources.go)
// reads each:
//   - composition name, namespace, GVK, UID: the query parameters, and the live composition it GETs.
//   - releaseName: the query parameter it installs under.
//   - release revision: not read by chart-inspector, but cluster state its DryRunServer render sees
//     (lookup, and the GET of each rendered object).
//   - chart URL / version / repo / TLS / credentials: compositionDefinition.Spec.Chart.* and the
//     password Secret it reads. The cdc keys what it resolved for its own version AND the definition's
//     current version.
//   - definition identity and generation: the object it GETs.
//   - values: helmutils.ValuesFromSpec(composition) then InjectGlobalValues, which also puts the
//     composition's name, namespace, UID, apiVersion, kind, resource, the paused annotation and the
//     krateo namespace into the values.
func (h *handler) derivedRBACKeyFor(mg *unstructured.Unstructured, pkg *archive.Info, releaseName string, revision int) (derivedRBACKey, bool) {
	if pkg == nil || pkg.CompositionDefinitionInfo == nil || mg.GetUID() == "" {
		return derivedRBACKey{}, false
	}
	values, err := helmutils.ValuesFromSpec(mg)
	if err != nil {
		return derivedRBACKey{}, false
	}
	if err := values.InjectGlobalValues(mg, h.pluralizer, krateoNamespace); err != nil {
		return derivedRBACKey{}, false
	}
	// json.Marshal writes map keys in sorted order, so equal values digest equally.
	raw, err := json.Marshal(values)
	if err != nil {
		return derivedRBACKey{}, false
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
	}, true
}

// rbacCacheUse says an applyDerivedRBAC caller wants the cache, and what it knows that the function
// cannot: the release revision Observe just read. Only Observe passes one. Create, Update-time and
// the pre-teardown refresh always ask chart-inspector, as before.
type rbacCacheUse struct{ revision int }

func (k derivedRBACKey) String() string {
	return fmt.Sprintf("%s %s/%s release=%s rev=%d chart=%s@%s def=%s/%s gen=%d", k.GVK, k.Namespace, k.Name,
		k.ReleaseName, k.Revision, k.ChartURL, k.ChartVersion, k.DefinitionNamespace, k.DefinitionName, k.DefinitionGeneration)
}
