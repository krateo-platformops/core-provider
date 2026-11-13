// Package releaseref answers one question, for the served-version prune path: does a helm release
// revision that helm will READ still declare an object of a given Kind at a given apiVersion in its
// STORED MANIFEST? (#305, #471, #486)
//
// # Why the Kind is part of the question (#486)
//
// helm maps each stored document by its group, version AND kind ("no matches for kind
// \"BaremetalHost\" in version ..."), so pruning a version from one Kind's CRD can only strand a
// document of THAT Kind. Every composition CRD shares the composition.krateo.io group and its
// versions are chart versions, so different Kinds collide on the apiVersion all the time: on
// krateo-057 OtelCollectorTailSampling v0-1-2 stayed served because installer-platform's deployed
// revision declared ClickhouseOperator, KrateoMarketplaceCatalog and ModelConfigs at
// composition.krateo.io/v0-1-2. Matching the apiVersion alone held five CRDs' stale versions there,
// with zero instances on any of them.
//
// # Which revisions count (#471)
//
// Only the ones helm's automatic paths read: each release's LATEST revision and its last DEPLOYED
// revision. helm's Upgrade reads the latest and, when the latest is not deployed, the last deployed
// one (helm v3.20.2 pkg/action/upgrade.go:209-229); Rollback reads the latest and its target, and
// both cdc rollbacks target the latest (composition.go rollbackConfig(rel.Revision)). A SUPERSEDED
// revision older than both is read only by a manual `helm rollback <release> <N>`. Counting it held
// every old served version after every upgrade of a parent composition until helm history rotated,
// which on a quiet umbrella is never (osh-s2: v0-3-49 held by revs 2 and 3, both superseded, while
// rev 4 was latest and deployed). Diego, 2026-11-13: hold only for the latest and last deployed.
// A manual rollback to a revision whose version was pruned then fails with helm's mapping error,
// which is the right answer: it would recreate children at a version the Kind no longer serves.
//
// # Why this exists
//
// The prune predicate in prunableServedVersions kept a served version when another
// CompositionDefinition declared it, or when a composition INSTANCE carried its label. Neither term
// sees the case that wedges a parent: a parent composition's chart renders CHILD composition CRs,
// so the parent's stored helm manifest names the child's served version — six times, in the
// measured case. Prune that version and helm can no longer build the current resource list for the
// parent:
//
//	reconciling helm chart: reconcile: building current resource list:
//	[resource mapping not found for ... no matches for kind "BaremetalHost" in version
//	 "composition.krateo.io/v0-6-1" ... ensure CRDs are installed first]
//
// Ready=False, Synced=False, indefinitely. A controller restart does not clear it, and nothing the
// chart author can do avoids it: the stale reference is inside helm's own stored manifest.
//
// # Why the stored manifest and not the live objects
//
// A live child CR is already covered by the instance-label term. The manifest is a separate
// witness: it records what the release BELIEVES it manages, which is what helm reads back to
// compute a diff. A version can be absent from every live object and still be named by a manifest,
// which is exactly the wedge.
//
// # Why not rely on plumbing's repair
//
// plumbing's dropUnservedFromStoredManifest exists for this case and did not fire (#305). A repair
// that runs AFTER the prune is racing the thing it exists to survive — and its own instrument is a
// RESTMapper that may still know the pruned version, so it can report "nothing to repair" about a
// version that is already gone. Refusing to prune is cheaper and does not race: a lingering served
// version costs a little schema surface, a stranded revision costs the composition entirely.
//
// # The failure direction is not negotiable
//
// Every error here is returned, never swallowed into "no references found". A list failure, an
// undecodable release, an unparseable manifest: each means we could not determine whether the
// version is referenced, and on a prune path that must refuse. A false "unreferenced" deletes a
// version that is in use and wedges every release that named it.
package releaseref

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const (
	// helmOwnerLabel/helmOwnerValue and helmReleaseSecretType are helm's own storage-driver
	// conventions (helm.sh/helm/v3 pkg/storage/driver/secrets.go). Matched on BOTH the label and
	// the Secret type: the label narrows the List server-side, and the type is the authoritative
	// check, so a Secret that merely carries owner=helm is not decoded as a release.
	helmOwnerLabel = "owner"
	helmOwnerValue = "helm"

	helmReleaseSecretType = corev1.SecretType("helm.sh/release.v1")

	// releaseDataKey is where the driver puts the encoded release.
	releaseDataKey = "release"
)

