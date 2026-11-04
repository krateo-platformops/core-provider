package releaseref

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const probeVersion = "composition.krateo.io/v0-6-1"

// releaseSecret builds a helm release Secret the way helm's storage driver does: JSON, gzip,
// base64. compress=false exercises helm's BACKWARDS-COMPAT path, where the gzip magic header is
// absent and decompression is skipped.
func releaseSecret(t *testing.T, ns, release string, revision int, manifest string, compress bool) *corev1.Secret {
	t.Helper()
	payload, err := json.Marshal(storedRelease{Name: release, Version: revision, Manifest: manifest})
	if err != nil {
		t.Fatalf("marshalling release: %v", err)
	}
	if compress {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(payload); err != nil {
			t.Fatalf("gzip write: %v", err)
		}
		if err := zw.Close(); err != nil {
			t.Fatalf("gzip close: %v", err)
		}
		payload = buf.Bytes()
		// PREMISE: if this is not helm's magic header, decodeRelease takes the uncompressed branch
		// and the test proves nothing about compression.
		if !bytes.Equal(payload[0:3], magicGzip) {
			t.Fatalf("fixture is not gzip-framed as helm expects: %x", payload[0:3])
		}
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sh.helm.release.v1." + release + ".v" + string(rune('0'+revision)),
			Namespace: ns,
			Labels:    map[string]string{helmOwnerLabel: helmOwnerValue},
		},
		Type: corev1.SecretType(helmReleaseSecretType),
		Data: map[string][]byte{
			releaseDataKey: []byte(base64.StdEncoding.EncodeToString(payload)),
		},
	}
}

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// A parent composition's chart renders CHILD composition CRs. This is the manifest shape that
// wedged the parent in #305.
const parentManifest = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: parent-config
---
apiVersion: composition.krateo.io/v0-6-1
kind: BaremetalHost
metadata:
  name: node-1
---
apiVersion: composition.krateo.io/v0-6-1
kind: BaremetalHost
metadata:
  name: node-2
---
apiVersion: composition.krateo.io/v0-6-1
kind: NodeProfile
metadata:
  name: profile-a
`

func TestReferencingReleases_FindsTheWedgeShape(t *testing.T) {
	c := newClient(t, releaseSecret(t, "krateo-system", "parent", 3, parentManifest, true))

	refs, err := ReferencingReleases(context.Background(), c, probeVersion)
	if err != nil {
		t.Fatalf("ReferencingReleases: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d references, want 1: %v", len(refs), refs)
	}
	got := refs[0]
	if got.Release != "parent" || got.Revision != 3 {
		t.Errorf("release/revision = %q/%d, want parent/3", got.Release, got.Revision)
	}
	// Kinds deduplicated: two BaremetalHosts, one entry. The kinds are what make the refusal
	// diagnosable, so an empty or duplicated list is a defect even though the veto still works.
	want := []string{"BaremetalHost", "NodeProfile"}
	if strings.Join(got.Kinds, ",") != strings.Join(want, ",") {
		t.Errorf("kinds = %v, want %v (deduplicated, first-encountered order)", got.Kinds, want)
	}
}

// THE ROW A SUBSTRING IMPLEMENTATION FAILS, and the reason this function parses documents.
//
// The version string appears three times here and NOT ONCE as an object's apiVersion: in an
// annotation, in a ConfigMap's data, and inside an RBAC-ish list. helm would never try to map any
// of them. A strings.Contains over the manifest reports a reference, refuses a perfectly safe
// prune, and the next person loosens the predicate because it over-refuses.
const decoyManifest = `
apiVersion: v1
kind: ConfigMap
metadata:
  name: decoy
  annotations:
    krateo.io/previous-version: composition.krateo.io/v0-6-1
data:
  note: "migrated away from composition.krateo.io/v0-6-1"
  rendered: |
    apiVersion: composition.krateo.io/v0-6-1
    kind: BaremetalHost
---
apiVersion: composition.krateo.io/v0-6-2
kind: BaremetalHost
metadata:
  name: node-1
