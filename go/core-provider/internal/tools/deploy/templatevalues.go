package deploy

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// THE VALUES EACH CONTROLLER TEMPLATE IS RENDERED WITH, one function per template (#651), beside
// cdcDeploymentValues (the Deployment's). A template is rendered with missingkey=error, so a key it uses
// and its function does not pass refuses the render, whichever branch of the template is taken: a chart
// and a binary that do not belong together fail before anything rendered from them is applied.
//
// So EVERY key is always passed. One that does not apply is "", which every template reads as unset
// (`if` is false, `default` applies). Absent would be an error, and nil would print "<no value>".
// TestEveryTemplateKeyIsDeclared holds these functions against the shipped chart. The keys every template
// gets are templates.Values.

// unset is the value of a declared key that does not apply.
const unset = ""

// bindingValues: clusterrolebinding.yaml, compositiondefinition-rolebinding.yaml,
// secret-rolebinding.yaml, lease-rolebinding.yaml.
func bindingValues(saName, saNamespace string) []any {
	return []any{"serviceAccount", saName, "saNamespace", saNamespace}
}

// secretRoleValues: secret-role.yaml.
func secretRoleValues(secretName string) []any {
	return []any{"secretName", secretName}
}

// schemaConfigMapValues: the per-definition JSON schema ConfigMap.
func schemaConfigMapValues(schema string) []any {
	return []any{"schema", schema}
}

// envConfigMapValues: a controller's env ConfigMap (assets/cdc/configmap.yaml), the same for a dedicated
// controller and a pool member. The template renders the member keys (workers and resync interval, when
// the definition sets them, manager.MemberFromConfig) only for a pool member: a dedicated controller takes
// those as arguments, and its ConfigMap, and the checksum its pod carries, do not move with them
// (TestPoolMemberConfigMapKeys).
func envConfigMapValues(saName, saNamespace string, o DeployOptions) []any {
	var workers, resyncInterval any = unset, unset
	if c := o.Controller; c != nil {
		if c.Workers != nil {
			workers = *c.Workers
		}
		if c.ResyncInterval != nil {
			resyncInterval = *c.ResyncInterval
		}
	}
	return []any{
		"composition_controller_sa_name", saName,
		"composition_controller_sa_namespace", saNamespace,
		"status_data_template", o.StatusDataTemplate,
		"api_ref_name", o.ApiRefName,
		"api_ref_namespace", o.ApiRefNamespace,
		"api_ref_extras", o.ApiRefExtras,
		"authn_url", o.ControllerAuthnURL,
		"pool", o.Pool,
		"workers", workers,
		"resyncInterval", resyncInterval,
	}
}

// deploymentIdentityValues: the controller Deployment rendered only to be found (Lookup) or deleted
// (Undeploy) by name. Nothing that shapes the pod is passed.
func deploymentIdentityValues(saName, apiRefName string) []any {
	return []any{
		"serviceAccountName", saName,
		"api_ref_name", apiRefName,
		"config_checksum", unset,
		"workers", unset,
		"resyncInterval", unset,
		"resources", unset,
	}
}

// authnMapping is what an authn ServiceAccount mapping (authn-serviceaccount.yaml) says: the
// controller's, or core-provider's own, which has no principal and no delegation.
type authnMapping struct {
	saName, saNamespace, group, displayName, principal string
	// "" or as delegationValues renders them.
	delegation, delegationAllowed, delegationMaxDepth string
}

func (m authnMapping) values() []any {
	return []any{
		"saName", m.saName,
		"saNamespace", m.saNamespace,
		"group", m.group,
		"displayName", m.displayName,
		"principal", m.principal,
		"delegation", m.delegation,
		"delegationAllowed", m.delegationAllowed,
		"delegationMaxDepth", m.delegationMaxDepth,
	}
}

// poolConfigMapValues: a pool's process ConfigMap (assets/cdc/pool-configmap.yaml).
func poolConfigMapValues(o PoolOptions, pool string, sizing PoolSizing) []any {
	names, sa := PoolNames(pool), PoolServiceAccount(pool, o.Namespace)
	return []any{
		"pool", pool,
		"poolConfigMap", names.EnvConfigMap,
		"poolDeployment", names.Workload,
		"poolIndex", names.Index,
		"poolWorkers", sizing.Workers,
		"composition_controller_sa_name", sa.Name,
		"composition_controller_sa_namespace", sa.Namespace,
		"authn_url", o.Assets.AuthnURL,
	}
}

// poolDeploymentValues: a pool's Deployment (assets/cdc/pool-deployment.yaml). resources is JSON;
// processEnv is the pool's process ConfigMap data, whose checksum rolls the pod.
func poolDeploymentValues(o PoolOptions, pool, resources string, processEnv map[string]string) []any {
	names := PoolNames(pool)
	return []any{
		"pool", pool,
		"poolDeployment", names.Workload,
		"poolConfigMap", names.EnvConfigMap,
		"poolServiceAccount", PoolServiceAccount(pool, o.Namespace).Name,
		"poolResources", resources,
		"config_checksum", configChecksum(processEnv),
	}
}

// serviceTemplateMounted reports whether the chart mounts the controller Service template, which it does
// only with cdc.metrics.enabled. Only a file that does not exist says it does not: any other error is
// returned, never read as "no Service", which on Deploy dropped the Service from the digest and on
// Undeploy left it behind (#651).
func serviceTemplateMounted(path string) (bool, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading whether the controller Service template is mounted: %w", err)
	}
	return true, nil
}
