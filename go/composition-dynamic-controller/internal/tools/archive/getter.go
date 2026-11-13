package archive

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"github.com/krateo-platformops/unstructured-runtime/pkg/pluralizer"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

type CompositionDefinitionInfo struct {
	Namespace string
	Name      string
	GVR       schema.GroupVersionResource
}

type Auth struct {
	Username string
	Password string
}

type Info struct {
	// URL of the helm chart package that is being requested.
	URL string `json:"url"`

	// Version of the chart release to RENDER: the chart recorded for the requesting controller's own
	// version (#461). For the controller of the definition's current version it is spec.chart.version.
	Version string `json:"version,omitempty"`

	// DefinitionVersion is the CompositionDefinition's CURRENT spec.chart.version, whichever
	// controller asks. It differs from Version only for a controller of an older version, and it is
	// what isVersionMigrationHandover compares against: a retiring controller seeing an instance leave
	// its selector must recognise the move to the CURRENT version, and comparing its own chart
	// instead would read a migration as a genuine delete and uninstall (#461).
	DefinitionVersion string `json:"definitionVersion,omitempty"`

	// Repo is the repository name.
	Repo string `json:"repo,omitempty"`

	// Auth is the credentials to access the chart registry, if needed.
	Auth *Auth `json:"auth,omitempty"`

	// InsecureSkipTLSverify indicates whether to skip TLS verification.
	InsecureSkipTLSverify bool `json:"insecureSkipTLSverify,omitempty"`

	// CompositionDefinitionInfo is the information about the composition definition.
	CompositionDefinitionInfo *CompositionDefinitionInfo `json:"compositionDefinitionInfo,omitempty"`
}

func (i *Info) IsOCI() bool {
	return strings.HasPrefix(i.URL, "oci://")
}

func (i *Info) IsTGZ() bool {
	return strings.HasSuffix(i.URL, ".tgz")
}

func (i *Info) IsHTTP() bool {
	return strings.HasPrefix(i.URL, "http://") || strings.HasPrefix(i.URL, "https://")
}

type Getter interface {
	Get(un *unstructured.Unstructured) (*Info, error)
	// Locate returns what Get does except the registry credentials, and reads no Secret: the
	// composition's definition, its chart URL/version/repo, and the definition's identity. For callers
	// that never fetch the chart. A composition's Delete is one (#402): it must finish even when the
	// chart's passwordRef Secret is gone, and the uninstall needs none of the chart's content.
	Locate(un *unstructured.Unstructured) (*Info, error)
	WithLogger(logger logging.Logger) Getter
}

func Static(chart string) Getter {
	return staticGetter{chartName: chart}
}

func Dynamic(cfg *rest.Config, pluralizer pluralizer.PluralizerInterface) (Getter, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}

	return &dynamicGetter{
		dynamicClient: dyn,
		logger:        logging.NewNopLogger(),
		pluralizer:    pluralizer,
	}, nil
}

var _ Getter = (*staticGetter)(nil)

type staticGetter struct {
	chartName string
}

func (pig staticGetter) WithLogger(logger logging.Logger) Getter {
	return &staticGetter{
		chartName: pig.chartName,
	}
}

func (pig staticGetter) Locate(un *unstructured.Unstructured) (*Info, error) { return pig.Get(un) }

func (pig staticGetter) Get(_ *unstructured.Unstructured) (*Info, error) {
	return &Info{
		URL: pig.chartName,
	}, nil
}

var _ Getter = (*dynamicGetter)(nil)

type dynamicGetter struct {
	dynamicClient dynamic.Interface
	logger        logging.Logger
	pluralizer    pluralizer.PluralizerInterface
}

func (g *dynamicGetter) WithLogger(logger logging.Logger) Getter {
	if logger == nil {
		logger = logging.NewNopLogger()
	}
	return &dynamicGetter{
		dynamicClient: g.dynamicClient,
		logger:        logger,
		pluralizer:    g.pluralizer,
	}
}

func (g *dynamicGetter) Get(uns *unstructured.Unstructured) (*Info, error) { return g.get(uns, true) }

func (g *dynamicGetter) Locate(uns *unstructured.Unstructured) (*Info, error) {
	return g.get(uns, false)
}

