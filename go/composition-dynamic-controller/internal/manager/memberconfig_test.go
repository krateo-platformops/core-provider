package manager

import (
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/composition"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// One parser for a dedicated controller's environment and a pool member's ConfigMap: these are the
// keys core-provider renders (helm/core-provider/assets/cdc/configmap.yaml).
func TestMemberFromConfig(t *testing.T) {
	var got struct {
		name, ns string
		extras   map[string]any
		calls    int
	}
	resolver := func(name, ns string, extras map[string]any) composition.APIResolver {
		got.name, got.ns, got.extras, got.calls = name, ns, extras, got.calls+1
		return nil
	}
	full := map[string]string{
		KeyGroup: "composition.krateo.io", KeyVersion: "v1-8-129", KeyResource: "portals",
		KeyLeaseName:          "v1-8-129.portals.composition.krateo.io",
		KeyStatusDataTemplate: `[{"forPath":"url","expression":"${ .api.u }"}]`,
		KeyAPIRefName:         "portal-api", KeyAPIRefNamespace: "krateo-system", KeyAPIRefExtras: `{"a":1}`,
		KeyWorkers: "3", KeyResyncInterval: "90s",
	}
	m, err := MemberFromConfig(full, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.GVR != (schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-8-129", Resource: "portals"}) ||
		m.LeaseName != "v1-8-129.portals.composition.krateo.io" || len(m.StatusDataTemplate) != 1 ||
		m.StatusDataTemplate[0].ForPath != "url" || m.Workers != 3 || m.ResyncInterval != 90*time.Second {
		t.Fatalf("member = %+v", m)
	}
	if got.calls != 1 || got.name != "portal-api" || got.ns != "krateo-system" || got.extras["a"] != float64(1) {
		t.Fatalf("resolver called with %+v", got)
	}

	t.Run("no apiRef, no resolver", func(t *testing.T) {
		got.calls = 0
		if _, err := MemberFromConfig(map[string]string{KeyVersion: "v1", KeyResource: "x"}, resolver, nil); err != nil || got.calls != 0 {
			t.Fatalf("err=%v calls=%d", err, got.calls)
		}
	})
	t.Run("a member that names no Kind x version is refused", func(t *testing.T) {
		for _, data := range []map[string]string{{}, {KeyResource: "x"}, {KeyVersion: "v1"}} {
			if _, err := MemberFromConfig(data, resolver, nil); err == nil {
				t.Errorf("%v accepted", data)
			}
		}
	})
	t.Run("a bad projection or extras degrades with a warning, the member still starts", func(t *testing.T) {
		var warned []string
		m, err := MemberFromConfig(map[string]string{KeyVersion: "v1", KeyResource: "x", KeyStatusDataTemplate: "{not json",
			KeyAPIRefName: "r", KeyAPIRefExtras: "{nope"}, resolver, func(msg string, _ error) { warned = append(warned, msg) })
		if err != nil || m.StatusDataTemplate != nil || len(warned) != 2 {
			t.Fatalf("err=%v template=%v warned=%v", err, m.StatusDataTemplate, warned)
		}
	})
	t.Run("bad workers or resync are refused, not defaulted", func(t *testing.T) {
		for k, v := range map[string]string{KeyWorkers: "0", KeyResyncInterval: "soon"} {
			_, err := MemberFromConfig(map[string]string{KeyVersion: "v1", KeyResource: "x", k: v}, resolver, nil)
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Errorf("%s=%s: err=%v", k, v, err)
			}
		}
	})
}

func TestParseIndex(t *testing.T) {
	got, err := ParseIndex(map[string]string{
		"portals-v1-8-129":   `{"configMap":"portals-v1-8-129-configmap","checksum":"abc"}`,
		"broken-v1":          `portals-v1-configmap`,
		"empty-configmap-v1": `{"configMap":"","checksum":"x"}`,
	})
	if err == nil || !strings.Contains(err.Error(), "broken-v1") || !strings.Contains(err.Error(), "empty-configmap-v1") {
		t.Fatalf("unreadable entries not reported: %v", err)
	}
	if len(got) != 1 || got["portals-v1-8-129"] != (IndexEntry{ConfigMap: "portals-v1-8-129-configmap", Checksum: "abc"}) {
		t.Fatalf("parsed = %+v", got)
	}
}
