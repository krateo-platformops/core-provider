package manager

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/composition"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// The keys of a member's configuration: the env ConfigMap core-provider renders for one Kind x
// version (helm/core-provider/assets/cdc/configmap.yaml). A dedicated controller reads them from its
// environment; a pool reads them from each member's ConfigMap (docs/design/cdc-pooling.md §5c). Both
// go through MemberFromConfig, so the two cannot parse one configuration two ways.
const (
	KeyGroup              = "COMPOSITION_CONTROLLER_GROUP"
	KeyVersion            = "COMPOSITION_CONTROLLER_VERSION"
	KeyResource           = "COMPOSITION_CONTROLLER_RESOURCE"
	KeyLeaseName          = "COMPOSITION_CONTROLLER_LEASE_NAME"
	KeyStatusDataTemplate = "COMPOSITION_CONTROLLER_STATUS_DATA_TEMPLATE"
	KeyAPIRefName         = "COMPOSITION_CONTROLLER_API_REF_NAME"
	KeyAPIRefNamespace    = "COMPOSITION_CONTROLLER_API_REF_NAMESPACE"
	KeyAPIRefExtras       = "COMPOSITION_CONTROLLER_API_REF_EXTRAS"
	KeyWorkers            = "COMPOSITION_CONTROLLER_WORKERS"
	KeyResyncInterval     = "COMPOSITION_CONTROLLER_RESYNC_INTERVAL"
)

// ResolverFactory builds a member's apiRef resolver; the snowplow and authn endpoints it needs are the
// process's.
type ResolverFactory func(name, namespace string, extras map[string]any) composition.APIResolver

// MemberFromConfig reads one member from its configuration. A missing GVR is an error: a member that
// cannot say what it reconciles must not start. An unreadable status template or apiRef extras is
// ignored with a warning, as the dedicated controller always has (warn reports it), so a typo in a
// projection degrades that projection rather than stopping the controller. Workers and the resync
// interval are optional per-member overrides; zero means the process default.
func MemberFromConfig(data map[string]string, resolver ResolverFactory, warn func(msg string, err error)) (Member, error) {
	get := func(k string) string { return strings.TrimSpace(data[k]) }
	m := Member{GVR: schema.GroupVersionResource{Group: get(KeyGroup), Version: get(KeyVersion), Resource: get(KeyResource)}}
	if m.GVR.Version == "" || m.GVR.Resource == "" {
		return Member{}, fmt.Errorf("member configuration names no Kind x version (%s, %s and %s)", KeyGroup, KeyVersion, KeyResource)
	}
	m.LeaseName = get(KeyLeaseName)

	if s := get(KeyStatusDataTemplate); s != "" {
		if err := json.Unmarshal([]byte(s), &m.StatusDataTemplate); err != nil {
			m.StatusDataTemplate = nil
			warnf(warn, "ignoring invalid "+KeyStatusDataTemplate, err)
		}
	}
	if name := get(KeyAPIRefName); name != "" && resolver != nil {
		var extras map[string]any
		if s := get(KeyAPIRefExtras); s != "" {
			if err := json.Unmarshal([]byte(s), &extras); err != nil {
				extras = nil
				warnf(warn, "ignoring invalid "+KeyAPIRefExtras, err)
			}
		}
		m.APIResolver = resolver(name, get(KeyAPIRefNamespace), extras)
	}
	if s := get(KeyWorkers); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return Member{}, fmt.Errorf("%s=%q: want a positive integer", KeyWorkers, s)
		}
		m.Workers = n
	}
	if s := get(KeyResyncInterval); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return Member{}, fmt.Errorf("%s=%q: want a positive Go duration", KeyResyncInterval, s)
		}
		m.ResyncInterval = d
	}
	return m, nil
}

func warnf(warn func(string, error), msg string, err error) {
	if warn != nil {
		warn(msg, err)
	}
}