`

func TestReferencingReleases_IgnoresTheVersionStringOutsideAnApiVersion(t *testing.T) {
	c := newClient(t, releaseSecret(t, "krateo-system", "decoy", 1, decoyManifest, true))

	// CONTROL: the probe version really is present as text, or this test proves nothing about
	// discrimination — it would pass against an implementation that never matches anything.
	if !strings.Contains(decoyManifest, probeVersion) {
		t.Fatal("premise: the fixture must contain the version as a substring")
	}

	refs, err := ReferencingReleases(context.Background(), c, probeVersion)
	if err != nil {
		t.Fatalf("ReferencingReleases: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("got %d references, want 0: %v.\nThe version appears only in an annotation, in "+
			"ConfigMap data, and in an embedded document — never as an object's apiVersion. A "+
			"substring matcher reports a reference here and blocks a safe prune forever", len(refs), refs)
	}

	// And the version that IS declared must still be found, so the negative above is discrimination
	// rather than blindness.
	refs, err = ReferencingReleases(context.Background(), c, "composition.krateo.io/v0-6-2")
	if err != nil {
		t.Fatalf("ReferencingReleases (v0-6-2): %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d references for the DECLARED version, want 1 — the negative row above is "+
			"only meaningful if this one is positive", len(refs))
	}
}

// helm stores some releases UNCOMPRESSED (anything written before compression was introduced; its
// decoder checks for the magic header rather than assuming it). A decoder that assumed gzip would
// error on those — and since an error here refuses the prune, the symptom would be "this version
// can never be pruned" rather than a crash, which is the kind of thing that goes unnoticed for
// months.
func TestReferencingReleases_HandlesUncompressedReleases(t *testing.T) {
	c := newClient(t, releaseSecret(t, "krateo-system", "legacy", 1, parentManifest, false))

	refs, err := ReferencingReleases(context.Background(), c, probeVersion)
	if err != nil {
		t.Fatalf("an uncompressed release must decode, got: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("got %d references from an uncompressed release, want 1", len(refs))
	}
}

// A LIST FAILURE MUST BE AN ERROR, NEVER ZERO REFERENCES. This is the arm that matters most: the
// caller prunes on an empty result, so "I could not list" answered as "nothing references it"
// deletes a version that is in use.
func TestReferencingReleases_ListFailureIsAnErrorNotZero(t *testing.T) {
	boom := errors.New("apiserver unavailable")
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	c := fake.NewClientBuilder().WithScheme(s).
		WithObjects(releaseSecret(t, "krateo-system", "parent", 1, parentManifest, true)).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return boom
			},
		}).Build()

	refs, err := ReferencingReleases(context.Background(), c, probeVersion)
	if err == nil {
		t.Fatalf("a list failure returned refs=%v and NO error. The caller prunes on an empty "+
			"result, so this silently deletes a served version that is still referenced", refs)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap the list failure so the cause is diagnosable", err)
	}
	if refs != nil {
		t.Errorf("refs = %v on error, want nil: a partial answer invites a caller to use it", refs)
	}
}

// A release payload we cannot decode is also "could not determine". Asserted on the ERROR FAMILY
// rather than on a message, so the test survives a reworded wrap.
func TestReferencingReleases_UndecodableReleaseIsAnError(t *testing.T) {
	for _, tc := range []struct {
		name string
		data map[string][]byte
	}{
		{"not base64", map[string][]byte{releaseDataKey: []byte("!!!not-base64!!!")}},
		{"base64 of nonsense", map[string][]byte{releaseDataKey: []byte(base64.StdEncoding.EncodeToString([]byte("not json")))}},
		{"no release key at all", map[string][]byte{"other": []byte("x")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sec := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name: "sh.helm.release.v1.broken.v1", Namespace: "krateo-system",
					Labels: map[string]string{helmOwnerLabel: helmOwnerValue},
				},
				Type: corev1.SecretType(helmReleaseSecretType),
				Data: tc.data,
			}
			c := newClient(t, sec)

			refs, err := ReferencingReleases(context.Background(), c, probeVersion)
			if err == nil {
				t.Errorf("got refs=%v and no error; an undecodable release means we cannot see "+
					"what it manages, and that must refuse the prune", refs)
			}
		})
	}
}

// An empty apiVersion is a CALLER BUG, and answering "unreferenced" would turn it into a deletion
// of every version. Refuse instead.
func TestReferencingReleases_EmptyApiVersionRefuses(t *testing.T) {
	c := newClient(t, releaseSecret(t, "krateo-system", "parent", 1, parentManifest, true))

	for _, v := range []string{"", "   "} {
		if _, err := ReferencingReleases(context.Background(), c, v); err == nil {
			t.Errorf("apiVersion %q returned no error: a caller that computed an empty version has "+
				"a bug, and 'unreferenced' would make it destructive", v)
		}
	}
}

// Secrets that are not helm releases must be ignored rather than decoded. A Secret carrying
// owner=helm but a different type is the shape that would otherwise produce a spurious decode
// error and block every prune.
func TestReferencingReleases_IgnoresNonReleaseSecrets(t *testing.T) {
	impostor := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "owner-helm-but-not-a-release", Namespace: "krateo-system",
			Labels: map[string]string{helmOwnerLabel: helmOwnerValue},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"release": []byte("garbage that would fail to decode")},
	}
	c := newClient(t, impostor)

	refs, err := ReferencingReleases(context.Background(), c, probeVersion)
	if err != nil {
		t.Fatalf("a non-release Secret must be skipped, not decoded: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("got %d references from a non-release Secret", len(refs))
	}
}

// An empty manifest is legitimately zero references (a release that rendered nothing), and must not
// be confused with a manifest we failed to read.
func TestKindsAtAPIVersion_EmptyManifestIsZeroNotAnError(t *testing.T) {
	kinds, err := kindsAtAPIVersion("", probeVersion)
	if err != nil {
		t.Fatalf("empty manifest: %v", err)
	}
	if len(kinds) != 0 {
		t.Errorf("kinds = %v, want none", kinds)
	}
	// A comment-only document decodes to null and zero-values the struct, which is not an error.
	kinds, err = kindsAtAPIVersion("# nothing here\n", probeVersion)
	if err != nil {
		t.Fatalf("comment-only manifest must not error: %v", err)
	}
	if len(kinds) != 0 {
		t.Errorf("kinds = %v, want none", kinds)
	}
}
