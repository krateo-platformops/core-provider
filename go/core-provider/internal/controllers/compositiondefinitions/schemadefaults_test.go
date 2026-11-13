package compositiondefinitions

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// schemaDefaultMismatches walks a values.schema.json and reports every node whose `default` differs
// from values.yaml at the same path, or names a key values.yaml does not define. Both sides go
// through JSON, so a YAML int and a JSON number compare equal.
func schemaDefaultMismatches(t *testing.T, schemaJSON, valuesYAML []byte) (mismatches []string, compared int) {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal(schemaJSON, &schema); err != nil {
		t.Fatal(err)
	}
	valuesJSON, err := yaml.YAMLToJSON(valuesYAML)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(valuesJSON, &values); err != nil {
		t.Fatal(err)
	}
	var walk func(node map[string]any, path []string)
	walk = func(node map[string]any, path []string) {
		if def, ok := node["default"]; ok && len(path) > 0 {
			compared++
			var cur any = values
			found := true
			for _, k := range path {
				m, ok := cur.(map[string]any)
				if !ok {
					found = false
					break
				}
				if cur, ok = m[k]; !ok {
					found = false
					break
				}
			}
			switch {
			case !found:
				mismatches = append(mismatches, strings.Join(path, ".")+": schema default for a key values.yaml does not define")
			case !reflect.DeepEqual(def, cur):
				mismatches = append(mismatches, strings.Join(path, ".")+": schema default differs from values.yaml")
			}
		}
		props, _ := node["properties"].(map[string]any)
		for k, child := range props {
			if c, ok := child.(map[string]any); ok {
				walk(c, append(append([]string{}, path...), k))
			}
		}
	}
	walk(schema, nil)
	return mismatches, compared
}

// core-provider#479: helm does not apply schema defaults, but crdgen, UI forms and values generators
// do. A schema default that disagrees with values.yaml is therefore a second, stale source of truth
// (acmp's zendesk-kog 0.2.2 CrashLooped on krateo-057 from one: tag "0.1.0", never published). Here
// cdc.image.tag was "0.20.2" against "" and chartInspector.service.port 80 against 8081.
func TestValuesSchemaDefaultsMatchValues(t *testing.T) {
	schemaJSON, err := os.ReadFile(filepath.Join(chartDir, "values.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	valuesYAML, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	mismatches, compared := schemaDefaultMismatches(t, schemaJSON, valuesYAML)
	if compared == 0 {
		t.Fatal("compared 0 schema defaults: the walk found nothing, so this test would pass vacuously")
	}
	for _, m := range mismatches {
		t.Error(m)
	}
	t.Logf("compared %d schema defaults against values.yaml", compared)
}

// The guard fires on both defects it exists for (control): a stale default and a default for an
// undefined key; and stays quiet on an agreeing one, numbers included.
func TestSchemaDefaultMismatchesFiresOnTheDefect(t *testing.T) {
	schema := []byte(`{"properties":{"cdc":{"properties":{
		"image":{"properties":{"tag":{"default":"0.20.2"}}},
		"port":{"default":8081},
		"autoscaling":{"properties":{"enabled":{"default":false}}}}}}}`)
	values := []byte("cdc:\n  image:\n    tag: \"\"\n  port: 8081\n")
	got, compared := schemaDefaultMismatches(t, schema, values)
	if compared != 3 || len(got) != 2 {
		t.Fatalf("compared %d, mismatches %v; want 3 compared, the stale tag and the undefined key", compared, got)
	}
}
