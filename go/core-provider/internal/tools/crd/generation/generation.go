package generation

import (
	"crypto/sha256"
	"encoding/hex"
	encjson "encoding/json"
	"fmt"
	"sync"

	_ "embed"

	clientsetscheme "k8s.io/client-go/kubernetes/scheme"

	"k8s.io/apimachinery/pkg/runtime/serializer/json"

	"github.com/krateo-platformops/plumbing/crdgen"
	hasher "github.com/krateo-platformops/plumbing/kubeutil/hasher"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// crdgen.Generate shells out to the Go toolchain at runtime (`go mod tidy` + `go run controller-gen`
// on generated sources). That is expensive, and Observe calls it on EVERY CompositionDefinition
// reconcile purely to diff the desired CRD against the live one. On cold start the 5 managed
// workers fire it concurrently, and the parallel `go` invocations thrash the shared build/module
// cache into a multi-core livelock — the engine pegs every CPU it has and makes zero reconcile
// progress, so component versions never advance and the CDCs never roll (observed wedging the
// installer-release cluster after a restart; a warm cluster with few observes never hits the storm).
//
// Fix both dimensions here, at the single call site, without touching crdgen:
//   - memoize by the (effective spec+status schema, gvk) digest so the toolchain runs once per
//     distinct schema instead of once per Observe (the schema is stable across the vast majority
//     of reconciles), and
//   - serialize the actual invocation so concurrent cache-misses on cold start run the toolchain
//     one at a time instead of stampeding it.
var (
	crdGenCache sync.Map   // digest string -> []byte (generated CRD manifest)
	crdGenMu    sync.Mutex // serializes crdgen.Generate (the go-toolchain exec) across goroutines
)

func crdGenKey(specSchema, statusSchema []byte, gvk schema.GroupVersionKind) string {
	h := sha256.New()
	h.Write(specSchema)
	h.Write([]byte{0})
	h.Write(statusSchema)
	fmt.Fprintf(h, "\x00%s", gvk.String())
	return hex.EncodeToString(h.Sum(nil))
}

//go:embed statics/status.schema.json
var statusJsonSchema []byte

//go:embed statics/empty.schema.json
var emptyJsonSchema []byte

func SetServedStorage(crd *apiextensionsv1.CustomResourceDefinition, version string, served, storage bool) {
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Name == version {
			crd.Spec.Versions[i].Served = served
			crd.Spec.Versions[i].Storage = storage
		}
	}
}

// AppendVersion appends the version of the toadd CRD to the crd CRD and sets the Storage and Served fields in the last version of the crd CRD.
func AppendVersion(crd apiextensionsv1.CustomResourceDefinition, toadd apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, error) {
	for _, el2 := range toadd.Spec.Versions {
		exist := false
		vacuum := false
		for _, el1 := range crd.Spec.Versions {
			if el1.Name == el2.Name {
				exist = true
				break
			}
		}
		for _, el1 := range crd.Spec.Versions {
			if el1.Name == "vacuum" {
				vacuum = true
				break
			}
		}

		if !exist {
			crd.Spec.Versions = append(crd.Spec.Versions, el2)
			if !vacuum {
				crd.Spec.Versions = append(crd.Spec.Versions, apiextensionsv1.CustomResourceDefinitionVersion{
					Name:    "vacuum",
					Served:  false,
					Storage: true,
					Schema: &apiextensionsv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
							Type:        "object",
							Description: "This is a vacuum version to storage different versions",
							Properties: map[string]apiextensionsv1.JSONSchemaProps{
								"apiVersion": {
									Type:        "string",
									Description: "APIVersion defines the versioned schema of this representation of an object. Servers should convert recognized schemas to the latest internal value, and may reject unrecognized values. More info: https://git.k8s.io/community/contributors/devel/sig-architecture/api-conventions.md#resources",
								},
								"kind": {
									Type: "string",
								},
								"metadata": {
									Type: "object",
								},
								"spec": {
									Type:                   "object",
									XPreserveUnknownFields: &[]bool{true}[0],
								},
								"status": {
									Type:                   "object",
									XPreserveUnknownFields: &[]bool{true}[0],
								},
							},
						},
					},
				})
			}
			for i := range crd.Spec.Versions {
				// if different from vacuum served: false and storage: true
				if crd.Spec.Versions[i].Name != "vacuum" {
					crd.Spec.Versions[i].Served = true
					crd.Spec.Versions[i].Storage = false
				}
			}
		}
	}

	return &crd, nil
}

