package crdtightening

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// ShippedCRD is one CRD, with its per-version schemas as the comparison wants them.
//
// The schemas are map[string]any rather than the typed *JSONSchemaProps because that is what
// Tightenings takes, and the conversion goes through JSON so it is exact rather than hand-copied:
// a field this repo does not model yet still arrives in the map and is still compared.
type ShippedCRD struct {
	Source  string // the file it was read from, so a finding can cite it
	Name    string // compositiondefinitions.core.krateo.io
	Group   string
	Kind    string
	Schemas map[string]map[string]any // version name -> openAPIV3Schema
}

// VersionNames returns the versions in a stable order, so a report does not reorder between runs.
func (c ShippedCRD) VersionNames() []string {
	out := make([]string, 0, len(c.Schemas))
	for v := range c.Schemas {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Report is the outcome of a comparison, and it carries its own DENOMINATOR.
//
// Pairs is the number of (CRD, version) pairs actually compared. It is here because a comparison
// that ran over nothing produces an empty Findings slice, which is indistinguishable from a clean
// run -- the single most likely way for this gate to be decorative. Every caller must read Pairs
// before believing Findings.
type Report struct {
	Findings []Tightening

	Pairs           int // (CRD, version) pairs compared — the denominator
	ShippedVersions int // versions found on disk
	BaselineCRDs    int // CRDs found in the baseline artifact

	// NewVersions are shipped versions with no counterpart in the baseline. NOT findings: a version
	// that did not exist cannot have become stricter.
	NewVersions []string

	// DroppedVersions are baseline versions no longer shipped. ALSO NOT findings, and this is a
	// decision rather than an omission: core-provider prunes stale served versions on purpose
	// (pruneStaleServedVersions, #207), so flagging a removal would make the gate fail on correct
	// behaviour. Reported so the number is visible rather than silently discarded.
	DroppedVersions []string
}

// Clean reports whether the gate passes. A report with no findings and no pairs is NOT clean.
func (r Report) Clean() bool { return len(r.Findings) == 0 && r.Pairs > 0 }

// Summary is one line for a run log, with the denominator beside the result.
func (r Report) Summary() string {
	return fmt.Sprintf("%d finding(s) over %d (CRD, version) pair(s); %d shipped version(s), "+
		"%d baseline CRD(s); %d new version(s) skipped, %d dropped version(s) ignored",
		len(r.Findings), r.Pairs, r.ShippedVersions, r.BaselineCRDs,
		len(r.NewVersions), len(r.DroppedVersions))
}

// DiscoverCRDs reads every CRD this repo ships from `dir`, non-recursively.
//
// `go/core-provider/crds` is the canonical source and the Makefile says so: "CRD-owning repo
// regenerates ./crds", and `generate: ## Regenerate CRDs into ./crds.` -- which the shared release
// workflow invokes on every tag. helm/core-provider-crds/templates/ holds the same content today
// but is a copy that can go stale without anything complaining, so reading it would make this gate
// measure the copy rather than the artifact.
func DiscoverCRDs(dir string) ([]ShippedCRD, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading the shipped CRD directory %s: %w", dir, err)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); strings.HasSuffix(n, ".yaml") || strings.HasSuffix(n, ".yml") {
			files = append(files, filepath.Join(dir, n))
		}
	}
	// A ZERO HERE IS AN ERROR, NOT AN EMPTY RESULT. Discovery finding nothing means the path is
	// wrong or the generator did not run, and either way every comparison downstream would report
	// "no tightenings" over an empty set. A check that inspects nothing passes for the wrong reason.
	if len(files) == 0 {
		return nil, fmt.Errorf("no .yaml files in %s: discovery found nothing to compare, which is "+
			"a broken gate rather than a clean result", dir)
	}

	var out []ShippedCRD
	for _, f := range files {
		crds, err := crdsFromFile(f)
		if err != nil {
			return nil, err
		}
		out = append(out, crds...)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("found %d yaml file(s) in %s but no CustomResourceDefinition in any "+
			"of them", len(files), dir)
	}
	return out, nil
}

// LoadBaselineCRDs walks an unpacked baseline chart and returns every CRD in it.
//
// Walked and filtered BY CONTENT rather than by path. A chart's CRDs may sit under templates/,
// crds/ or a subchart directory, and the release workflow moves them ("refreshing crds-subchart/"),
// so a hard-coded path is a false negative waiting for a chart reorganisation -- and a false
// negative here reads exactly like "the baseline has no such CRD", which this gate would then
// report as a new version and skip.
func LoadBaselineCRDs(root string) ([]ShippedCRD, error) {
	var out []ShippedCRD
	var scanned int

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
			return nil
		}
		scanned++
		crds, cerr := crdsFromFile(path)
		if cerr != nil {
			// A chart template that is not a parseable CRD is normal -- helm templates are not
			// valid YAML until rendered. Skipped rather than fatal, and the scanned count below is
			// what keeps that from hiding a baseline with nothing in it.
			return nil
		}
		out = append(out, crds...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking the baseline artifact at %s: %w", root, err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no CustomResourceDefinition found in the baseline artifact at %s "+
			"(%d yaml file(s) scanned): comparing against it would report every shipped version as "+
			"new and find nothing", root, scanned)
	}
	return out, nil
}

