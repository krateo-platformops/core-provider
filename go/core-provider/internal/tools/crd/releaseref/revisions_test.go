package releaseref

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func secretsAsObjects(in []*corev1.Secret) []client.Object {
	out := make([]client.Object, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

// releaseSecretWithStatus is releaseSecret with helm's release status recorded (#471), uncompressed
// (the compression branch is covered by releaseSecret's own tests).
func releaseSecretWithStatus(t *testing.T, ns, release string, revision int, status, manifest string) *corev1.Secret {
	t.Helper()
	rel := storedRelease{Name: release, Version: revision, Manifest: manifest}
	rel.Info.Status = status
	payload, err := json.Marshal(rel)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("sh.helm.release.v1.%s.v%d", release, revision),
			Namespace: ns,
			Labels:    map[string]string{helmOwnerLabel: helmOwnerValue},
		},
		Type: helmReleaseSecretType,
		Data: map[string][]byte{releaseDataKey: []byte(base64.StdEncoding.EncodeToString(payload))},
	}
}

func childAt(version string) string {
	return "apiVersion: composition.krateo.io/" + version + "\nkind: Glance\nmetadata:\n  name: glance\n"
}

// #471: only the revisions helm's automatic paths read hold a version: each release's LATEST and its
// last DEPLOYED revision. Each arm is a history shape; the first is osh-s2's, measured.
func TestReferencingReleases_OnlyLatestAndLastDeployedHold(t *testing.T) {
	old := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-3-49", Kind: "Glance"}
	cur := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-3-50", Kind: "Glance"}
	for _, tc := range []struct {
		name    string
		history []*corev1.Secret
		probe   schema.GroupVersionKind
		want    []int // revisions that hold probe
	}{
		{"osh-s2: two superseded revisions at the old version, latest deployed at the new", []*corev1.Secret{
			releaseSecretWithStatus(t, "openstack", "umbrella", 2, "superseded", childAt("v0-3-49")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 3, "superseded", childAt("v0-3-49")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 4, "deployed", childAt("v0-3-50")),
		}, old, nil},
		{"control: the same history holds the CURRENT version through the latest", []*corev1.Secret{
			releaseSecretWithStatus(t, "openstack", "umbrella", 2, "superseded", childAt("v0-3-49")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 4, "deployed", childAt("v0-3-50")),
		}, cur, []int{4}},
		{"a failed latest at the new version: the last deployed (old) holds, as helm's upgrade reads it", []*corev1.Secret{
			releaseSecretWithStatus(t, "openstack", "umbrella", 3, "deployed", childAt("v0-3-49")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 4, "failed", childAt("v0-3-50")),
		}, old, []int{3}},
		{"a pending latest at the old version holds (the cdc rolls back to the latest)", []*corev1.Secret{
			releaseSecretWithStatus(t, "openstack", "umbrella", 3, "deployed", childAt("v0-3-50")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 4, "pending-upgrade", childAt("v0-3-49")),
		}, old, []int{4}},
		{"no deployed revision at all: only the latest holds", []*corev1.Secret{
			releaseSecretWithStatus(t, "openstack", "umbrella", 1, "failed", childAt("v0-3-49")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 2, "failed", childAt("v0-3-50")),
		}, old, nil},
		{"releases are told apart by namespace: another namespace's latest still holds", []*corev1.Secret{
			releaseSecretWithStatus(t, "openstack", "umbrella", 4, "deployed", childAt("v0-3-50")),
			releaseSecretWithStatus(t, "openstack", "umbrella", 3, "superseded", childAt("v0-3-49")),
			releaseSecretWithStatus(t, "other", "umbrella", 1, "deployed", childAt("v0-3-49")),
		}, old, []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClient(t, secretsAsObjects(tc.history)...)
			refs, err := ReferencingReleases(context.Background(), c, tc.probe)
			if err != nil {
				t.Fatal(err)
			}
			var got []int
			for _, r := range refs {
				got = append(got, r.Revision)
			}
			sort.Ints(got)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("holding revisions = %v, want %v (refs %v)", got, tc.want, refs)
			}
		})
	}
}

// A superseded revision is still DECODED: whether it is superseded is only known from the whole
// history, so an undecodable one is still a refusal, as before #471.
func TestReferencingReleases_UndecodableSupersededRevisionStillRefuses(t *testing.T) {
	bad := releaseSecretWithStatus(t, "openstack", "umbrella", 2, "superseded", childAt("v0-3-49"))
	bad.Data[releaseDataKey] = []byte("not base64 !!")
	c := newClient(t, secretsAsObjects([]*corev1.Secret{bad,
		releaseSecretWithStatus(t, "openstack", "umbrella", 4, "deployed", childAt("v0-3-50"))})...)
	if _, err := ReferencingReleases(context.Background(), c, schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v0-3-49", Kind: "Glance"}); err == nil || !strings.Contains(err.Error(), "decoding release secret") {
		t.Errorf("err = %v, want a decode refusal", err)
	}
}