// magicGzip is the gzip header helm checks for. Compression is OPTIONAL in this format — helm
// skips decompression when the header is absent, for releases stored before compression was
// introduced (pkg/storage/driver/util.go:68). A decoder that assumed gzip would error on those,
// and an error here is a refusal to prune, so getting this wrong fails safe but noisily. Matching
// helm's own check keeps it correct instead.
var magicGzip = []byte{0x1f, 0x8b, 0x08}

// Reference is one release revision whose stored manifest declares the asked Kind at the
// apiVersion, with enough identity to put in a log line an operator can act on.
type Reference struct {
	// SecretNamespace/SecretName locate the release revision itself.
	SecretNamespace string
	SecretName      string
	// Release and Revision are helm's own identifiers, so this can be matched against `helm
	// history`.
	Release  string
	Revision int
	// Kinds are ALL the kinds declared at the apiVersion in this manifest (the asked Kind among
	// them), deduplicated and ordered as first encountered. This is what makes the refusal
	// diagnosable rather than merely a veto.
	Kinds []string
}

func (r Reference) String() string {
	k := strings.Join(r.Kinds, ",")
	if k == "" {
		k = "<no kind>"
	}
	return fmt.Sprintf("%s/%s(rev %d, secret %s/%s, kinds %s)",
		r.SecretNamespace, r.Release, r.Revision, r.SecretNamespace, r.SecretName, k)
}

// storedRelease is a PROJECTION of helm's stored release JSON, not a reimplementation of it. Only
// the fields this question needs are declared; everything else in the document is ignored by
// encoding/json. This keeps core-provider free of a direct helm dependency for a read that needs
// three strings.
type storedRelease struct {
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Manifest string `json:"manifest"`
	// Info.Status is helm's release status ("deployed", "superseded", "failed", "pending-upgrade",
	// ...), which decides whether this revision is the release's last DEPLOYED one (#471).
	Info struct {
		Status string `json:"status"`
	} `json:"info"`
}

// statusDeployed is helm's release.StatusDeployed.
const statusDeployed = "deployed"

// manifestObject is the minimum needed to compare a document's apiVersion.
type manifestObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
}

// ReferencingReleases returns every helm release revision helm will read (each release's latest and
// last deployed revision, #471) whose stored manifest declares at least one object of gvk's Kind at
// gvk's apiVersion (e.g. BaremetalHost at "composition.krateo.io/v0-6-1"). A document of another
// Kind at the same apiVersion does not count (#486). Every release Secret is still DECODED,
// superseded ones
// included: which revision is latest or deployed is only known from all of them, and an undecodable
// one is an error, as before.
//
// An empty slice with a nil error means "no release references this version", which is the only
// condition under which a caller may prune. Any error means "could not determine" and the caller
// MUST NOT prune.
//
// Group, Version and Kind must all be set. An empty one returns an error rather than matching
// nothing: a caller that computed an empty group, version or kind has a bug, and answering
// "unreferenced" would turn that bug into a deletion.
func ReferencingReleases(ctx context.Context, c client.Client, gvk schema.GroupVersionKind) ([]Reference, error) {
	if strings.TrimSpace(gvk.Group) == "" || strings.TrimSpace(gvk.Version) == "" || strings.TrimSpace(gvk.Kind) == "" {
		return nil, fmt.Errorf("incomplete GroupVersionKind %q: refusing to report on an unspecified version or kind", gvk.String())
	}
	apiVersion := gvk.GroupVersion().String()
	if c == nil {
		return nil, fmt.Errorf("nil client: cannot determine whether %s is referenced", apiVersion)
	}

	var secrets corev1.SecretList
	if err := c.List(ctx, &secrets, client.MatchingLabels{helmOwnerLabel: helmOwnerValue}); err != nil {
		// Returned, not degraded. See the package comment: a list failure is not zero references.
		return nil, fmt.Errorf("listing helm release secrets: %w", err)
	}

	// Decode every release first: the revisions that count are only known once all of a release's
	// revisions are (#471).
	type decoded struct {
		sec *corev1.Secret
		rel *storedRelease
	}
	var all []decoded
	latest := map[string]int{}   // release key -> highest revision
	deployed := map[string]int{} // release key -> highest deployed revision
	key := func(ns, name string) string { return ns + "/" + name }
	for i := range secrets.Items {
		sec := &secrets.Items[i]
		if sec.Type != helmReleaseSecretType {
			continue
		}
		raw, ok := sec.Data[releaseDataKey]
		if !ok {
			// A release secret with no release payload is malformed, not empty. Refusing is the
			// only safe reading: we cannot see what it manages.
			return nil, fmt.Errorf("release secret %s/%s has no %q key",
				sec.Namespace, sec.Name, releaseDataKey)
		}
		rel, err := decodeRelease(raw)
		if err != nil {
			return nil, fmt.Errorf("decoding release secret %s/%s: %w", sec.Namespace, sec.Name, err)
		}
		k := key(sec.Namespace, rel.Name)
		if rel.Version > latest[k] {
			latest[k] = rel.Version
		}
		if rel.Info.Status == statusDeployed && rel.Version > deployed[k] {
			deployed[k] = rel.Version
		}
		all = append(all, decoded{sec, rel})
	}

	var refs []Reference
	for _, d := range all {
		sec, rel := d.sec, d.rel
		k := key(sec.Namespace, rel.Name)
		if rel.Version != latest[k] && rel.Version != deployed[k] {
			continue // superseded and older than both: only a manual rollback reads it (#471)
		}
		kinds, err := kindsAtAPIVersion(rel.Manifest, apiVersion)
		if err != nil {
			return nil, fmt.Errorf("scanning manifest of release secret %s/%s: %w",
				sec.Namespace, sec.Name, err)
		}
		if !slices.Contains(kinds, gvk.Kind) {
			continue // nothing, or only other Kinds, at this apiVersion: helm maps those by their own Kind (#486)
		}
		refs = append(refs, Reference{
			SecretNamespace: sec.Namespace,
			SecretName:      sec.Name,
			Release:         rel.Name,
			Revision:        rel.Version,
			Kinds:           kinds,
		})
	}
	return refs, nil
}

