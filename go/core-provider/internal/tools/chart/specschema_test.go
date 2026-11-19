package chart

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/krateo-platformops/plumbing/valuesschema"
)

func schemalessChart(values string) fstest.MapFS {
	return fstest.MapFS{
		"c/Chart.yaml":  {Data: []byte("apiVersion: v2\nname: c\nversion: 0.1.0\n")},
		"c/values.yaml": {Data: []byte(values)},
	}
}

func TestOptIn(t *testing.T) {
	for _, tc := range []struct {
		ann         map[string]string
		wantInfer   bool
		wantInvalid string
	}{
		{nil, false, ""},
		{map[string]string{"other": "x"}, false, ""},
		{map[string]string{ValuesSchemaAnnotation: "infer"}, true, ""},
		{map[string]string{ValuesSchemaAnnotation: " infer "}, true, ""},
		{map[string]string{ValuesSchemaAnnotation: "Infer"}, false, "Infer"},
		{map[string]string{ValuesSchemaAnnotation: "true"}, false, "true"},
		{map[string]string{ValuesSchemaAnnotation: ""}, false, ""},
	} {
		infer, invalid := OptIn(tc.ann)
		if infer != tc.wantInfer || invalid != tc.wantInvalid {
			t.Errorf("OptIn(%v) = %v, %q; want %v, %q", tc.ann, infer, invalid, tc.wantInfer, tc.wantInvalid)
		}
	}
}

// The authored file always wins, opt-in or not; without it, only the opt-in infers, and anything else
// is the fs.ErrNotExist the caller reports as ValuesSchemaMissing.
func TestSpecSchema_Sources(t *testing.T) {
	authored := schemalessChart("a: 1\n")
	authored["c/values.schema.json"] = &fstest.MapFile{Data: []byte(`{"type":"object"}`)}

	for _, infer := range []bool{false, true} {
		r, err := SpecSchema(authored, "c", infer)
		if err != nil || r.Source != SchemaAuthored || string(r.Bytes) != `{"type":"object"}` || r.Report != nil {
			t.Errorf("authored, infer=%v: %+v, %v; want the authored file", infer, r, err)
		}
	}

	if _, err := SpecSchema(schemalessChart("a: 1\n"), "c", false); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("no file, no opt-in: err = %v, want fs.ErrNotExist (ValuesSchemaMissing)", err)
	}

	r, err := SpecSchema(schemalessChart("a: 1\n"), "c", true)
	if err != nil || r.Source != SchemaInferred || r.Report == nil {
		t.Fatalf("no file, opted in: %+v, %v; want an inferred schema with its report", r, err)
	}
	if !strings.Contains(string(r.Bytes), `"x-krateo-values-schema":{"source":"inferred"`) {
		t.Errorf("an inferred schema carries no marker: %s", r.Bytes)
	}
}

// Memoised by the digest of the files inference reads: five reads of one chart infer once; a changed
// values.yaml infers again. CONTROL for the first half is the second: a key that ignored content
// would serve the stale schema there.
func TestSpecSchema_InferenceIsMemoisedByContent(t *testing.T) {
	runs := 0
	prev := inferFn
	inferFn = func(fsys fs.FS, dir string) (*valuesschema.Result, error) {
		runs++
		return valuesschema.Infer(fsys, dir)
	}
	defer func() { inferFn = prev }()

	c := schemalessChart("memo-a: 1\n")
	for i := 0; i < 5; i++ {
		if _, err := SpecSchema(c, "c", true); err != nil {
			t.Fatal(err)
		}
	}
	if runs != 1 {
		t.Fatalf("5 reads of an unchanged chart ran inference %d times, want 1", runs)
	}
	c["c/values.yaml"] = &fstest.MapFile{Data: []byte("memo-a: 1\nmemo-b: two\n")}
	r, err := SpecSchema(c, "c", true)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 2 || !strings.Contains(string(r.Bytes), `"memo-b"`) {
		t.Errorf("a changed values.yaml: runs=%d, schema has memo-b=%v; want a fresh inference", runs, strings.Contains(string(r.Bytes), `"memo-b"`))
	}
	// The same LENGTH, different content: a key over paths and sizes alone would serve the stale
	// schema here.
	c["c/values.yaml"] = &fstest.MapFile{Data: []byte("memo-a: 1\nmemo-c: two\n")}
	r, err = SpecSchema(c, "c", true)
	if err != nil {
		t.Fatal(err)
	}
	if runs != 3 || !strings.Contains(string(r.Bytes), `"memo-c"`) {
		t.Errorf("a same-length content change: runs=%d, schema has memo-c=%v; want a fresh inference", runs, strings.Contains(string(r.Bytes), `"memo-c"`))
	}
}

func TestLRU_EvictsOldest(t *testing.T) {
	c := newLRU(2)
	a, b, d := &valuesschema.Result{}, &valuesschema.Result{}, &valuesschema.Result{}
	c.put("a", a)
	c.put("b", b)
	c.get("a") // a is now the most recent
	c.put("d", d)
	if _, ok := c.get("b"); ok {
		t.Error("b, the least recently used, was not evicted")
	}
	if got, ok := c.get("a"); !ok || got != a {
		t.Error("a was evicted though recently used")
	}
}
