package chart

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"
	"unicode/utf8"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

func cm(name string) string {
	return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name + "\n---\n"
}

func fixture(files map[string]string) fstest.MapFS {
	m := fstest.MapFS{}
	for p, b := range files {
		m["c/"+p] = &fstest.MapFile{Data: []byte(b)}
	}
	if _, ok := files["Chart.yaml"]; !ok {
		m["c/Chart.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: v2\nname: c\nversion: 1.2.3\n")}
	}
	return m
}

func kinds(c v1alpha1.ChartContents) string {
	var out []string
	for _, k := range c.Kinds {
		out = append(out, fmt.Sprintf("%s/%s=%d", k.APIVersion, k.Kind, k.Count))
	}
	return strings.Join(out, " ")
}

// The release manifest at default values: counted by kind, ordered by count desc, then apiVersion,
// then kind; NOTES.txt is not an object; a List counts its items.
func TestContents_CountsAndOrder(t *testing.T) {
	c := Contents(fixture(map[string]string{
		"values.yaml":           "replicas: 2\n",
		"templates/cms.yaml":    cm("a") + cm("b") + cm("c"),
		"templates/deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: d\nspec:\n  replicas: {{ .Values.replicas }}\n",
		"templates/secret.yaml": "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n",
		"templates/list.yaml":   "apiVersion: v1\nkind: List\nitems:\n- apiVersion: v1\n  kind: Service\n  metadata:\n    name: s1\n- apiVersion: v1\n  kind: Service\n  metadata:\n    name: s2\n",
		"templates/NOTES.txt":   "thanks",
	}), "c")
	if c.Unknown != "" {
		t.Fatalf("unexpected unknown: %s", c.Unknown)
	}
	want := "v1/ConfigMap=3 v1/Service=2 apps/v1/Deployment=1 v1/Secret=1"
	if got := kinds(c); got != want {
		t.Errorf("kinds = %s\nwant    %s", got, want)
	}
	if c.ChartVersion != "1.2.3" || c.Values != "default" || c.Omitted != 0 {
		t.Errorf("chartVersion=%q values=%q omitted=%d", c.ChartVersion, c.Values, c.Omitted)
	}
}

// Subcharts are included, and helm's dependency conditions decide which ones render at the defaults.
// (Not named "on"/"off": YAML 1.1 reads those keys as booleans, so a condition on them never matches.)
func TestContents_SubchartsIncluded(t *testing.T) {
	c := Contents(fixture(map[string]string{
		"Chart.yaml":                        "apiVersion: v2\nname: c\nversion: 1.0.0\ndependencies:\n- name: sub-on\n  version: 0.1.0\n- name: sub-off\n  version: 0.1.0\n  condition: sub-off.enabled\n",
		"values.yaml":                       "sub-off:\n  enabled: false\n",
		"templates/cm.yaml":                 cm("parent"),
		"charts/sub-on/Chart.yaml":          "apiVersion: v2\nname: sub-on\nversion: 0.1.0\n",
		"charts/sub-on/templates/svc.yaml":  "apiVersion: v1\nkind: Service\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: Service\nmetadata:\n  name: b\n",
		"charts/sub-off/Chart.yaml":         "apiVersion: v2\nname: sub-off\nversion: 0.1.0\n",
		"charts/sub-off/templates/pvc.yaml": "apiVersion: v1\nkind: PersistentVolumeClaim\nmetadata:\n  name: p\n",
	}), "c")
	if c.Unknown != "" {
		t.Fatalf("unexpected unknown: %s", c.Unknown)
	}
	if got, want := kinds(c), "v1/Service=2 v1/ConfigMap=1"; got != want {
		t.Errorf("kinds = %s, want %s (the enabled subchart's Services, not the disabled one's PVC)", got, want)
	}
}

// Hooks are not part of the release manifest. CONTROL: the same Job without the hook annotation is.
func TestContents_HooksExcluded(t *testing.T) {
	job := "apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: j\n%s"
	hooked := Contents(fixture(map[string]string{
		"templates/job.yaml": fmt.Sprintf(job, "  annotations:\n    helm.sh/hook: pre-install\n"),
		"templates/cm.yaml":  cm("x"),
	}), "c")
	plain := Contents(fixture(map[string]string{
		"templates/job.yaml": fmt.Sprintf(job, ""),
		"templates/cm.yaml":  cm("x"),
	}), "c")
	if got := kinds(hooked); got != "v1/ConfigMap=1" {
		t.Errorf("with a pre-install hook: %s, want only the ConfigMap", got)
	}
	if got := kinds(plain); got != "batch/v1/Job=1 v1/ConfigMap=1" {
		t.Errorf("control, the Job without a hook: %s, want the Job counted", got)
	}
}

