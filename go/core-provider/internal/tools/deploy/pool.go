package deploy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// A cdc pool (docs/design/cdc-pooling.md §5c): one cdc process hosting several Kind x version
// controllers, its members. Its objects live in the management namespace beside the dedicated
// controllers they replace; each member keeps its per-Kind x version objects under today's names
// (env ConfigMap, derived RBAC, ownership Lease grant), with the pool's ServiceAccount as subject.

const (
	// PoolLabel is set on every object of a pool, valued with the pool's name.
	PoolLabel = "krateo.io/cdc-pool"
	// poolPrefix keeps every pool object name a DNS-1123 label: the API caps a pool name at 54
	// characters, and 9 + 54 = 63.
	poolPrefix = "cdc-pool-"
)

// PoolObjectNames are the names of a pool's objects, all in the management namespace.
type PoolObjectNames struct {
	// Workload is the Deployment, ServiceAccount and Service name: cdc-pool-<pool>.
	Workload string
	// EnvConfigMap holds the pool's process-level env: cdc-pool-<pool>-configmap.
	EnvConfigMap string
	// Index is the membership index the pool watches: cdc-pool-<pool>-members.
	Index string
}

// PoolNames derives a pool's object names from its name.
func PoolNames(pool string) PoolObjectNames {
	w := poolPrefix + pool
	return PoolObjectNames{Workload: w, EnvConfigMap: w + configmapResourceSuffix, Index: w + "-members"}
}

// MemberKey is a member's key in the index and the stem of its per-Kind x version objects:
// <resource>-<version>, exactly the dedicated controller's object name.
func MemberKey(gvr schema.GroupVersionResource) string {
	return resourceNamer(gvr.Resource, gvr.Version)
}

// MemberEnvConfigMapName is the member's env ConfigMap, rendered exactly as for a dedicated
// controller: the pool reads it to configure that member.
func MemberEnvConfigMapName(gvr schema.GroupVersionResource) string {
	return getCDCConfigmapNN(types.NamespacedName{Name: MemberKey(gvr)}).Name
}

// PoolMember is one member as the index records it: its Kind x version, and the live data of its env
// ConfigMap, whose checksum the index carries.
type PoolMember struct {
	GVR       schema.GroupVersionResource
	EnvConfig map[string]string
}

// PoolIndexEntry is one member's value in the index, read by the cdc's pool (manager.IndexEntry):
// the member's env ConfigMap, and that ConfigMap's checksum, so a change to a member's configuration
// changes the index and the pool restarts exactly that member.
type PoolIndexEntry struct {
	ConfigMap string `json:"configMap"`
	Checksum  string `json:"checksum"`
}

// RenderPoolIndex is the membership index: one key per member (MemberKey), valued with its
// PoolIndexEntry as JSON. It is a pure function of the members, so rewriting it on every member
// reconcile cannot drift. No member renders an empty index: the pool's last controller is draining.
func RenderPoolIndex(pool, namespace string, members []PoolMember) (corev1.ConfigMap, error) {
	names := PoolNames(pool)
	data := make(map[string]string, len(members))
	for _, m := range members {
		b, err := json.Marshal(PoolIndexEntry{ConfigMap: MemberEnvConfigMapName(m.GVR), Checksum: configChecksum(m.EnvConfig)})
		if err != nil {
			return corev1.ConfigMap{}, fmt.Errorf("encoding the index entry for %s: %w", m.GVR, err)
		}
		data[MemberKey(m.GVR)] = string(b)
	}
	return corev1.ConfigMap{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"},
		ObjectMeta: metav1.ObjectMeta{Name: names.Index, Namespace: namespace, Labels: map[string]string{PoolLabel: pool}},
		Data:       data,
	}, nil
}

// PoolIndexMembers reads an index back into its member keys, sorted.
func PoolIndexMembers(cm corev1.ConfigMap) []string {
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ValidatePoolName refuses a name whose objects would not be valid: the API's schema already does,
// so this guards a caller that builds a pool name some other way.
func ValidatePoolName(pool string) error {
	if pool == "" || len(poolPrefix+pool) > 63 || strings.Trim(pool, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
		strings.HasPrefix(pool, "-") || strings.HasSuffix(pool, "-") {
		return fmt.Errorf("pool name %q: must be a DNS-1123 label of at most %d characters", pool, 63-len(poolPrefix))
	}
	return nil
}
