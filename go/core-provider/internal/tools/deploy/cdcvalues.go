package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

// cdcDeploymentValues are the stage-2 values the per-version controller Deployment is rendered with.
//
// Per-Kind tuning (spec.controller) is unset ("") unless the definition sets it, so the template's
// chart-global defaults (workers=1, resync=3m) apply for definitions that omit them. Every key is always
// passed (templatevalues.go).
//
// config_checksum (#391) is the checksum of the env ConfigMap the pod reads through envFrom, the only
// ConfigMap it consumes. It is rendered into the pod template, so a change to that ConfigMap changes
// the template and rolls the pod IN THE SAME APPLY. It replaces the restartedAt patch that used to
// follow the apply, which was a second template write and rolled the cdc twice whenever the
// deployment controller synced between the two.
func cdcDeploymentValues(saName string, opts DeployOptions, envConfig map[string]string) ([]any, error) {
	var workers, resyncInterval, resources any = unset, unset, unset
	if c := opts.Controller; c != nil {
		if c.Workers != nil {
			workers = *c.Workers
		}
		if c.ResyncInterval != nil {
			resyncInterval = *c.ResyncInterval
		}
		if c.Resources != nil {
			// Marshal to JSON (valid YAML) and inline it into the template as `resources: {...}`.
			rj, err := json.Marshal(c.Resources)
			if err != nil {
				return nil, fmt.Errorf("marshaling spec.controller.resources: %w", err)
			}
			resources = string(rj)
		}
	}
	return []any{
		"serviceAccountName", saName,
		"api_ref_name", opts.ApiRefName,
		"config_checksum", configChecksum(envConfig),
		"workers", workers,
		"resyncInterval", resyncInterval,
		"resources", resources,
	}, nil
}

// configChecksum is a stable digest of a ConfigMap's data: keys in sorted order, each key and value
// length-prefixed so no two different maps can produce the same input.
func configChecksum(data map[string]string) string {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		fmt.Fprintf(h, "%d:%s%d:%s", len(k), k, len(data[k]), data[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}