// A chart that cannot render at its defaults is Unknown with the reason, and Kinds is empty, never
// an empty list read as "creates nothing".
func TestContents_UnknownWhenNotRenderableAtDefaults(t *testing.T) {
	for name, tc := range map[string]struct {
		files map[string]string
		why   string
	}{
		"a required value": {map[string]string{
			"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ required \"name is required\" .Values.name }}\n",
		}, "name is required"},
		"defaults violating values.schema.json": {map[string]string{
			"values.yaml":        "{}\n",
			"values.schema.json": `{"type":"object","required":["region"],"properties":{"region":{"type":"string"}}}`,
			"templates/cm.yaml":  cm("x"),
		}, "region"},
		"a declared dependency not vendored": {map[string]string{
			"Chart.yaml":        "apiVersion: v2\nname: c\nversion: 1.0.0\ndependencies:\n- name: missing\n  version: 0.1.0\n",
			"templates/cm.yaml": cm("x"),
		}, "missing in charts/"},
	} {
		t.Run(name, func(t *testing.T) {
			c := Contents(fixture(tc.files), "c")
			if c.Unknown == "" || !strings.Contains(c.Unknown, tc.why) {
				t.Fatalf("unknown = %q, want it to name %q", c.Unknown, tc.why)
			}
			if len(c.Kinds) != 0 || c.Omitted != 0 {
				t.Errorf("unknown with kinds %v / omitted %d; Kinds must be empty", c.Kinds, c.Omitted)
			}
			if c.Values != "default" {
				t.Errorf("values = %q", c.Values)
			}
		})
	}
	// CONTROL: the same required-value chart renders once the default provides the value.
	c := Contents(fixture(map[string]string{
		"values.yaml":       "name: given\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ required \"name is required\" .Values.name }}\n",
	}), "c")
	if c.Unknown != "" || kinds(c) != "v1/ConfigMap=1" {
		t.Errorf("control: %q %s", c.Unknown, kinds(c))
	}
}

// More kinds than the cap: the first ChartContentsMaxKinds in order are listed, the rest counted.
func TestContents_CappedWithOmitted(t *testing.T) {
	var b strings.Builder
	total := v1alpha1.ChartContentsMaxKinds + 10
	for i := 0; i < total; i++ {
		fmt.Fprintf(&b, "apiVersion: example.org/v1\nkind: K%03d\nmetadata:\n  name: x\n---\n", i)
	}
	b.WriteString(cm("a") + cm("b")) // the one kind with count 2 must sort first
	c := Contents(fixture(map[string]string{"templates/many.yaml": b.String()}), "c")
	if len(c.Kinds) != v1alpha1.ChartContentsMaxKinds || c.Omitted != total+1-v1alpha1.ChartContentsMaxKinds {
		t.Fatalf("listed %d, omitted %d; want %d and %d", len(c.Kinds), c.Omitted, v1alpha1.ChartContentsMaxKinds, total+1-v1alpha1.ChartContentsMaxKinds)
	}
	if c.Kinds[0].Kind != "ConfigMap" || c.Kinds[1].Kind != "K000" || c.Kinds[len(c.Kinds)-1].Kind != fmt.Sprintf("K%03d", v1alpha1.ChartContentsMaxKinds-2) {
		t.Errorf("order: first %s, second %s, last %s", c.Kinds[0].Kind, c.Kinds[1].Kind, c.Kinds[len(c.Kinds)-1].Kind)
	}
	// CONTROL: a chart under the cap omits nothing.
	if small := Contents(fixture(map[string]string{"templates/cm.yaml": cm("a")}), "c"); small.Omitted != 0 {
		t.Errorf("control: omitted %d under the cap", small.Omitted)
	}
}

// Deterministic: the same files give a byte-identical result, from the memo and from a fresh
// render; a same-length content change is a new render, not a stale memo hit.
func TestContents_DeterministicAndMemoisedByContent(t *testing.T) {
	files := map[string]string{"templates/a.yaml": cm("a") + "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\n"}
	first := Contents(fixture(files), "c")
	again := Contents(fixture(files), "c")
	contentsCache = newContentsLRU(128) // a fresh process
	fresh := Contents(fixture(files), "c")
	if !reflect.DeepEqual(first, again) || !reflect.DeepEqual(first, fresh) {
		t.Fatalf("not deterministic:\n%+v\n%+v\n%+v", first, again, fresh)
	}
	// Same length, different kind: "Secret" -> "Sacret" is a different file, so a new render.
	files["templates/a.yaml"] = strings.Replace(files["templates/a.yaml"], "Secret", "Sacret", 1)
	if got := kinds(Contents(fixture(files), "c")); !strings.Contains(got, "Sacret") {
		t.Errorf("a same-length content change served the stale render: %s", got)
	}
	// A caller's copy cannot change the memoised value.
	c := Contents(fixture(files), "c")
	c.Kinds[0].Count = 99
	if Contents(fixture(files), "c").Kinds[0].Count == 99 {
		t.Error("mutating a returned result changed the memo")
	}
}

// #599 / frontend: Unknown is helm's render error, whose length the chart decides; it is capped so a
// long template error cannot bloat the definition's status. The cut lands on a rune boundary and is
// marked. CONTROL: a short message is untouched.
func TestCapUnknown(t *testing.T) {
	short := "the chart does not render at its default values: boom"
	if got := capUnknown(short); got != short {
		t.Fatalf("control: a short message changed: %q", got)
	}
	long := strings.Repeat("é", UnknownMaxBytes) // 2 bytes per rune, so the cut can fall mid-rune
	got := capUnknown(long)
	if len(got) > UnknownMaxBytes {
		t.Fatalf("capped to %d bytes, want <= %d", len(got), UnknownMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("the cut split a rune")
	}
	if !strings.HasSuffix(got, " ... (truncated)") {
		t.Fatalf("no truncation mark: %q", got[len(got)-30:])
	}
}
