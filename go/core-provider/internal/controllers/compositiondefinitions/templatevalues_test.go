package compositiondefinitions

// The keys core-provider passes the controller Deployment and env ConfigMap templates, all unset ("").
// A template is rendered with missingkey=error (#651), so a test that renders one passes every key, with
// its own pairs after these (a later pair wins). deploy's TestEveryTemplateKeyIsDeclared holds the keys
// themselves.
func deploymentTemplateValues(pairs ...any) []any {
	return append([]any{"serviceAccountName", "", "api_ref_name", "", "config_checksum", "", "workers", "", "resyncInterval", "", "resources", ""}, pairs...)
}

func envConfigMapTemplateValues(pairs ...any) []any {
	return append([]any{"composition_controller_sa_name", "", "composition_controller_sa_namespace", "", "status_data_template", "",
		"api_ref_name", "", "api_ref_namespace", "", "api_ref_extras", "", "authn_url", "", "pool", "", "workers", "", "resyncInterval", ""}, pairs...)
}
