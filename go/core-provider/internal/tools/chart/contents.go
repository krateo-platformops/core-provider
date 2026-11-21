package chart

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/engine"
	"helm.sh/helm/v3/pkg/releaseutil"
	"sigs.k8s.io/yaml"
)

// core-provider#599 (Diego, 2026-11-21): a CompositionDefinition's status says what its chart
// creates, for the portal's blueprint page. core-provider renders the chart ITSELF, offline, with
// helm's own library: the same steps `helm install` takes before it talks to a cluster
// (dependency processing, values coalescing and schema validation, template rendering, and the
// hook/manifest split), with no cluster at all. Not chart-inspector, which answers per composition
// with that composition's values and returns traced API calls, not rendered kinds.
//
// What is counted is the release MANIFEST at the chart's default values (see v1alpha1.ChartContents
// for why hooks and crds/ are not part of it).

// ContentsValues is ChartContents.Values: the render uses the chart's own defaults only.
const ContentsValues = "default"

// contentsKubeVersion is the cluster version the render assumes for .Capabilities: the chart floor
// of core-provider itself (helm/core-provider/Chart.yaml kubeVersion), so a chart gating a template
// on the Kubernetes version renders the way it would where core-provider runs.
var contentsKubeVersion = chartutil.KubeVersion{Version: "v1.36.0", Major: "1", Minor: "36"}

// Contents renders the chart under dir in fsys at its default values and counts the release
// manifest's objects by kind. It never fails: a chart that cannot be rendered at its defaults
// returns Unknown with the reason, so the result can always be recorded. It is memoised by the
// digest of every file under dir, so a chart is rendered once, not on every reconcile, and a changed
// file is never served a stale result.
func Contents(fsys fs.FS, dir string) v1alpha1.ChartContents {
	files, key, err := chartFiles(fsys, dir)
	if err != nil {
		return v1alpha1.ChartContents{Values: ContentsValues, Unknown: fmt.Sprintf("the chart's files cannot be read: %v", err)}
	}
	if c, ok := contentsCache.get(key); ok {
		return copyContents(c)
	}
	c := renderContents(files)
	contentsCache.put(key, c)
	return copyContents(c)
}

// copyContents keeps the memoised value from being mutated through a caller's copy.
func copyContents(c v1alpha1.ChartContents) v1alpha1.ChartContents {
	if c.Kinds != nil {
		c.Kinds = append([]v1alpha1.ChartContentsKind(nil), c.Kinds...)
	}
	return c
}

// chartFiles reads every regular file under dir, with paths relative to dir, and their digest.
func chartFiles(fsys fs.FS, dir string) ([]*loader.BufferedFile, string, error) {
	var files []*loader.BufferedFile
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(p, dir), "/")
		files = append(files, &loader.BufferedFile{Name: rel, Data: b})
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	h := sha256.New()
	fmt.Fprintf(h, "max=%d kube=%s\n", v1alpha1.ChartContentsMaxKinds, contentsKubeVersion.Version)
	for _, f := range files {
		fmt.Fprintf(h, "%s %d\n", f.Name, len(f.Data))
		h.Write(f.Data)
	}
	return files, hex.EncodeToString(h.Sum(nil)), nil
}

// renderContents is the uncached render, a pure function of the files.
func renderContents(files []*loader.BufferedFile) v1alpha1.ChartContents {
	unknown := func(version, format string, a ...any) v1alpha1.ChartContents {
		return v1alpha1.ChartContents{ChartVersion: version, Values: ContentsValues, Unknown: fmt.Sprintf(format, a...)}
	}
	chrt, err := loader.LoadFiles(files)
	if err != nil {
		return unknown("", "the chart cannot be loaded: %v", err)
	}
	version := chrt.Metadata.Version
	// As `helm install` does: a dependency declared in Chart.yaml but not vendored in charts/ makes
	// the install fail, so the render does not pretend the subchart renders nothing.
	if deps := chrt.Metadata.Dependencies; len(deps) > 0 {
		if err := checkDependencies(chrt, deps); err != nil {
			return unknown(version, "a dependency of the chart is missing: %v", err)
		}
	}
	// Dependency conditions and tags, evaluated against the defaults, decide which subcharts render.
	if err := chartutil.ProcessDependenciesWithMerge(chrt, map[string]interface{}{}); err != nil {
		return unknown(version, "the chart's dependencies cannot be resolved at its default values: %v", err)
	}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = contentsKubeVersion
	// ToRenderValues coalesces the defaults (subcharts included) and validates them against every
	// values.schema.json, so defaults that violate the chart's own schema are reported, not rendered.
	vals, err := chartutil.ToRenderValues(chrt, map[string]interface{}{},
		chartutil.ReleaseOptions{Name: "contents", Namespace: "default", Revision: 1, IsInstall: true}, caps)
	if err != nil {
		return unknown(version, "the chart's default values are not valid for it: %v", err)
	}
	// engine.Render, not RenderWithClient: no cluster. A template's `lookup` returns an empty
	// result, as in `helm template`.
	rendered, err := engine.Render(chrt, vals)
	if err != nil {
		return unknown(version, "the chart does not render at its default values: %v", err)
	}
	for name := range rendered {
		if strings.HasSuffix(name, "NOTES.txt") {
			delete(rendered, name)
		}
	}
	// helm's own split: hooks are kept out of the release manifest.
	_, manifests, err := releaseutil.SortManifests(rendered, caps.APIVersions, releaseutil.InstallOrder)
	if err != nil {
		return unknown(version, "the rendered chart is not valid Kubernetes YAML: %v", err)
	}
	counts := map[[2]string]int{}
	for _, m := range manifests {
		if err := countManifest(counts, m); err != nil {
			return unknown(version, "the rendered %s cannot be read: %v", path.Base(m.Name), err)
		}
	}
	return summarise(version, counts)
}