// RemoveStaleVersions removes the named non-vacuum versions from the CRD's spec.Versions.
// The "vacuum" storage version is NEVER removed (the apiserver forbids deleting the storage
// version, and it preserves all instances). The caller is responsible for the prunability
// predicate (no definition/instance references the version, and it is not the current served
// version); this is the pure mutation. Returns whether anything was actually removed.
func RemoveStaleVersions(crd *apiextensionsv1.CustomResourceDefinition, prune map[string]bool) bool {
	removed := false
	kept := make([]apiextensionsv1.CustomResourceDefinitionVersion, 0, len(crd.Spec.Versions))
	for _, v := range crd.Spec.Versions {
		if v.Name != "vacuum" && prune[v.Name] {
			removed = true
			continue
		}
		kept = append(kept, v)
	}
	crd.Spec.Versions = kept
	return removed
}

// compositionVersionColumn surfaces the served version an instance was written through — the
// krateo.io/composition-version label stamped by the version MutatingAdmissionPolicy (F9) / the
// owner-scoped migration (F5) — in `kubectl get <composition>` (#234B). The generated composition
// CRDs otherwise only show AGE/READY, so the version-per-instance is invisible. It reads empty on
// clusters where the stamping policy is not installed (the known policy-absence gap).
var compositionVersionColumn = apiextensionsv1.CustomResourceColumnDefinition{
	Name:     "VERSION",
	Type:     "string",
	JSONPath: `.metadata.labels.krateo\.io/composition-version`,
}

// AddCompositionVersionColumn adds the VERSION printer column to every served version of the CRD
// that does not already carry it. The non-served "vacuum" storage version is skipped (kubectl never
// lists it). Idempotent.
func AddCompositionVersionColumn(crd *apiextensionsv1.CustomResourceDefinition) {
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Name == "vacuum" {
			continue
		}
		has := false
		for _, c := range v.AdditionalPrinterColumns {
			if c.Name == compositionVersionColumn.Name {
				has = true
				break
			}
		}
		if !has {
			v.AdditionalPrinterColumns = append(v.AdditionalPrinterColumns, compositionVersionColumn)
		}
	}
}

func UpdateStatus(crd *apiextensionsv1.CustomResourceDefinition, version apiextensionsv1.CustomResourceDefinitionVersion) error {
	if version.Schema == nil || version.Schema.OpenAPIV3Schema == nil {
		return fmt.Errorf("CRD %s version %s schema is nil", crd.Name, version.Name)
	}
	newStatus := version.Schema.OpenAPIV3Schema.Properties["status"]

	// Update the status schema for all versions
	for i := range crd.Spec.Versions {
		if crd.Spec.Versions[i].Schema != nil && crd.Spec.Versions[i].Schema.OpenAPIV3Schema != nil {
			crd.Spec.Versions[i].Schema.OpenAPIV3Schema.Properties["status"] = newStatus
		}
	}
	return nil
}
func Unmarshal(dat []byte) (*apiextensionsv1.CustomResourceDefinition, error) {
	s := json.NewYAMLSerializer(json.DefaultMetaFactory,
		clientsetscheme.Scheme,
		clientsetscheme.Scheme)

	res := &apiextensionsv1.CustomResourceDefinition{}
	_, _, err := s.Decode(dat, nil, res)
	return res, err
}

func GenerateCRD(specSchema []byte, gvk schema.GroupVersionKind) (*apiextensionsv1.CustomResourceDefinition, error) {
	bcrd, err := generateCRD(specSchema, gvk, false)
	if err != nil {
		return nil, fmt.Errorf("error generating CRD: %w", err)
	}
	crd, err := Unmarshal(bcrd)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling generated CRD: %w", err)
	}
	return crd, nil
}

