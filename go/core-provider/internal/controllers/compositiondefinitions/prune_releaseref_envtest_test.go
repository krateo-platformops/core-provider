//go:build envtest

package compositiondefinitions

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/apis"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// storedHelmRelease writes a helm release Secret the way helm's storage driver does (JSON, gzip,
// base64, type helm.sh/release.v1, label owner=helm), with only the fields releaseref reads.
func storedHelmRelease(t *testing.T, ctx context.Context, cl client.Client, ns, release string, revision int, status, manifest string) {
	t.Helper()
	doc, err := json.Marshal(map[string]any{
		"name": release, "version": revision, "manifest": manifest,
		"info": map[string]any{"status": status},
	})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(doc); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, revision), Namespace: ns,
			Labels: map[string]string{"owner": "helm", "name": release},
		},
		Type: corev1.SecretType("helm.sh/release.v1"),
		Data: map[string][]byte{"release": []byte(base64.StdEncoding.EncodeToString(buf.Bytes()))},
	}
	if err := cl.Create(ctx, sec); err != nil {
		t.Fatalf("create release secret: %v", err)
	}
	t.Cleanup(func() { _ = cl.Delete(context.Background(), sec) })
}

// #486, through prunableServedVersions on a real apiserver: a deployed helm revision that declares
// ANOTHER Kind at the old version's apiVersion does not hold it (the krateo-057 false keep), and
// one that declares THIS CRD's Kind there does. A control first proves the old version is prunable
// with no releases at all, so "still prunable" after the other-Kind release is the Kind filter and
// not an eligibility the test failed to set up; the last arm proves the term still fires.
func TestEnvtest_ReleaseRevisionHoldsOnlyItsOwnKind(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	sc := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(sc)
	_ = apiextensionsv1.AddToScheme(sc)
	_ = apis.AddToScheme(sc)
	cl, err := client.New(cfg, client.Options{Scheme: sc})
	if err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	curGVK, gvrV1, _, _ := buildTwoVersionCRD(t, ctx, cl, dyn)
	e := newExternal(cl, dyn)
	old := compositionGroup + "/v1-0-0"

	decide := func() (prunable, kept []string) {
		t.Helper()
		p, k, err := e.prunableServedVersions(ctx, curGVK, gvrV1, "self", "default")
		if err != nil {
			t.Fatalf("prunableServedVersions: %v", err)
		}
		return p, k
	}

	if p, k := decide(); !contains(p, "v1-0-0") {
		t.Fatalf("control: with no releases v1-0-0 must be prunable (prunable=%v kept=%v)", p, k)
	}

	// Another composition Kind at the same apiVersion, in a DEPLOYED revision.
	storedHelmRelease(t, ctx, cl, "default", "umbrella", 7, "deployed",
		"apiVersion: "+old+"\nkind: SomeOtherKind\nmetadata:\n  name: other\n")
	if p, k := decide(); !contains(p, "v1-0-0") {
		t.Errorf("a revision declaring SomeOtherKind at %s held %s's v1-0-0 (prunable=%v kept=%v): "+
			"helm maps that document by its own Kind, so pruning this Kind's version cannot strand it (#486)",
			old, curGVK.Kind, p, k)
	}

	// This CRD's own Kind at the old apiVersion: the #305 wedge shape, which must still hold.
	storedHelmRelease(t, ctx, cl, "default", "parent", 3, "deployed",
		"apiVersion: "+old+"\nkind: "+curGVK.Kind+"\nmetadata:\n  name: child\n")
	p, k := decide()
	if contains(p, "v1-0-0") {
		t.Fatalf("a deployed revision declaring %s at %s did not hold v1-0-0 (prunable=%v kept=%v): the #305 wedge",
			curGVK.Kind, old, p, k)
	}
	held := false
	for _, r := range k {
		if strings.HasPrefix(r, "v1-0-0:release-revisions=1:") && strings.Contains(r, "default/parent(rev 3") {
			held = true
		}
	}
	if !held {
		t.Errorf("kept = %v, want exactly one release-revision reason naming default/parent rev 3 (the other-Kind umbrella must not be counted)", k)
	}
}