// decodeRelease mirrors helm's storage driver: base64, then gzip IF the magic header is present,
// then JSON. The optional-compression branch is helm's, not a defensive guess
// (pkg/storage/driver/util.go decodeRelease).
func decodeRelease(data []byte) (*storedRelease, error) {
	b, err := base64.StdEncoding.DecodeString(string(data))
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	if len(b) > 3 && bytes.Equal(b[0:3], magicGzip) {
		r, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		defer r.Close()
		unzipped, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("gzip read: %w", err)
		}
		b = unzipped
	}
	var rel storedRelease
	if err := json.Unmarshal(b, &rel); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return &rel, nil
}

// kindsAtAPIVersion returns the kinds declared at apiVersion in a rendered helm manifest, in first
// encountered order, deduplicated.
//
// WHY THE DOCUMENTS ARE PARSED rather than the manifest substring-searched. "composition.krateo.io/
// v0-6-1" could appear in an annotation value, a ConfigMap's data, or an RBAC rule, none of which
// is an object AT that version and none of which helm would try to map. A substring match would
// refuse prunes that are perfectly safe, and a prune predicate that over-refuses gets loosened by
// the next person. Comparing the parsed apiVersion field answers the question actually asked.
//
// An undecodable document is an ERROR, not a skipped document: it means part of the manifest could
// not be examined, and reporting "no references" from a partial scan is the destructive answer.
func kindsAtAPIVersion(manifest, apiVersion string) ([]string, error) {
	if strings.TrimSpace(manifest) == "" {
		return nil, nil
	}
	reader := k8syaml.NewYAMLReader(bufio.NewReader(strings.NewReader(manifest)))
	var kinds []string
	seen := map[string]bool{}
	for {
		doc, err := reader.Read()
		if err == io.EOF {
			return kinds, nil
		}
		if err != nil {
			return nil, fmt.Errorf("splitting manifest: %w", err)
		}
		if len(bytes.TrimSpace(doc)) == 0 {
			continue
		}
		var obj manifestObject
		// A document that cannot be parsed into {apiVersion, kind} is an ERROR, deliberately, and
		// the asymmetry is the reason. Skipping it would mean reporting on a manifest we only
		// partly read, and the report feeds a deletion. Every rendered helm document is a
		// Kubernetes object, so a non-mapping document means the manifest is not what we think it
		// is — refusing to prune is the right answer to that, and a comment-only document decodes
		// to null and zero-values the struct without erroring.
		//
		// The cost of being wrong in this direction is a served version that lingers; the cost of
		// being wrong in the other is a composition wedged indefinitely (#305).
		if err := yaml.Unmarshal(doc, &obj); err != nil {
			return nil, fmt.Errorf("parsing manifest document: %w", err)
		}
		if obj.APIVersion != apiVersion || obj.Kind == "" {
			continue
		}
		if seen[obj.Kind] {
			continue
		}
		seen[obj.Kind] = true
		kinds = append(kinds, obj.Kind)
	}
}