func StatusEqual(crd1, crd2 *apiextensionsv1.CustomResourceDefinition) (bool, error) {
	crdHasher := hasher.NewFNVObjectHash()
	i1 := 0

	searchFirstStatus := func(crd *apiextensionsv1.CustomResourceDefinition) int {
		for i, v := range crd.Spec.Versions {
			if v.Schema != nil && v.Schema.OpenAPIV3Schema != nil {
				if _, ok := v.Schema.OpenAPIV3Schema.Properties["status"]; ok {
					return i
				}
			}
		}
		return -1
	}
	i1 = searchFirstStatus(crd1)
	if i1 == -1 {
		return false, fmt.Errorf("CRD %s has no version with status property", crd1.Name)
	}
	err := crdHasher.SumHash(crd1.Spec.Versions[i1].Schema.OpenAPIV3Schema.Properties["status"])
	if err != nil {
		return false, fmt.Errorf("error hashing CRD status: %w", err)
	}
	genCRDHasher := hasher.NewFNVObjectHash()

	i2 := searchFirstStatus(crd2)
	if i2 == -1 {
		return false, fmt.Errorf("CRD %s has no version with status property", crd2.Name)
	}
	err = genCRDHasher.SumHash(crd2.Spec.Versions[i2].Schema.OpenAPIV3Schema.Properties["status"])
	if err != nil {
		return false, fmt.Errorf("error hashing generated CRD status: %w", err)
	}

	return crdHasher.GetHash() == genCRDHasher.GetHash(), nil
}

func GVKExists(crd *apiextensionsv1.CustomResourceDefinition, gvk schema.GroupVersionKind) bool {
	// Check if the CRD has the given GVK
	if crd.Spec.Group != gvk.Group {
		return false
	}
	if crd.Spec.Names.Kind != gvk.Kind {
		return false
	}

	for _, v := range crd.Spec.Versions {
		if v.Name == gvk.Version {
			return true
		}
	}
	return false
}

func GetGVRFromGeneratedCRD(specSchema []byte, gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	bcrd, err := generateCRD(specSchema, gvk, true)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("error generating CRD for GVR fallback: %w", err)
	}
	crd, err := Unmarshal(bcrd)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("error unmarshalling generated CRD for GVR fallback: %w", err)
	}
	gvr := schema.GroupVersionResource{
		Group:    crd.Spec.Group,
		Version:  crd.Spec.Versions[0].Name,
		Resource: crd.Spec.Names.Plural,
	}
	return gvr, nil
}

func generateCRD(specSchema []byte, gvk schema.GroupVersionKind, onlyMetadata bool) ([]byte, error) {
	// Read static status schema
	statusSchema := statusJsonSchema

	if onlyMetadata {
		specSchema = []byte(emptyJsonSchema)
		statusSchema = []byte(emptyJsonSchema)
	}

	// Normalize int-or-string unions before handing the schema to crdgen. A chart
	// values.schema.json expresses a Kubernetes quantity as a JSON-Schema union —
	// oneOf/anyOf of {type:number|integer} and {type:string} (e.g. the ACK ec2-chart's
	// resources.requests/limits.cpu/memory). controller-gen (which crdgen shells out to)
	// cannot emit a structural CRD for that union and fails the whole run with "not all
	// generators ran successfully", so the CompositionDefinition never installs. Rewrite
	// each such union to the x-kubernetes-int-or-string extension, which IS emittable.
	// Any other schema is passed through untouched; the emptyJsonSchema no-ops here.
	specSchema = normalizeIntOrStringUnions(specSchema)

	// Cache/serialize the go-toolchain invocation (see the crdGenCache comment above). Key on the
	// EFFECTIVE schemas (post onlyMetadata substitution) so metadata-only calls collapse to one entry.
	key := crdGenKey(specSchema, statusSchema, gvk)
	if v, ok := crdGenCache.Load(key); ok {
		return append([]byte(nil), v.([]byte)...), nil
	}

	crdGenMu.Lock()
	defer crdGenMu.Unlock()
	// Another goroutine may have generated this identical CRD while we waited for the lock.
	if v, ok := crdGenCache.Load(key); ok {
		return append([]byte(nil), v.([]byte)...), nil
	}

	res, err := crdgen.Generate(crdgen.Options{
		Group:        gvk.Group,
		Version:      gvk.Version,
		Kind:         gvk.Kind,
		Managed:      true,
		Categories:   []string{"compositions", "comps"},
		SpecSchema:   specSchema,
		StatusSchema: statusSchema,
	})
	if err != nil {
		return nil, fmt.Errorf("generating crd: %w", err)
	}
	crdGenCache.Store(key, append([]byte(nil), res...))
	return res, nil
}

