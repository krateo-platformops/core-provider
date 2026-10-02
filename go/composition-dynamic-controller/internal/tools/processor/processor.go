package processor

import (
	"io"
	"strings"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/hasher"
	"github.com/krateo-platformops/plumbing/helm"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func DecodeMinRelease(rel *helm.Release) ([]MinimalMetadata, string, error) {
	return DecodeRelease[MinimalMetadata](rel)
}

func DecodeUnstructuredRelease(rel *helm.Release) ([]unstructured.Unstructured, string, error) {
	return DecodeRelease[unstructured.Unstructured](rel)
}

// DecodeRelease returns the release's objects plus a content digest. The digest is computed from
// the traceparent-stripped manifest (identical to ComputeReleaseDigest) so the value Create/Update
// store in status.digest matches what Observe compares against; the objects are decoded from the
// original manifest so they retain their real annotations.
func DecodeRelease[T any, PT interface {
	*T
	MinimalMetaObject
}](rel *helm.Release) ([]T, string, error) {
	if rel == nil {
		return nil, "", nil
	}
	// 1. Fast path: Empty manifest
	if strings.TrimSpace(rel.Manifest) == "" {
		return nil, "", nil
	}

	h := hasher.NewFNVObjectHash()

	// 2. Digest: hash the traceparent-STRIPPED manifest, byte-identical to
	// ComputeReleaseDigest. The digest this returns is stored in status.digest by Create/Update,
	// and Observe compares against it using ComputeReleaseDigest — the two MUST hash the same
	// content or every reconcile sees a spurious diff. The per-reconcile krateo.io/traceparent
	// (+ tracestate) annotations, stamped on EVERY resource including nested *.krateo.io CR
	// children, change each cycle; stripping them here keeps the digest content-only (no churn).
	if err := h.SumHashStrings(stripTraceContext(rel.Manifest)); err != nil {
		return nil, "", err
	}

	// 3. Decoder Setup — decode the ORIGINAL manifest for the returned object list (the objects
	// must keep their real annotations); only the digest above is computed from the stripped copy.
	// We use a larger buffer (4096) to reduce read syscalls for large objects
	decoder := yaml.NewYAMLOrJSONDecoder(strings.NewReader(rel.Manifest), 4096)

	// Pre-allocate slice. Heuristic: 10 objects prevents resizing for most charts.
	objects := make([]T, 0, 10)

	for {
		// 4. OPTIMIZATION: Direct Decode
		// Instead of decoding to RawExtension (buffer) -> json.Unmarshal (struct),
		// we decode directly into the struct. This saves one massive allocation per object.
		var obj T
		err := decoder.Decode(&obj)

		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, "", err
		}

		// 5. Filter Empty/Null Objects
		// Since we skipped RawExtension check, we validate the object itself.
		// If APIVersion is empty, it's likely a "---" separator or empty doc.
		p := PT(&obj)
		if p.GetAPIVersion() == "" {
			continue
		}

		objects = append(objects, obj)
	}

	return objects, h.GetHash(), nil
}

// ComputeReleaseDigest calculates the hash of the release manifest without decoding objects.
func ComputeReleaseDigest(rel *helm.Release) (string, error) {
	if strings.TrimSpace(rel.Manifest) == "" {
		return "", nil
	}

	h := hasher.NewFNVObjectHash()
	// Exclude the per-reconcile krateo.io/traceparent (+ tracestate) annotations from the
	// change-detection digest: their value differs on every reconcile (each is a new trace),
	// so hashing them would make the composition perpetually "out-of-date" and trigger a helm
	// Upgrade on every reconcile (churn). They are still stamped on the live resources for
	// trace propagation — just not counted as a content change.
	err := h.SumHashStrings(stripTraceContext(rel.Manifest))
	if err != nil {
		return "", err
	}

	return h.GetHash(), nil
}

// stripTraceContext removes the krateo.io/traceparent and krateo.io/tracestate annotation
// lines from a rendered manifest so they do not affect the release digest. Only those lines
// are removed and everything else stays byte-identical; a manifest that never carried them
// (fast path) hashes exactly as before the feature existed.
func stripTraceContext(manifest string) string {
	if !strings.Contains(manifest, "krateo.io/traceparent") && !strings.Contains(manifest, "krateo.io/tracestate") {
		return manifest
	}
	lines := strings.Split(manifest, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "krateo.io/traceparent:") || strings.HasPrefix(t, "krateo.io/tracestate:") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}
