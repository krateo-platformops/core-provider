package chart

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/krateo-platformops/plumbing/valuesschema"
	"sigs.k8s.io/yaml"
)

// #568 child 6 (D1, Diego 2026-11-17/18): the ONE seam through which a CompositionDefinition's spec
// schema is read, at all three call sites (Observe, Create, Update).
//
//   - An authored values.schema.json at the chart root always wins.
//   - Otherwise the schema is inferred from the chart's values.yaml files (plumbing valuesschema) only
//     when the definition opted in with the annotation ValuesSchemaAnnotation: ValuesSchemaInfer.
//   - Otherwise the read fails with fs.ErrNotExist, as before, and the caller reports
//     Ready=False/ValuesSchemaMissing, whose message names the opt-in.
//   - An annotation with any other value is NOT an opt-in: the read fails the same way, and
//     InvalidOptIn names the bad value so the message can say what was accepted.

// ValuesSchemaAnnotation is the opt-in annotation on a CompositionDefinition; ValuesSchemaInfer is
// its one accepted value.
const (
	ValuesSchemaAnnotation = "krateo.io/values-schema"
	ValuesSchemaInfer      = "infer"
)

// SchemaSource is where a spec schema came from.
type SchemaSource string

const (
	SchemaAuthored SchemaSource = "Authored"
	SchemaInferred SchemaSource = "Inferred"
)

// SpecSchemaResult is a definition's spec schema and where it came from. Report is set for an
// inferred schema.
type SpecSchemaResult struct {
	Bytes  []byte
	Source SchemaSource
	Report *valuesschema.Report
	// Values is the chart's root values.yaml, decoded (#686): what helm merges under a user's values,
	// so what the generated CRD must default to. Nil when the chart ships none. ValuesErr is set
	// instead when the file exists and cannot be read or parsed: the schema is still usable (a
	// definition being deleted must not depend on it), but nothing may be GENERATED from the pair,
	// because "could not read values.yaml" must not be treated as "values.yaml has no keys", which
	// would strip every default.
	Values    map[string]any
	ValuesErr error
}

// OptIn reads the opt-in annotation: infer is true only for exactly ValuesSchemaInfer; invalid is the
// value when the annotation is present with anything else ("" otherwise).
func OptIn(annotations map[string]string) (infer bool, invalid string) {
	v, ok := annotations[ValuesSchemaAnnotation]
	if !ok {
		return false, ""
	}
	if strings.TrimSpace(v) == ValuesSchemaInfer {
		return true, ""
	}
	return false, v
}

// SpecSchema returns the chart's spec schema: the authored file when present, else an inferred one
// when infer is set. With neither, the error wraps fs.ErrNotExist.
func SpecSchema(fsys fs.FS, dir string, infer bool) (SpecSchemaResult, error) {
	b, err := ChartJsonSchema(fsys, dir)
	if err == nil {
		// The inferred-schema marker is core-provider's to write: an authored schema never carries it
		// (#568 child 7, marker.go). Removed here, once, so every consumer reads the same bytes.
		b, err = stripRootMarker(b)
		if err != nil {
			return SpecSchemaResult{}, fmt.Errorf("reading the chart's values.schema.json: %w", err)
		}
		res := SpecSchemaResult{Bytes: b, Source: SchemaAuthored}
		res.Values, res.ValuesErr = ChartValues(fsys, dir)
		return res, nil
	}
	if !errors.Is(err, fs.ErrNotExist) || !infer {
		return SpecSchemaResult{}, err
	}
	r, err := inferMemoised(fsys, dir)
	if err != nil {
		return SpecSchemaResult{}, fmt.Errorf("inferring a values schema (%s: %s): %w", ValuesSchemaAnnotation, ValuesSchemaInfer, err)
	}
	report := r.Report
	res := SpecSchemaResult{Bytes: r.Schema, Source: SchemaInferred, Report: &report}
	res.Values, res.ValuesErr = ChartValues(fsys, dir)
	return res, nil
}

// ChartValues reads and decodes the chart's root values.yaml (#686). A chart without one returns
// (nil, nil): helm then has no defaults either. Numbers decode as json.Number so an integer keeps its
// exact text.
func ChartValues(fsys fs.FS, dir string) (map[string]any, error) {
	b, err := fs.ReadFile(fsys, path.Join(dir, "values.yaml"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the chart's values.yaml: %w", err)
	}
	j, err := yaml.YAMLToJSON(b)
	if err != nil {
		return nil, fmt.Errorf("parsing the chart's values.yaml: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(j))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding the chart's values.yaml: %w", err)
	}
	return out, nil
}

// inferMemoised is valuesschema.Infer memoised by the digest of exactly the files inference reads
// (every Chart.yaml, values.yaml, values.schema.json and vendored .tgz under dir) and the rules
// version, so a chart's schema is inferred once, not on every Observe, and a changed file is never
// served a stale result. Bounded, least recently used first out.
func inferMemoised(fsys fs.FS, dir string) (*valuesschema.Result, error) {
	key, err := inferenceKey(fsys, dir)
	if err != nil {
		return nil, err
	}
	if r, ok := inferCache.get(key); ok {
		return r, nil
	}
	r, err := inferFn(fsys, dir)
	if err != nil {
		return nil, err
	}
	inferCache.put(key, r)
	return r, nil
}

// inferFn is valuesschema.Infer, a variable so a test can count the runs.
var inferFn = valuesschema.Infer

// inferenceKey digests every file valuesschema reads, in sorted path order, with the rules version.
func inferenceKey(fsys fs.FS, dir string) (string, error) {
	var paths []string
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		switch base := path.Base(p); {
		case base == "Chart.yaml", base == "values.yaml", base == "values.schema.json", strings.HasSuffix(base, ".tgz"):
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(paths)
	h := sha256.New()
	fmt.Fprintf(h, "rules=%s\n", valuesschema.RulesVersion)
	for _, p := range paths {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s %d\n", strings.TrimPrefix(p, dir), len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

const inferCacheSize = 128

var inferCache = newLRU(inferCacheSize)

type lru struct {
	mu    sync.Mutex
	max   int
	order *list.List
	items map[string]*list.Element
}

type lruEntry struct {
	key string
	val *valuesschema.Result
}

func newLRU(max int) *lru {
	return &lru{max: max, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(k string) (*valuesschema.Result, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(e)
	return e.Value.(*lruEntry).val, true
}

func (c *lru) put(k string, v *valuesschema.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		e.Value.(*lruEntry).val = v
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&lruEntry{key: k, val: v})
	for c.order.Len() > c.max {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*lruEntry).key)
	}
}