// normalizeIntOrStringUnions rewrites JSON-Schema int-or-string unions
// (a oneOf/anyOf whose members are exactly {type:number|integer} and {type:string})
// to the Kubernetes `x-kubernetes-int-or-string: true` extension, which controller-gen
// (via crdgen) can emit — a bare oneOf/anyOf union cannot be expressed in a structural
// CRD and fails generation. Every other schema construct is preserved. On any JSON
// parse/marshal error the original bytes are returned unchanged (fail-open: never make
// a schema that previously generated worse).
func normalizeIntOrStringUnions(specSchema []byte) []byte {
	var root interface{}
	if err := encjson.Unmarshal(specSchema, &root); err != nil {
		return specSchema
	}
	normalizeSchemaNode(root)
	out, err := encjson.Marshal(root)
	if err != nil {
		return specSchema
	}
	return out
}

// normalizeSchemaNode walks the decoded schema in place, collapsing int-or-string
// unions at any depth and then recursing into every remaining child.
func normalizeSchemaNode(node interface{}) {
	switch n := node.(type) {
	case map[string]interface{}:
		// A Kubernetes quantity in a chart values.schema.json is a JSON-Schema union —
		// oneOf/anyOf of {type:number|integer} and {type:string} (e.g. the ACK ec2-chart's
		// resources.requests/limits.cpu/memory). crdgen cannot turn a bare union into a
		// usable Go field: it emits {type:object, x-kubernetes-preserve-unknown-fields},
		// so an instance's scalar "128Mi" is rejected ("expected map"). Collapse the union
		// to `type: string` — a quantity is always written as a string ("128Mi","50m"),
		// which controller-gen emits as a clean, instanceable string field.
		for _, key := range []string{"oneOf", "anyOf"} {
			if raw, ok := n[key]; ok && isIntOrStringUnion(raw) {
				delete(n, key)
				n["type"] = "string"
			}
		}
		// controller-gen refuses to emit a float64 field for a bare `type: number` (errors
		// "found float … re-run with crd:allowDangerousTypes=true" and fails the WHOLE
		// generation — e.g. the ec2-chart's reconcile.defaultResyncPeriod/maxConcurentSyncs).
		// Represent it as a string so the CRD generates + instances validate; `integer`/
		// `string`/`boolean`/`object`/`array` are untouched. Numeric `default`s (invalid on
		// a string field) are dropped. A bare number that is truly a float loses its numeric
		// type at the API boundary — an accepted, documented trade against no install at all.
		if t, ok := n["type"].(string); ok && t == "number" {
			n["type"] = "string"
			delete(n, "format")
			delete(n, "default")
		}
		for _, v := range n {
			normalizeSchemaNode(v)
		}
	case []interface{}:
		for _, v := range n {
			normalizeSchemaNode(v)
		}
	}
}

// isIntOrStringUnion reports whether a oneOf/anyOf value is EXACTLY the two-member set
// {number|integer, string}, each member a bare {"type": "..."}. Anything richer (extra
// keys, more members, nested constraints) is left untouched so real unions are preserved.
func isIntOrStringUnion(raw interface{}) bool {
	arr, ok := raw.([]interface{})
	if !ok || len(arr) != 2 {
		return false
	}
	types := map[string]bool{}
	for _, e := range arr {
		m, ok := e.(map[string]interface{})
		if !ok || len(m) != 1 {
			return false
		}
		t, ok := m["type"].(string)
		if !ok {
			return false
		}
		types[t] = true
	}
	return types["string"] && (types["number"] || types["integer"])
}
