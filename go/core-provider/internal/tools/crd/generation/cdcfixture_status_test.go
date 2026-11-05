package generation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"sigs.k8s.io/yaml"
)

// THE CDC'S TEST CRDS MUST DECLARE THE STATUS THE CDC WRITES. A structural CRD prunes every status
// field it does not declare, so a fixture that lags statics/status.schema.json makes the cdc's
// integration suite test a world production does not have. That happened: the fixtures under
// go/composition-dynamic-controller/testdata/crds lacked 8 baseline fields, managedDigest among them.
// The apiserver stripped the stamp, the #273 check reported "the recorded set carries no digest" on
// every pass, and three SelfHeal assessments failed on main. Nobody saw, because no CI job runs
// `-tags integration`. This is #293's production failure, reproduced in the fixture.
//
// This guard is a plain unit test, so it runs where the integration suite does not, and it names the
// fixture and the missing field.
func TestCDCFixtureCRDsDeclareTheBaselineStatus(t *testing.T) {
	var baseline struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(statusJsonSchema, &baseline); err != nil {
		t.Fatalf("parsing statics/status.schema.json: %v", err)
	}
	if len(baseline.Properties) == 0 {
		t.Fatal("the baseline declares no status properties — the guard would pass vacuously")
	}

	root := filepath.Join("..", "..", "..", "..", "..", "composition-dynamic-controller", "testdata", "crds")
	files, err := filepath.Glob(filepath.Join(root, "*", "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Kind string `json:"kind"`
			Spec struct {
				Group    string `json:"group"`
				Versions []struct {
					Name   string `json:"name"`
					Schema struct {
						OpenAPIV3Schema struct {
							Properties map[string]struct {
								Properties map[string]json.RawMessage `json:"properties"`
							} `json:"properties"`
						} `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if crd.Kind != "CustomResourceDefinition" || crd.Spec.Group != "composition.krateo.io" {
			continue
		}
		for _, v := range crd.Spec.Versions {
			checked++
			declared := v.Schema.OpenAPIV3Schema.Properties["status"].Properties
			var missing []string
			for k := range baseline.Properties {
				if _, ok := declared[k]; !ok {
					missing = append(missing, k)
				}
			}
			sort.Strings(missing)
			if len(missing) > 0 {
				t.Errorf("%s (version %s) does not declare status fields the cdc writes: %v. The apiserver "+
					"will prune them, and the cdc integration suite will test behaviour production does not have",
					f, v.Name, missing)
			}
		}
	}
	// Denominator: a glob that matched nothing would pass every assertion above.
	if checked == 0 {
		t.Fatalf("found no composition.krateo.io CRD versions under %s (%d files) — the path is wrong, not the fixtures clean", root, len(files))
	}
	t.Logf("checked %d composition CRD version(s) in %d file(s) against %d baseline status fields", checked, len(files), len(baseline.Properties))
}
