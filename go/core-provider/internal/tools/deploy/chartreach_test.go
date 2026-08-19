package deploy

import "testing"

func TestIsClusterLocalChartURL(t *testing.T) {
	clusterLocal := []string{
		"oci://installer-core-provider-registry.krateo-system.svc.cluster.local:5000/charts/demo:1.0",
		"oci://registry.krateo-system.svc/charts/demo",
		"http://charts.default.svc.cluster.local/demo.tgz",
		"https://myregistry.local/charts/demo",
		"oci://localhost:5000/charts/demo",
		"http://127.0.0.1:8080/demo.tgz",
		"oci://10.96.1.10:5000/charts/demo",       // RFC-1918
		"oci://192.168.1.5/charts/demo",           // RFC-1918
		"oci://172.16.0.9:5000/charts/demo",       // RFC-1918
		"registry.krateo-system.svc.cluster.local/charts/demo", // scheme-less
	}
	for _, u := range clusterLocal {
		if !IsClusterLocalChartURL(u) {
			t.Errorf("expected %q to be cluster-local", u)
		}
	}

	reachable := []string{
		"oci://ghcr.io/example/krateo/demo:1.0",
		"https://charts.example.com/demo.tgz",
		"oci://registry-1.docker.io/library/demo",
		"oci://8.8.8.8:5000/charts/demo", // public IP
		"ghcr.io/example/krateo/demo", // scheme-less public
		"",                               // unknown host -> fail open
	}
	for _, u := range reachable {
		if IsClusterLocalChartURL(u) {
			t.Errorf("expected %q to be reachable (not cluster-local)", u)
		}
	}
}
