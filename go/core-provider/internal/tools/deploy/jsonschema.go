package deploy

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// renderJSONSchemaConfigMap renders the per-version ConfigMap that carries the chart's
// values.schema.json, with the schema stored as COMPACT JSON (#485).
//
// Why compact. A ConfigMap's data may not exceed 1 MiB (1,048,576 bytes), and the chart's file is
// stored verbatim, pretty-printed. installer's schema crossed it at chart 0.3.634 (1,049,383 B;
// 0.3.635 is 1,059,606 B) and its version could never be deployed:
//
//	ConfigMap "installers-v0-3-634-jsonschema-configmap" is invalid: []: Too long: may not be more than 1048576 bytes
//
// Compact, the same documents are 601,833 and 608,420 B. The CRD then binds first: it carries the
// same schema once per served version, against etcd's ~1.5 MiB request limit (#484).
//
// Why json.Compact and never a decode/re-encode. The value is read as TEXT by the portal's
// blueprint form, which splices identity fields into it and hands it to the Form widget precisely
// to keep the AUTHORING order of `properties` (the field order users see). json.Compact removes
// insignificant whitespace only: keys stay in order and number literals stay as written. Decoding
// into a map and re-encoding would sort every blueprint's fields alphabetically and could rewrite
// numbers. The key stays "values.schema.json".
//
// Only Deploy writes this content. Undeploy and the digest lookup render the ConfigMap for its
// name and namespace alone, so they do not go through here.
func renderJSONSchemaConfigMap(gvr schema.GroupVersionResource, nn types.NamespacedName, templatePath string, schemaBytes []byte) (corev1.ConfigMap, error) {
	var compact bytes.Buffer
	if err := json.Compact(&compact, schemaBytes); err != nil {
		return corev1.ConfigMap{}, fmt.Errorf("compacting the chart's values.schema.json: %w", err)
	}
	cm := corev1.ConfigMap{}
	if err := objects.CreateK8sObject(&cm, gvr, nn, templatePath, "schema", compact.String()); err != nil {
		return corev1.ConfigMap{}, fmt.Errorf("error creating ConfigMap for JSON schema: %w", err)
	}
	return cm, nil
}
