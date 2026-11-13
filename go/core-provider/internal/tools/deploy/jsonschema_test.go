package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// configMapDataLimit is the apiserver's limit on a ConfigMap's data (corev1 validation).
const configMapDataLimit = 1048576

var schemaCMGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-3-635", Resource: "installers"}

// shippedJSONSchemaTemplate returns the ConfigMap template core-provider renders at runtime: the
// chart asset, with helm's escaping undone the way helm renders it into the mounted file.
func shippedJSONSchemaTemplate(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "..", "helm", "core-provider", "assets", "jsonSchemaConfigmap.yaml"))
	if err != nil {
		t.Fatalf("read the shipped template: %v", err)
	}
	s := strings.NewReplacer(`{{ "{{" }}`, "{{", `{{ "}}" }}`, "}}").Replace(string(raw))
	if !strings.Contains(s, "{{ .schema | indent 4 }}") || !strings.Contains(s, `"values.schema.json"`) {
		t.Fatalf("premise: the unescaped template must render .schema under the values.schema.json key, got:\n%s", s)
	}
	p := filepath.Join(t.TempDir(), "jsonSchemaConfigmap.yaml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// prettySchema builds a pretty-printed values.schema.json of at least minBytes, in the shape that
// matters: top-level properties in NON-alphabetical authoring order (zeta before alpha), number
// literals a re-encode would rewrite (10.50, 1e3), and a string with significant inner whitespace.
func prettySchema(minBytes int) []byte {
	var b strings.Builder
	b.WriteString("{\n  \"$schema\": \"http://json-schema.org/draft-07/schema#\",\n  \"type\": \"object\",\n  \"properties\": {\n")
	b.WriteString("    \"zeta\": {\n      \"type\": \"number\",\n      \"default\": 10.50,\n      \"maximum\": 1e3\n    },\n")
	b.WriteString("    \"alpha\": {\n      \"type\": \"string\",\n      \"description\": \"keeps  two spaces and a \\n escape\"\n    }")
	for i := 0; b.Len() < minBytes; i++ {
		fmt.Fprintf(&b, ",\n    \"field%06d\": {\n      \"type\": \"string\",\n      \"description\": \"generated field number %06d of the bulk\"\n    }", i, i)
	}
	b.WriteString("\n  }\n}\n")
	return []byte(b.String())
}

func TestRenderJSONSchemaConfigMapStoresCompactJSON(t *testing.T) {
	tmpl := shippedJSONSchemaTemplate(t)
	in := prettySchema(0)
	var want bytes.Buffer
	if err := json.Compact(&want, in); err != nil {
		t.Fatal(err)
	}

	cm, err := renderJSONSchemaConfigMap(schemaCMGVR, types.NamespacedName{Name: "installers-v0-3-635", Namespace: "krateo-system"}, tmpl, in)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := cm.Data["values.schema.json"]
	if !ok {
		t.Fatalf("data keys = %v, want the key to stay values.schema.json", keysOf(cm.Data))
	}
	if got != want.String() {
		t.Fatalf("stored schema is not json.Compact of the chart's file:\n got %q\nwant %q", got, want.String())
	}

	// What the readers rely on, stated separately so a failure names the broken property.
	if strings.ContainsAny(got, "\n") {
		t.Error("the stored schema still contains newlines")
	}
	if zi, ai := strings.Index(got, `"zeta"`), strings.Index(got, `"alpha"`); zi < 0 || ai < 0 || zi > ai {
		t.Errorf("authoring order lost: zeta at %d, alpha at %d; the portal form shows fields in this order", zi, ai)
	}
	for _, lit := range []string{`"default":10.50`, `"maximum":1e3`, `"keeps  two spaces and a \n escape"`} {
		if !strings.Contains(got, lit) {
			t.Errorf("literal %s not preserved verbatim in %s", lit, got)
		}
	}
	var a, b any
	if err := json.Unmarshal(in, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(got), &b); err != nil {
		t.Fatalf("stored schema does not parse: %v", err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Error("stored schema decodes to a different document than the chart's file")
	}
}

// The size the fix exists for: a schema over the limit pretty-printed, under it compact (installer
// 0.3.635: 1,059,606 B pretty, 608,420 B compact).
func TestRenderJSONSchemaConfigMapFitsASchemaOverTheLimitPrettyPrinted(t *testing.T) {
	tmpl := shippedJSONSchemaTemplate(t)
	in := prettySchema(1059606)
	if len(in) <= configMapDataLimit {
		t.Fatalf("premise: the fixture must exceed the ConfigMap limit pretty-printed, it is %d B", len(in))
	}
	cm, err := renderJSONSchemaConfigMap(schemaCMGVR, types.NamespacedName{Name: "installers-v0-3-635", Namespace: "krateo-system"}, tmpl, in)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(cm.Data["values.schema.json"]); n >= configMapDataLimit {
		t.Errorf("compact schema is %d B, want under the %d B ConfigMap limit (pretty was %d B)", n, configMapDataLimit, len(in))
	}
}

// Not valid JSON is an error, not a ConfigMap carrying something the readers cannot parse.
func TestRenderJSONSchemaConfigMapRefusesInvalidJSON(t *testing.T) {
	tmpl := shippedJSONSchemaTemplate(t)
	for _, in := range [][]byte{nil, []byte(""), []byte(`{"type": "object",`), []byte(`not json`)} {
		if cm, err := renderJSONSchemaConfigMap(schemaCMGVR, types.NamespacedName{Name: "x", Namespace: "y"}, tmpl, in); err == nil {
			t.Errorf("%q rendered %v with no error", in, cm.Data)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