// get is Get, and with withCredentials false it is Locate: the passwordRef Secret is not read and
// Info.Auth is nil.
func (g *dynamicGetter) get(uns *unstructured.Unstructured, withCredentials bool) (*Info, error) {
	if uns == nil {
		return nil, fmt.Errorf("unstructured object is nil")
	}
	gvr, err := g.pluralizer.GVKtoGVR(uns.GroupVersionKind())
	if err != nil {
		return nil, fmt.Errorf("error getting GVR for Kind: '%v', Group: '%v, Version: '%v': %w", uns.GroupVersionKind().Kind, uns.GroupVersionKind().Group, uns.GroupVersionKind().Version, err)
	}

	var cdInfo *CompositionDefinitionInfo
	lbl := uns.GetLabels()
	if lbl != nil {
		var gvr schema.GroupVersionResource
		group, ok := lbl[compositionMeta.CompositionDefinitionGroupLabel]
		if ok && group != "" {
			gvr.Group = group
		}
		version, ok := lbl[compositionMeta.CompositionDefinitionVersionLabel]
		if ok && version != "" {
			gvr.Version = version
		}
		resource, ok := lbl[compositionMeta.CompositionDefinitionResourceLabel]
		if ok && resource != "" {
			gvr.Resource = resource
		}

		name := lbl[compositionMeta.CompositionDefinitionNameLabel]

		namespace := lbl[compositionMeta.CompositionDefinitionNamespaceLabel]

		if len(gvr.Group) > 0 && len(gvr.Resource) > 0 && len(gvr.Version) > 0 && len(name) > 0 && len(namespace) > 0 {
			g.logger.Debug("Using labels to get composition definition", "compositionDefinitionName", name, "compositionDefinitionNamespace", namespace, "gvr", gvr.String())
			cdInfo = &CompositionDefinitionInfo{
				Name:      name,
				Namespace: namespace,
				GVR:       gvr,
			}
		}
	}

	var compositionDefinition *unstructured.Unstructured
	if cdInfo != nil {
		g.logger.Debug("Getting composition definition", "compositionDefinitionName", cdInfo.Name, "compositionDefinitionNamespace", cdInfo.Namespace, "compositionDefinitionGVR", cdInfo.GVR.String())
		compositionDefinition, err = g.dynamicClient.Resource(cdInfo.GVR).
			Namespace(cdInfo.Namespace).
			Get(context.Background(), cdInfo.Name, metav1.GetOptions{})
		if err != nil {
			g.logger.Warn("Error getting composition definition", "error", err.Error(), "compositionDefinitionName", cdInfo.Name, "compositionDefinitionNamespace", cdInfo.Namespace, "gvr", cdInfo.GVR.String())
			compositionDefinition = nil
		}
		if compositionDefinition != nil {
			version, kind, err := getChartVersionKind(compositionDefinition)
			switch {
			case err == nil:
			case !withCredentials && errors.Is(err, errDefinitionStatusUnset):
				// Locate, which a composition's Delete uses (#419). The labels name this definition
				// exactly (name, namespace, GVR); with no status there is nothing to check them
				// against. A definition with no status never became Available, so no version
				// migration can be in flight for isVersionMigrationHandover to protect. Failing here
				// instead deadlocked: the definition's Delete waits for its compositions, and the
				// composition's Delete needed this status, which a definition deleted before its
				// first status write never gets. Get (Create/Update) still requires it.
				g.logger.Debug("Composition definition has no status yet; trusting the composition's labels to locate it for teardown",
					"compositionDefinitionName", cdInfo.Name, "compositionDefinitionNamespace", cdInfo.Namespace)
				break
			default:
				return nil, fmt.Errorf("error getting chart version and kind from composition definition '%s' in namespace '%s': %w", cdInfo.Name, cdInfo.Namespace, err)
			}
			if err == nil && (version != uns.GetLabels()[compositionMeta.CompositionVersionLabel] || kind != uns.GetKind()) {
				g.logger.Warn("Labels do not match composition definition", "compositionDefinitionName", cdInfo.Name, "compositionDefinitionNamespace", cdInfo.Namespace, "gvr", gvr.String(), "expectedVersion", uns.GetLabels()[compositionMeta.CompositionVersionLabel], "foundVersion", version, "expectedKind", uns.GetKind(), "foundKind", kind)
				compositionDefinition = nil
			}
		}
	}

	if compositionDefinition == nil {
		// Search for the composition definition in the namespace of the unstructured object
		g.logger.Debug("Searching for composition definition")
		compositionDefinition, err = g.searchCompositionDefinition(gvr, uns)
		if err != nil {
			return nil, fmt.Errorf("error searching for composition definition in namespace '%s': %w", uns.GetNamespace(), err)
		}
	}

	// #461 (Diego, 2026-11-13): render the chart recorded for THIS controller's version, not the
	// definition's current spec.chart. The informer delivers instances at the controller's own
	// version, so uns's version is it.
	chart, err := chartForVersion(compositionDefinition, uns.GroupVersionKind().Version)
	if err != nil {
		g.logger.Debug("Resolving the chart for this controller's version", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace(), "version", uns.GroupVersionKind().Version)
		return nil, err
	}
	definitionVersion, _, err := unstructured.NestedString(compositionDefinition.UnstructuredContent(), "spec", "chart", "version")
	if err != nil {
		return nil, fmt.Errorf("resolving 'spec.chart.version': %w", err)
	}
	packageUrl, repo, packageVersion := chart.url, chart.repo, chart.version
	if packageUrl == "" {
		return nil, fmt.Errorf("missing chart url in definition for '%v' in namespace: %s", gvr, uns.GetNamespace())
	}
	g.logger.Debug("PackageUrl for", "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace(), "url", packageUrl, "chartVersion", packageVersion, "from", chart.source)

	username, passwordRef := chart.username, chart.passwordRef
	var password string
	if passwordRef != nil && withCredentials {
		password, err = GetSecret(context.Background(), g.dynamicClient, SecretKeySelector{
			Name:      passwordRef["name"],
			Namespace: passwordRef["namespace"],
			Key:       passwordRef["key"],
		})
		if err != nil {
			g.logger.Debug("Failed to resolve secret", "error", err.Error(), "secretName", passwordRef["name"], "secretNamespace", passwordRef["namespace"])
			return nil, err
		}
	}
	insecureSkipTLSverify := chart.insecureSkipVerifyTLS

	compositionDefinitionGVR, err := g.pluralizer.GVKtoGVR(compositionDefinition.GroupVersionKind())
	if err != nil {
		g.logger.Debug("Converting GVK to GVR for composition definition", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, fmt.Errorf("converting GVK to GVR for composition definition: %w", err)
	}

	return &Info{
		URL:                   packageUrl,
		Version:               packageVersion,
		DefinitionVersion:     definitionVersion,
		Repo:                  repo,
		Auth:                  authOrNil(withCredentials, username, password),
		InsecureSkipTLSverify: insecureSkipTLSverify,
		CompositionDefinitionInfo: &CompositionDefinitionInfo{
			Name:      compositionDefinition.GetName(),
			Namespace: compositionDefinition.GetNamespace(),
			GVR:       compositionDefinitionGVR,
		},
	}, nil
}

type SecretKeySelector struct {
	Name      string
	Namespace string
	Key       string
}

func GetSecret(ctx context.Context, client dynamic.Interface, secretKeySelector SecretKeySelector) (string, error) {
	gvr := schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "secrets",
	}

	sec, err := client.Resource(gvr).Namespace(secretKeySelector.Namespace).Get(ctx, secretKeySelector.Name, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	data, _, err := unstructured.NestedMap(sec.Object, "data")
	if err != nil {
		return "", err
	}
	bsec := data[secretKeySelector.Key].(string)
	bkey, err := base64.StdEncoding.DecodeString(bsec)
	if err != nil {
		return "", fmt.Errorf("failed to decode secret key: %w", err)
	}
	return string(bkey), nil
}

func (g *dynamicGetter) searchCompositionDefinition(gvr schema.GroupVersionResource, mg *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	gvrForDefinitions := schema.GroupVersionResource{
		Group:    "core.krateo.io",
		Version:  "v1alpha1",
		Resource: "compositiondefinitions",
	}
	all, err := g.dynamicClient.Resource(gvrForDefinitions).
		List(context.Background(), metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	tot := len(all.Items)
	if tot == 0 {
		return nil,
			fmt.Errorf("no definition found for '%v' in namespace: %s", gvr.String(), mg.GetNamespace())
	}

	compositionDefinition := &all.Items[0]
	if tot > 1 {
		instanceLabels := mg.GetLabels()
		found := false

		// 1. Authoritative: the definition-ref labels stamped on the composition instance.
		// These identify the owning CompositionDefinition by name+namespace and survive
		// chart-version bumps, unlike the composition-version label which is only migrated
		// by a successful reconcile.
		refName := instanceLabels[compositionMeta.CompositionDefinitionNameLabel]
		refNamespace := instanceLabels[compositionMeta.CompositionDefinitionNamespaceLabel]
		if refName != "" && refNamespace != "" {
			refMatched := false
			for i := range all.Items {
				el := &all.Items[i]
				if el.GetName() != refName || el.GetNamespace() != refNamespace {
					continue
				}
				refMatched = true
				// The ref labels identify the owner by name+namespace only: guard against
				// them pointing at a definition serving a DIFFERENT kind (mislabeled
				// instance, name reuse after delete/recreate), which would fetch the
				// wrong chart. An unreadable status kind is treated as a mismatch.
				_, kind, kindErr := getChartVersionKind(el)
				if kindErr != nil || kind != mg.GetKind() {
					g.logger.Warn("Definition-ref labels point at a composition definition of a different kind, falling back to version/kind matching", "compositionDefinitionName", refName, "compositionDefinitionNamespace", refNamespace, "expectedKind", mg.GetKind(), "foundKind", kind, "gvr", gvr.String())
					break
				}
				compositionDefinition = el
				g.logger.Debug("Resolved composition definition via definition-ref labels", "compositionDefinitionName", refName, "compositionDefinitionNamespace", refNamespace, "gvr", gvr.String())
				found = true
				break
			}
			if !refMatched {
				// Stale labels are possible: fall through to version/kind matching.
				g.logger.Debug("Definition-ref labels did not match any composition definition, falling back to version/kind matching", "compositionDefinitionName", refName, "compositionDefinitionNamespace", refNamespace, "gvr", gvr.String())
			}
		}

		// 2. Exact match on chart version + kind (previous behavior).
		if !found {
			for i := range all.Items {
				el := &all.Items[i]
				version, kind, err := getChartVersionKind(el)
				if err != nil {
					g.logger.Debug("Failed to get chart version and kind", "error", err.Error(), "compositionDefinitionName", el.GetName(), "compositionDefinitionNamespace", el.GetNamespace(), "gvr", gvr.String())
					continue
				}
				if version == instanceLabels[compositionMeta.CompositionVersionLabel] && kind == mg.GetKind() {
					compositionDefinition = el
					g.logger.Debug("Found matching composition definition", "compositionDefinitionName", el.GetName(), "compositionDefinitionNamespace", el.GetNamespace(), "gvr", gvr.String())
					found = true
					break
				}
			}
		}

		// 3. Last resort: a single definition serving this kind. During a chart-version bump
		// the definition's status version moves ahead of the instance's composition-version
		// label (which only a successful reconcile would migrate), so an exact version match
		// can never succeed and reconciliation wedges. Tolerate the skew when the owner is
		// unambiguous.
		//
		// This tier tolerates version SKEW, not label ABSENCE, and the distinction matters more
		// than it looks. Read on its own it suggests the krateo.io/composition-version label is
		// optional -- resolution clearly succeeds here without a matching one -- which in turn
		// suggests the charts' Kubernetes >= 1.36 floor (the MutatingAdmissionPolicy that stamps
		// that label) could be lowered. It cannot.
		//
		// Everything in this function runs DOWNSTREAM of the controller's ListWatcher, which
		// carries an exact-equality requirement on that same label: see main.go
		// (labels.NewRequirement(CompositionVersionLabel, selection.Equals, ...)) feeding the
		// ListWatcher's LabelSelector. An instance with no label is never delivered, so this
		// tier never runs for one. It rescues an instance whose label is present but stale; it
		// cannot rescue an instance whose label was never written.
		//
		// Noted here rather than only in helm/core-provider/Chart.yaml because this is where a
		// reader checking whether the floor is load-bearing actually lands, and the answer this
		// code appears to give in isolation is the wrong one.
		if !found {
			var sameKind []*unstructured.Unstructured
			for i := range all.Items {
				el := &all.Items[i]
				_, kind, err := getChartVersionKind(el)
				if err != nil {
					continue
				}
				if kind == mg.GetKind() {
					sameKind = append(sameKind, el)
				}
			}
			if len(sameKind) == 1 {
				compositionDefinition = sameKind[0]
				g.logger.Warn("Resolved composition definition by unique kind, tolerating composition-version label skew", "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace(), "expectedVersion", instanceLabels[compositionMeta.CompositionVersionLabel], "kind", mg.GetKind(), "gvr", gvr.String())
				found = true
			}
		}

		if !found {
			return nil,
				fmt.Errorf("too many definitions [%d] found for '%v' in namespace: %s", tot, gvr.String(), mg.GetNamespace())
		}
	}

	g.logger.Debug("Using composition definition", "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace(), "gvr", gvr.String())

	return compositionDefinition, nil
}

// errDefinitionStatusUnset marks a CompositionDefinition whose status core-provider has not written
// yet (no status.apiVersion / status.kind): it never became Available. Distinct from a status that is
// present but malformed, which stays a hard error.
var errDefinitionStatusUnset = errors.New("the composition definition's status is not set yet")

func getChartVersionKind(el *unstructured.Unstructured) (string, string, error) {
	apiversion, ok, err := unstructured.NestedString(el.UnstructuredContent(), "status", "apiVersion")
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve 'status.apiVersion': %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("missing 'status.apiVersion': %w", errDefinitionStatusUnset)
	}
	versionSplit := strings.Split(apiversion, "/")
	if len(versionSplit) != 2 {
		return "", "", fmt.Errorf("invalid format for 'status.apiVersion'")
	}
	kind, ok, err := unstructured.NestedString(el.UnstructuredContent(), "status", "kind")
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve 'status.kind': %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("missing 'status.kind': %w", errDefinitionStatusUnset)
	}

	version := versionSplit[1]
	return version, kind, nil
}

// authOrNil is the registry credentials, or nil for Locate, which reads none.
func authOrNil(withCredentials bool, username, password string) *Auth {
	if !withCredentials {
		return nil
	}
	return &Auth{Username: username, Password: password}
}

// errNoChartForVersion: the definition records no chart for the requesting controller's version. A
// controller must not render some OTHER version's chart in its place (#461), so this is an error.
var errNoChartForVersion = errors.New("the composition definition records no chart for this controller's version")

// resolvedChart is the chart a controller renders, from spec.chart or from the version's record.
type resolvedChart struct {
	url, version, repo, username string
	passwordRef                  map[string]string
	insecureSkipVerifyTLS        bool
	source                       string // "spec.chart" or "status.managed.versionInfo[<version>].chart"
}

// chartForVersion returns the chart the controller of ownVersion renders (#461, Diego 2026-11-13):
//   - the definition's current version (status.apiVersion), or no status yet: spec.chart, as before;
//   - an older version: the chart core-provider recorded for it in status.managed.versionInfo when
//     that version was current (helpers/status.UpdateVersionInfo writes it once and preserves it).
//     So a retiring controller keeps rendering its own chart until migration relabels the instance,
//     and never starts a new-chart upgrade it may be killed in the middle of; and an instance held on
//     an old version under upgradePolicy Manual or Paused stays on its version's chart.
//   - an older version with no recorded chart: errNoChartForVersion, never a guess.
//
// Both shapes (ChartInfo and ChartInfoProps) carry the same JSON keys, read here once. The TLS flag is
// insecureSkipVerifyTLS, the API's key; the getter used to read "insecureSkipTLSverify", which the CRD
// does not have, so the flag was always false (#476).
func chartForVersion(cd *unstructured.Unstructured, ownVersion string) (resolvedChart, error) {
	current, _, verr := getChartVersionKind(cd)
	if verr != nil || ownVersion == "" || ownVersion == current {
		m, _, err := unstructured.NestedMap(cd.UnstructuredContent(), "spec", "chart")
		if err != nil {
			return resolvedChart{}, fmt.Errorf("resolving 'spec.chart': %w", err)
		}
		return readChart(m, "spec.chart")
	}
	infos, _, err := unstructured.NestedSlice(cd.UnstructuredContent(), "status", "managed", "versionInfo")
	if err != nil {
		return resolvedChart{}, fmt.Errorf("resolving 'status.managed.versionInfo': %w", err)
	}
	for _, it := range infos {
		vi, ok := it.(map[string]any)
		if !ok || vi["version"] != ownVersion {
			continue
		}
		m, ok := vi["chart"].(map[string]any)
		if !ok || m == nil {
			break
		}
		return readChart(m, "status.managed.versionInfo["+ownVersion+"].chart")
	}
	return resolvedChart{}, fmt.Errorf("%w: version %s (current %s)", errNoChartForVersion, ownVersion, current)
}

func readChart(m map[string]any, source string) (resolvedChart, error) {
	c := resolvedChart{source: source}
	var err error
	if c.url, _, err = unstructured.NestedString(m, "url"); err != nil {
		return c, fmt.Errorf("%s.url: %w", source, err)
	}
	if c.version, _, err = unstructured.NestedString(m, "version"); err != nil {
		return c, fmt.Errorf("%s.version: %w", source, err)
	}
	if c.repo, _, err = unstructured.NestedString(m, "repo"); err != nil {
		return c, fmt.Errorf("%s.repo: %w", source, err)
	}
	if c.username, _, err = unstructured.NestedString(m, "credentials", "username"); err != nil {
		return c, fmt.Errorf("%s.credentials.username: %w", source, err)
	}
	if c.passwordRef, _, err = unstructured.NestedStringMap(m, "credentials", "passwordRef"); err != nil {
		return c, fmt.Errorf("%s.credentials.passwordRef: %w", source, err)
	}
	if c.insecureSkipVerifyTLS, _, err = unstructured.NestedBool(m, "insecureSkipVerifyTLS"); err != nil {
		return c, fmt.Errorf("%s.insecureSkipVerifyTLS: %w", source, err)
	}
	return c, nil
}