// countManifest counts one rendered document: its own kind, or each item of a List.
func countManifest(counts map[[2]string]int, m releaseutil.Manifest) error {
	if m.Head == nil || m.Head.Kind == "" {
		return nil // an empty document renders nothing
	}
	if m.Head.Kind == "List" {
		var l struct {
			Items []struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			} `json:"items"`
		}
		if err := yaml.Unmarshal([]byte(m.Content), &l); err != nil {
			return err
		}
		for _, it := range l.Items {
			if it.Kind != "" {
				counts[[2]string{it.APIVersion, it.Kind}]++
			}
		}
		return nil
	}
	counts[[2]string{m.Head.Version, m.Head.Kind}]++
	return nil
}

// summarise orders the counts (count descending, then apiVersion, then kind) and caps the list.
func summarise(version string, counts map[[2]string]int) v1alpha1.ChartContents {
	kinds := make([]v1alpha1.ChartContentsKind, 0, len(counts))
	for k, n := range counts {
		kinds = append(kinds, v1alpha1.ChartContentsKind{APIVersion: k[0], Kind: k[1], Count: n})
	}
	sort.Slice(kinds, func(i, j int) bool {
		a, b := kinds[i], kinds[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		if a.APIVersion != b.APIVersion {
			return a.APIVersion < b.APIVersion
		}
		return a.Kind < b.Kind
	})
	c := v1alpha1.ChartContents{ChartVersion: version, Values: ContentsValues}
	if len(kinds) > v1alpha1.ChartContentsMaxKinds {
		c.Omitted = len(kinds) - v1alpha1.ChartContentsMaxKinds
		kinds = kinds[:v1alpha1.ChartContentsMaxKinds]
	}
	c.Kinds = kinds
	return c
}

// contentsCache is a small bounded memo, least recently used first out.
var contentsCache = newContentsLRU(128)

type contentsLRU struct {
	mu    sync.Mutex
	max   int
	order []string
	items map[string]v1alpha1.ChartContents
}

func newContentsLRU(max int) *contentsLRU {
	return &contentsLRU{max: max, items: map[string]v1alpha1.ChartContents{}}
}

func (c *contentsLRU) get(k string) (v1alpha1.ChartContents, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.items[k]
	if ok {
		c.touch(k)
	}
	return v, ok
}

func (c *contentsLRU) put(k string, v v1alpha1.ChartContents) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[k]; !ok && len(c.items) >= c.max {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.items, oldest)
	}
	c.items[k] = v
	c.touch(k)
}

func (c *contentsLRU) touch(k string) {
	for i, x := range c.order {
		if x == k {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
	c.order = append(c.order, k)
}

// checkDependencies is helm v3.20.2's action.CheckDependencies (pkg/action/install.go), the check
// `helm install` makes: every dependency Chart.yaml declares must be vendored in charts/. It is
// mirrored rather than imported because helm's action package brings its SQL storage driver,
// kustomize and containerd into this binary (36 modules, measured with go mod tidy) for a ten-line
// predicate. Same rule and the same message.
func checkDependencies(ch *chart.Chart, reqs []*chart.Dependency) error {
	var missing []string
OUTER:
	for _, r := range reqs {
		for _, d := range ch.Dependencies() {
			if d.Name() == r.Name {
				continue OUTER
			}
		}
		missing = append(missing, r.Name)
	}
	if len(missing) > 0 {
		return fmt.Errorf("found in Chart.yaml, but missing in charts/ directory: %s", strings.Join(missing, ", "))
	}
	return nil
}
