package archive

import (
	"context"
	"encoding/base64"
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

	// Version of the chart release.
	Version string `json:"version,omitempty"`

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

func (g *dynamicGetter) Get(uns *unstructured.Unstructured) (*Info, error) {
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
			if err != nil {
				return nil, fmt.Errorf("error getting chart version and kind from composition definition '%s' in namespace '%s': %w", cdInfo.Name, cdInfo.Namespace, err)
			}
			if version != uns.GetLabels()[compositionMeta.CompositionVersionLabel] || kind != uns.GetKind() {
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

	packageUrl, ok, err := unstructured.NestedString(compositionDefinition.UnstructuredContent(), "spec", "chart", "url")
	if err != nil {
		g.logger.Debug("Failed to resolve 'status.packageUrl'", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, err
	}
	if !ok {
		return nil,
			fmt.Errorf("missing 'status.packageUrl' in definition for '%v' in namespace: %s", gvr, uns.GetNamespace())
	}

	g.logger.Debug("PackageUrl for", "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace(), "url", packageUrl)

	packageVersion, _, err := unstructured.NestedString(compositionDefinition.UnstructuredContent(), "spec", "chart", "version")
	if err != nil {
		g.logger.Debug("Failed to resolve 'spec.chart.version'", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, err
	}
	repo, _, err := unstructured.NestedString(compositionDefinition.UnstructuredContent(), "spec", "chart", "repo")
	if err != nil {
		g.logger.Debug("Failed to resolve 'spec.chart.repo'", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, err
	}

	username, _, err := unstructured.NestedString(compositionDefinition.UnstructuredContent(), "spec", "chart", "credentials", "username")
	if err != nil {
		g.logger.Debug("Failed to resolve 'spec.chart.credentials.username'", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, err
	}

	passwordRef, _, err := unstructured.NestedStringMap(compositionDefinition.UnstructuredContent(), "spec", "chart", "credentials", "passwordRef")
	if err != nil {
		g.logger.Debug("Failed to resolve 'spec.chart.credentials.passwordRef'", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, err
	}

	var password string
	if passwordRef != nil {
		password, err = GetSecret(context.Background(), g.dynamicClient, SecretKeySelector{
			Name:      passwordRef["name"],
			Namespace: passwordRef["namespace"],
			Key:       passwordRef["key"],
		})
		if err != nil {
			g.logger.Debug("Failed to resolve secret", "error", err.Error(), "compositionDefinitionName", passwordRef["compositionDefinitionName"], "compositionDefinitionNamespace", passwordRef["compositionDefinitionNamespace"])
			return nil, err
		}
	}
	insecureSkipTLSverify, _, err := unstructured.NestedBool(compositionDefinition.UnstructuredContent(), "spec", "chart", "insecureSkipTLSverify")
	if err != nil {
		g.logger.Debug("Failed to resolve 'spec.chart.insecureSkipTLSverify'", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, err
	}

	compositionDefinitionGVR, err := g.pluralizer.GVKtoGVR(compositionDefinition.GroupVersionKind())
	if err != nil {
		g.logger.Debug("Converting GVK to GVR for composition definition", "error", err.Error(), "compositionDefinitionName", compositionDefinition.GetName(), "compositionDefinitionNamespace", compositionDefinition.GetNamespace())
		return nil, fmt.Errorf("converting GVK to GVR for composition definition: %w", err)
	}

	return &Info{
		URL:     packageUrl,
		Version: packageVersion,
		Repo:    repo,
		Auth: &Auth{
			Username: username,
			Password: password,
		},
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
		// carries an exact-equality requirement on that same label: see main.go:329
		// (labels.NewRequirement(CompositionVersionLabel, selection.Equals, ...)) feeding
		// main.go:376. An instance with no label is never delivered, so this tier never runs for
		// one. It rescues an instance whose label is present but stale; it cannot rescue an
		// instance whose label was never written.
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

func getChartVersionKind(el *unstructured.Unstructured) (string, string, error) {
	apiversion, ok, err := unstructured.NestedString(el.UnstructuredContent(), "status", "apiVersion")
	if err != nil {
		return "", "", fmt.Errorf("failed to resolve 'status.apiVersion': %w", err)
	}
	if !ok {
		return "", "", fmt.Errorf("missing 'status.apiVersion'")
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
		return "", "", fmt.Errorf("missing 'status.kind'")
	}

	version := versionSplit[1]
	return version, kind, nil
}
