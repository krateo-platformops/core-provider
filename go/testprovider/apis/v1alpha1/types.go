// Package v1alpha1 declares the one kind the #310 verification provider reconciles.
//
// WHY A TYPED KIND AT ALL, when the cdc reads children as unstructured. provider-runtime's
// NewReconciler takes a resource.ManagedKind and constructs the object from the manager's scheme
// (reconciler.go:696-703), so the runtime cannot reconcile an unregistered kind. A typed Go type is
// the price of using the REAL runtime rather than a stand-in, and using the real runtime is the
// entire point: the Ready=Unknown/NotObserved this produces is written by provider-runtime's own
// error path, not patched into status by a test.
package v1alpha1

import (
	prv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// Group/Version/Kind for the verification kind. Deliberately NOT under composition.krateo.io or any
// production group: nothing in a real cluster has this kind, so a stray CRD cannot collide with a
// composition and this provider can never be pointed at production data.
const (
	Group   = "e2e.krateo.io"
	Version = "v1alpha1"
	Kind    = "TestChild"
	Plural  = "testchildren"
)

var (
	// SchemeGroupVersion is the GV this package registers.
	SchemeGroupVersion = schema.GroupVersion{Group: Group, Version: Version}

	// TestChildGroupVersionKind is what gets handed to provider-runtime as the ManagedKind.
	TestChildGroupVersionKind = SchemeGroupVersion.WithKind(Kind)

	// SchemeBuilder registers the types with a scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(addKnownTypes)

	// AddToScheme is the entry point main.go uses.
	AddToScheme = SchemeBuilder.AddToScheme
)

func addKnownTypes(s *runtime.Scheme) error {
	s.AddKnownTypes(SchemeGroupVersion, &TestChild{}, &TestChildList{})
	metav1.AddToGroupVersion(s, SchemeGroupVersion)
	return nil
}

// TestChildSpec carries the switch that makes this verification possible.
type TestChildSpec struct {
	// FailObserve makes this child's Observe return an error.
	//
	// THE SWITCH IS PER-OBJECT AND IN THE SPEC, not a process flag or an env var, and that is what
	// makes arms (a) through (d) expressible without restarting anything: one child can be failing
	// while its sibling succeeds, which is exactly the mixed-parent case the #310 criterion is
	// about. A process-level flag could only ever produce "all children failing", which is arm (b)
	// alone — the arm that is least able to catch the defect.
	FailObserve bool `json:"failObserve,omitempty"`

	// Unready makes Observe SUCCEED but report the external resource as not ready, so the provider
	// sets Ready=False/Creating. This is the sibling state arm (c) needs, and it must come from a
	// successful Observe: a failing Observe would produce NotObserved again and collapse the two
	// states the arm exists to distinguish.
	Unready bool `json:"unready,omitempty"`
}

// TestChildStatus is the minimum resource.Managed requires: a Conditioned status.
type TestChildStatus struct {
	prv1.ConditionedStatus `json:",inline"`
}

// TestChild is the verification child kind.
//
// +kubebuilder:object:root=true
type TestChild struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TestChildSpec   `json:"spec,omitempty"`
	Status TestChildStatus `json:"status,omitempty"`
}

// TestChildList is required by the manager's cache, which LISTs before it watches.
type TestChildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TestChild `json:"items"`
}

// The DeepCopy methods below are hand-written rather than generated. controller-gen is not wired
// into this module and adding it for one type would make the verification harness harder to read
// than the thing it verifies.

// DeepCopyObject implements runtime.Object.
func (in *TestChild) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(TestChild)
	in.DeepCopyInto(out)
	return out
}

// DeepCopyInto copies the receiver into out.
func (in *TestChild) DeepCopyInto(out *TestChild) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	in.Status.ConditionedStatus.DeepCopyInto(&out.Status.ConditionedStatus)
}

// DeepCopyObject implements runtime.Object.
func (in *TestChildList) DeepCopyObject() runtime.Object {
	if in == nil {
		return nil
	}
	out := new(TestChildList)
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]TestChild, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
	return out
}

// GetCondition and SetConditions satisfy resource.Conditioned, which resource.Managed embeds.
func (in *TestChild) GetCondition(ct prv1.ConditionType) prv1.Condition {
	return in.Status.GetCondition(ct)
}

// SetConditions is the method provider-runtime calls to write Ready and Synced.
func (in *TestChild) SetConditions(c ...prv1.Condition) {
	in.Status.SetConditions(c...)
}
