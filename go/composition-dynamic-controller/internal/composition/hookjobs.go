package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

// annotationHookJobsToDelete records, on the composition, the hook Jobs its release left behind
// that an uninstall must delete (core-provider#576, Diego 2026-11-18). helm never deletes a hook
// resource at uninstall, and no hook-delete-policy acts then, so charts whose pods wait on hook Jobs
// by name (OpenStack-Helm's DEPENDENCY_JOBS) leave every one behind: 156 Completed pods after one
// 33-component teardown.
//
// RECORDED BEFORE THE UNINSTALL, deliberately. After a successful uninstall the release record is
// gone, so a pass that failed to delete a Job could not re-read the hooks from helm. The list is
// written first and shrinks as deletes succeed; whatever remains is retried by the next Delete.
const annotationHookJobsToDelete = "krateo.io/hook-jobs-to-delete"

var jobsGVR = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}

// hookJobRef is one hook Job by its recorded identity.
type hookJobRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// recordedHookJobs are the Jobs in rel's own hook record (helm's rel.Hooks, by exact identity, not a
// label match), except those that ask to be kept (helm.sh/resource-policy: keep). A hook's namespace
// comes from its rendered manifest and defaults to the release namespace, as helm applies it.
func recordedHookJobs(rel *helmconfig.Release) ([]hookJobRef, error) {
	if rel == nil {
		return nil, nil
	}
	var out []hookJobRef
	for _, h := range rel.Hooks {
		if h.Kind != "Job" {
			continue
		}
		var doc struct {
			APIVersion string `json:"apiVersion"`
			Metadata   struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(h.Manifest), &doc); err != nil {
			return nil, fmt.Errorf("reading hook %s (%s): %w", h.Name, h.Path, err)
		}
		if !strings.HasPrefix(doc.APIVersion, "batch/") {
			continue // a Kind named Job in another group is not ours to delete
		}
		if doc.Metadata.Annotations["helm.sh/resource-policy"] == "keep" {
			continue
		}
		name := doc.Metadata.Name
		if name == "" {
			name = h.Name
		}
		ns := doc.Metadata.Namespace
		if ns == "" {
			ns = rel.Namespace
		}
		out = append(out, hookJobRef{Namespace: ns, Name: name})
	}
	return out, nil
}

// pendingHookJobs reads the composition's recorded list. A value that does not parse is an error,
// never an empty list: on a teardown path "could not read what is pending" must not read as
// "nothing is pending".
func pendingHookJobs(mg *unstructured.Unstructured) ([]hookJobRef, error) {
	raw, ok := mg.GetAnnotations()[annotationHookJobsToDelete]
	if !ok || raw == "" {
		return nil, nil
	}
	var refs []hookJobRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil, fmt.Errorf("reading annotation %s: %w", annotationHookJobsToDelete, err)
	}
	return refs, nil
}

// setPendingHookJobs writes refs (deduplicated, sorted) to the annotation, or removes it when empty.
func setPendingHookJobs(mg *unstructured.Unstructured, refs []hookJobRef) {
	seen := map[hookJobRef]bool{}
	var uniq []hookJobRef
	for _, r := range refs {
		if !seen[r] {
			seen[r] = true
			uniq = append(uniq, r)
		}
	}
	sort.Slice(uniq, func(i, j int) bool {
		if uniq[i].Namespace != uniq[j].Namespace {
			return uniq[i].Namespace < uniq[j].Namespace
		}
		return uniq[i].Name < uniq[j].Name
	})
	ann := mg.GetAnnotations()
	if len(uniq) == 0 {
		if _, ok := ann[annotationHookJobsToDelete]; ok {
			delete(ann, annotationHookJobsToDelete)
			mg.SetAnnotations(ann)
		}
		return
	}
	b, _ := json.Marshal(uniq) // a slice of two strings always marshals
	if ann == nil {
		ann = map[string]string{}
	}
	ann[annotationHookJobsToDelete] = string(b)
	mg.SetAnnotations(ann)
}

// deleteHookJobs deletes each Job, its pods with it (background propagation). One already gone is
// done; any other failure keeps that Job in remaining and is returned, so the caller records what is
// left and fails the pass rather than reading a failed delete as a deleted Job.
func deleteHookJobs(ctx context.Context, dyn dynamic.Interface, refs []hookJobRef) (remaining []hookJobRef, err error) {
	bg := metav1.DeletePropagationBackground
	var errs []error
	for _, r := range refs {
		derr := dyn.Resource(jobsGVR).Namespace(r.Namespace).Delete(ctx, r.Name, metav1.DeleteOptions{PropagationPolicy: &bg})
		if derr == nil || apierrors.IsNotFound(derr) {
			continue
		}
		remaining = append(remaining, r)
		errs = append(errs, fmt.Errorf("deleting hook Job %s/%s: %w", r.Namespace, r.Name, derr))
	}
	return remaining, errors.Join(errs...)
}