// crdsFromFile decodes every CRD document in one file. Multi-document files are real: controller-gen
// emits one CRD per file today, but a chart may concatenate them, and a decoder that reads only the
// first document would silently compare a subset.
func crdsFromFile(path string) ([]ShippedCRD, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}

	var out []ShippedCRD
	for _, doc := range splitYAMLDocuments(raw) {
		if len(strings.TrimSpace(string(doc))) == 0 {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal(doc, &crd); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		if crd.Kind != "CustomResourceDefinition" {
			continue
		}

		schemas := map[string]map[string]any{}
		for _, v := range crd.Spec.Versions {
			if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
				continue
			}
			m, err := schemaToMap(v.Schema.OpenAPIV3Schema)
			if err != nil {
				return nil, fmt.Errorf("converting the schema of %s/%s in %s: %w",
					crd.Name, v.Name, path, err)
			}
			schemas[v.Name] = m
		}
		out = append(out, ShippedCRD{
			Source:  path,
			Name:    crd.Name,
			Group:   crd.Spec.Group,
			Kind:    crd.Spec.Names.Kind,
			Schemas: schemas,
		})
	}
	return out, nil
}

// schemaToMap converts a typed schema to the generic form Tightenings compares, through JSON so
// nothing is lost or renamed on the way.
func schemaToMap(s *apiextensionsv1.JSONSchemaProps) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// splitYAMLDocuments splits on lines that are exactly a document separator, which is the only form
// that is unambiguous: a bare `---` inside a block scalar is content, and splitting on the string
// would cut a schema in half.
func splitYAMLDocuments(raw []byte) [][]byte {
	var docs [][]byte
	var cur []byte
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 0, 1024*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimRight(line, " \t") == "---" {
			docs = append(docs, cur)
			cur = nil
			continue
		}
		cur = append(cur, []byte(line+"\n")...)
	}
	docs = append(docs, cur)
	return docs
}

// Compare pairs each shipped (CRD, version) with the baseline's and reports every tightening.
//
// Keyed on the CRD's NAME plus the version, not on the file name or on position. Both sides are
// generated and a file may be renamed, reordered or merged without any schema changing.
//
// WHY THIS GATE IS THE ONLY INSTRUMENT, measured rather than assumed. core-provider ships two CRDs
// and each declares exactly ONE version -- compositiondefinitions.core.krateo.io/v1alpha1 and
// kubernetestargets.core.krateo.io/v1alpha1. So every schema change this repo ever makes is an
// IN-PLACE change to a version that is already served and already has objects stored against it.
// There is no new version to compare against the old one, and nothing in the CRD's own identity
// moves when its schema narrows. A reviewer reading the generated diff sees the change; nothing
// else in CI can tell that the change admits less than the version in the field did.
//
// That is also why NewVersions is the uninteresting bucket here and DroppedVersions is near-empty:
// with one version per CRD, a healthy run has Pairs == the shipped version count and both buckets
// empty. A run where that is NOT so means something moved structurally and deserves a reader.
func Compare(shipped, baseline []ShippedCRD) (Report, error) {
	rep := Report{BaselineCRDs: len(baseline)}

	// name -> version -> schema
	base := map[string]map[string]map[string]any{}
	for _, b := range baseline {
		if base[b.Name] == nil {
			base[b.Name] = map[string]map[string]any{}
		}
		for v, s := range b.Schemas {
			base[b.Name][v] = s
		}
	}

	seen := map[string]bool{}
	for _, c := range shipped {
		for _, v := range c.VersionNames() {
			rep.ShippedVersions++
			key := c.Name + "/" + v
			old, ok := base[c.Name][v]
			if !ok {
				rep.NewVersions = append(rep.NewVersions, key)
				continue
			}
			seen[key] = true
			rep.Pairs++
			for _, t := range Tightenings(old, c.Schemas[v], key) {
				rep.Findings = append(rep.Findings, t)
			}
		}
	}

	for name, versions := range base {
		for v := range versions {
			key := name + "/" + v
			if !seen[key] {
				rep.DroppedVersions = append(rep.DroppedVersions, key)
			}
		}
	}
	sort.Strings(rep.NewVersions)
	sort.Strings(rep.DroppedVersions)

	// MEASURED NOTHING IS AN ERROR, not a pass. If every shipped version was new, there was no
	// baseline to compare against and the empty Findings slice says nothing about whether anything
	// got stricter. This is the arm that makes Clean()'s `Pairs > 0` more than decoration.
	if rep.Pairs == 0 {
		return rep, fmt.Errorf("compared 0 (CRD, version) pairs: %d shipped version(s) and %d "+
			"baseline CRD(s), with %d version(s) treated as new. The comparison measured nothing, "+
			"so 'no tightenings' is not a result", rep.ShippedVersions, rep.BaselineCRDs,
			len(rep.NewVersions))
	}
	return rep, nil
}
