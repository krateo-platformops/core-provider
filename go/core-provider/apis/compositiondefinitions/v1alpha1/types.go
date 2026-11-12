package v1alpha1

import (
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/resource"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
type CompositionDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []CompositionDefinition `json:"items"`
}

// GetItems of this CompositionDefinitionList.
func (l *CompositionDefinitionList) GetItems() []resource.Managed {
	items := make([]resource.Managed, len(l.Items))
	for i := range l.Items {
		items[i] = &l.Items[i]
	}
	return items
}

type Credentials struct {
	// Username: username for private repo
	Username string `json:"username"`
	// PasswordRef: reference to secret containing password for private repo.
	//
	// PRIVILEGE BOUNDARY, stated here because this is where someone writing a
	// CompositionDefinition will read it. The namespace is caller-supplied and is NOT confined to
	// the CompositionDefinition's own namespace: the referenced Secret is read with
	// core-provider's ServiceAccount, which can read Secrets cluster-wide, and nothing checks that
	// the author of this object was entitled to that Secret.
	//
	// So the credential named here travels to whatever `spec.chart.url` says. Granting create or
	// update on compositiondefinitions is therefore equivalent to granting cluster-wide Secret
	// READ, and should be treated as a privileged grant when writing RBAC — scope it the way you
	// would scope secrets access, not the way you would scope an application CR.
	//
	// THIS IS INTENDED AND WAS RULED ON, not an oversight (core-provider#306; Diego,
	// 2026-11-04). Cross-namespace references are a supported capability — deploy.go detects the
	// cross-namespace case and provisions a Role in the Secret's namespace so the fetch works, and
	// the equivalent surface in snowplow measured 23 of 26 references as cross-namespace. The
	// alternative considered was an RBAC check for a `use` verb against the requesting user, as
	// snowplow#1168 rules for its own endpoint Secrets. core-provider has no requesting user on a
	// reconcile: there is no SubjectAccessReview, UserInfo, impersonation or admission webhook
	// anywhere in this repo, so a reconcile sees this object and nothing about who wrote it. A
	// ServiceAccount substituted for the absent user would be a confused deputy with an
	// authorization check bolted on, which reads as authorised while being nothing of the kind.
	// The control therefore lives on who may write a CompositionDefinition at all.
	//
	// What is NOT in scope of that decision, and is fixed separately: the DESTINATION. A
	// credentialed fetch refuses link-local (including the cloud metadata service at
	// 169.254.169.254), unspecified and multicast addresses, checked at dial time on the resolved
	// address (plumbing dialguard, snowplow#1131).
	PasswordRef rtv1.SecretKeySelector `json:"passwordRef"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.version) || has(self.version)", message="Version is required once set"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.repo) || has(self.repo)", message="Repo is required once set"
type ChartInfo struct {
	// Url: oci or tgz full url
	Url string `json:"url"`
	// Version: desired chart version, needed for oci charts and for helm repo urls
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=20
	Version string `json:"version,omitempty"`
	// Repo: Helm repo name
	// Should be set only for helm repo urls
	// If specified with OCI registries, it will be used as the repository name, instead the URL should contain the full path to the chart.
	// This is ignored for tgz archives
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=256
	Repo string `json:"repo,omitempty"`

	// InsecureSkipVerifyTLS: skip tls verification
	// +optional
	InsecureSkipVerifyTLS bool `json:"insecureSkipVerifyTLS,omitempty"`

	// Credentials: credentials for private repos
	// +optional
	Credentials *Credentials `json:"credentials,omitempty"`
}

type ChartInfoProps struct {
	// Url: oci or tgz full url
	Url string `json:"url"`
	// Version: desired chart version, needed for oci charts and for helm repo urls
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=20
	Version string `json:"version,omitempty"`
	// Repo: helm repo name (for helm repo urls only)
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:MaxLength=256
	Repo string `json:"repo,omitempty"`

	// InsecureSkipVerifyTLS: skip tls verification
	// +optional
	InsecureSkipVerifyTLS bool `json:"insecureSkipVerifyTLS,omitempty"`

	// Credentials: credentials for private repos
	// +optional
	Credentials *Credentials `json:"credentials,omitempty"`
}

// DeploymentMode is the (status-only) classification of where the
// composition-dynamic-controller is deployed.
type DeploymentMode string

const (
	// DeploymentModeLocal: deployed into the management cluster (the default when no
	// target is referenced).
	DeploymentModeLocal DeploymentMode = "Local"
	// DeploymentModeRemote: deployed into a remote target cluster.
	DeploymentModeRemote DeploymentMode = "Remote"
)

// TargetReference references a namespaced KubernetesTarget by name. The target is always
// resolved in the referencing CompositionDefinition's OWN namespace, so no namespace field
// is carried here.
type TargetReference struct {
	// Name of the KubernetesTarget, resolved in the referencing object's own namespace.
	Name string `json:"name"`
}

// DeploymentTarget selects where the composition-dynamic-controller, the generated CRD
// and its RBAC are deployed. With no targetRef, deployment is local (the management
// cluster).
type DeploymentTarget struct {
	// TargetRef references a KubernetesTarget (resolved in this object's own namespace)
	// describing the remote cluster to deploy to. When omitted, deployment is local.
	// +optional
	TargetRef *TargetReference `json:"targetRef,omitempty"`
}

// Scale-to-zero is local-only in this release (#393, Diego 2026-11-11): a remote definition's controller
// and its instances live on the spoke, which needs spoke watches and spoke RBAC (core-provider#442).
// Rejected at admission so nobody believes the opt-in is active when it is not. (No quote characters in
// the rule: gofmt's doc-comment formatter rewrites two single quotes into a typographic quote.)
// +kubebuilder:validation:XValidation:rule="!(has(self.controller) && has(self.controller.scaleToZero) && self.controller.scaleToZero) || !(has(self.deploy) && has(self.deploy.targetRef) && size(self.deploy.targetRef.name) > 0)",message="spec.controller.scaleToZero is supported for local definitions only; for a remote spec.deploy.targetRef see core-provider#442"
type CompositionDefinitionSpec struct {
	// rtv1.ManagedSpec `json:",inline"`
	Chart *ChartInfo `json:"chart,omitempty"`

	// Deploy: selects whether the composition-dynamic-controller (and the generated
	// CRD and RBAC) are deployed locally in the management cluster (the default) or to
	// a remote target cluster referenced by a KubernetesTarget. When omitted, deployment
	// is local.
	// +optional
	Deploy *DeploymentTarget `json:"deploy,omitempty"`

	// ApiRef references a RESTAction whose calls are resolved (via snowplow, under an
	// authn-issued scoped service identity) each reconcile; results are keyed by call name
	// under ".api" in the projection root. Shape copied from snowplow's spec.apiRef
	// (name/namespace + inline extras).
	// +optional
	ApiRef *ApiReference `json:"apiRef,omitempty"`

	// StatusDataTemplate declares extra fields projected onto the generated Composition
	// CRD's status and populated by the controller each reconcile (snowplow
	// widgetDataTemplate shape). Each entry's ${ jq } Expression is evaluated over the
	// combined source root (built-in self/spec/status, helm, and api) and written under
	// .status at ForPath.
	// +optional
	// +listType=map
	// +listMapKey=forPath
	StatusDataTemplate []StatusFieldMapping `json:"statusDataTemplate,omitempty"`

	// UpgradePolicy governs whether existing composition instances are migrated onto a newly
	// served chart version:
	//   - Automatic (default): eagerly self-heal every owned instance onto the current version
	//     (retire the old version's controller once nothing references it). Backward-compatible
	//     with the pre-policy behavior.
	//   - Manual: keep existing instances on their current version (the new version is still served,
	//     so coexistence holds); migrate only when the krateo.io/upgrade-to-version annotation names
	//     the current served version.
	//   - Paused: never migrate; freeze existing instances on their version.
	// Under Manual/Paused the old version's controller is kept so coexisting instances are not
	// orphaned; served-version pruning still keeps any version an instance's label references.
	// +optional
	// +kubebuilder:validation:Enum=Automatic;Manual;Paused
	// +kubebuilder:default=Automatic
	UpgradePolicy UpgradePolicy `json:"upgradePolicy,omitempty"`

	// Controller tunes the composition-dynamic-controller (CDC) Deployment for THIS definition's
	// Kind. It is the per-Kind escape hatch from the otherwise-uniform, cluster-wide CDC template:
	// a Kind with many instances can raise reconcile concurrency and lengthen its resync without
	// touching — or restarting — any other controller. Omitted fields fall back to the chart-global
	// defaults, so existing definitions render byte-identically.
	// +optional
	Controller *ControllerConfig `json:"controller,omitempty"`
}

// ControllerConfig carries per-CompositionDefinition tuning for the composition-dynamic-controller.
// Every field is optional and, when unset, inherits the chart-global default baked into the CDC
// Deployment template. Note: replicas is deliberately NOT exposed here — running more than one CDC
// replica for a Kind is unsafe until sharding or leader election exists (two replicas both reconcile
// every instance and double-drive the same Helm release). Use Workers for in-process concurrency.
type ControllerConfig struct {
	// Workers is the number of concurrent reconcile workers inside the CDC process (the CDC's
	// -workers flag; one shared work queue feeds them). It is the primary lever for reconcile
	// throughput on a Kind with many instances. Defaults to the chart-global value (1) when unset.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=512
	Workers *int32 `json:"workers,omitempty"`

	// ResyncInterval is how often the CDC re-lists and re-observes every instance of the Kind to
	// correct drift (the CDC's -resync-interval flag). On a Kind with many instances a short resync
	// makes the controller re-enqueue the whole population continuously; lengthening it lets steady
	// state ride watch events instead of full sweeps. Defaults to the chart-global value (3m) when
	// unset. A Go duration string (e.g. "30m", "1h"); it is passed verbatim to the CDC's
	// -resync-interval flag. Typed as string (not metav1.Duration) deliberately: core-provider runs
	// no admission webhook, so the CRD schema is the only gate, and a plain string always decodes —
	// a bad value can never fail the typed informer's atomic list-decode and stall every
	// CompositionDefinition. The Pattern + CEL rules then reject unit-less/garbage values and
	// non-positive durations ("0s"/negative would crash the CDC's ticker) at admission.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="resyncInterval must be a positive Go duration such as 30m or 1h"
	ResyncInterval *string `json:"resyncInterval,omitempty"`

	// Pool names a shared controller pool this Kind's controller will run in (docs/design/cdc-pooling.md).
	// INERT in this release: the field is accepted and validated and changes nothing. Absent, and set,
	// render the same dedicated controller, byte for byte. It exists first so the API is settled before
	// the pool lifecycle (design step 5) is built. A name, not a boolean, because a pool's identity holds
	// the union of its members' permissions, and names let Kinds with similar permissions share one.
	// +optional
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Pool *string `json:"pool,omitempty"`

	// ScaleToZero runs this Kind's controller at zero replicas while no instance of it exists, and
	// wakes it when the first one appears (#393). Off when unset: the controller runs at the chart's
	// replica count, as before. Opting in costs one metadata watch on this Kind's instances, which is
	// what wakes it, so it is per definition rather than global. While idle the definition reports
	// Ready=True and ControllerActive=False, both with reason ScaledToZero (plumbing conditions, the
	// strings shared with oasgen-provider).
	// +optional
	ScaleToZero *bool `json:"scaleToZero,omitempty"`

	// ScaleToZeroHoldDown is how long the Kind must have no instances before its controller is scaled
	// to zero, so a delete followed by a re-create does not bounce it. Defaults to 10m. A Go duration
	// string, typed as a string for the same reason as ResyncInterval.
	// +optional
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$`
	// +kubebuilder:validation:XValidation:rule="duration(self) > duration('0s')",message="scaleToZeroHoldDown must be a positive Go duration"
	ScaleToZeroHoldDown *string `json:"scaleToZeroHoldDown,omitempty"`

	// Resources sets the CDC container's compute resource requests/limits for this Kind. The shared
	// template ships resources:{} (no requests), which is bad practice and makes the CDC
	// unschedulable-aware and impossible to autoscale; set requests here for a Kind whose CDC needs
	// a memory/CPU floor (e.g. one caching many instances). When unset, the chart-global default
	// (cdc.resources) applies, so existing definitions render byte-identically.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`
}

// UpgradePolicy selects the composition-instance migration strategy on a chart-version bump.
type UpgradePolicy string

const (
	// UpgradePolicyAutomatic eagerly migrates every owned instance onto the current version.
	UpgradePolicyAutomatic UpgradePolicy = "Automatic"
	// UpgradePolicyManual migrates only when the upgrade-to-version annotation approves it.
	UpgradePolicyManual UpgradePolicy = "Manual"
	// UpgradePolicyPaused freezes migration entirely.
	UpgradePolicyPaused UpgradePolicy = "Paused"

	// AnnotationKeyUpgradeToVersion, on a CompositionDefinition with UpgradePolicy=Manual, approves
	// migrating its instances onto the named served version (typically the current one).
	AnnotationKeyUpgradeToVersion = "krateo.io/upgrade-to-version"
)

// MigrationApproved reports whether the controller should migrate existing instances onto
// targetVersion given the definition's UpgradePolicy (and, for Manual, the upgrade-to-version
// annotation). An empty policy is treated as Automatic for backward compatibility.
func (cd *CompositionDefinition) MigrationApproved(targetVersion string) bool {
	switch cd.Spec.UpgradePolicy {
	case UpgradePolicyPaused:
		return false
	case UpgradePolicyManual:
		return cd.GetAnnotations()[AnnotationKeyUpgradeToVersion] == targetVersion
	default: // Automatic or unset
		return true
	}
}

// ApiReference references a RESTAction to resolve for status projection. It mirrors
// snowplow's spec.apiRef: a name/namespace plus an inline, free-form extras map.
type ApiReference struct {
	// Name of the referenced RESTAction.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Namespace of the referenced RESTAction.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`

	// Extras are author-declared STATIC values merged into the RESTAction's jq root
	// (snowplow spec.apiRef.extras). Input-only — they never surface in status. The
	// controller additionally injects per-instance context (compositionId, namespace, …)
	// which merges over these (request-wins).
	// +optional
	// +kubebuilder:pruning:PreserveUnknownFields
	Extras *apiextensionsv1.JSON `json:"extras,omitempty"`

	// Delegation lets this definition's composition controller act on behalf of a caller through
	// authn's POST /delegate (#405). It is rendered onto the controller's authn ServiceAccount mapping
	// (spec.delegation) on every apply; a value set by hand on the mapping is replaced. Absent means no
	// delegation key on the mapping at all, which authn answers with 403 DelegationNotAllowed.
	//
	// The bounds are authn's own (serviceaccounts.serviceaccount.authn.krateo.io): if this field
	// accepted more, the apiserver would reject the mapping's update instead, far from this object.
	// Enabling it on a real definition is a platform decision, not a default.
	// +optional
	Delegation *ApiRefDelegation `json:"delegation,omitempty"`
}

// ApiRefDelegation mirrors authn's ServiceAccount mapping spec.delegation, with its bounds.
type ApiRefDelegation struct {
	// Allowed enables delegation for the controller's identity. Required whenever delegation is set.
	Allowed bool `json:"allowed"`

	// MaxDepth limits how deep a delegation chain may go. Unset leaves authn's own default (3, applied
	// in authn's code, not by its CRD), so it is rendered only when set.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=8
	MaxDepth *int32 `json:"maxDepth,omitempty"`
}

// StatusFieldMapping is one projected status field (snowplow widgetDataTemplate item).
type StatusFieldMapping struct {
	// ForPath is the dotted path under .status to write, e.g. "endpoint" or "network.host".
	// +kubebuilder:validation:MinLength=1
	ForPath string `json:"forPath"`

	// Expression is a ${ jq } program evaluated over the projection root; a bare path is
	// the trivial copy. Plain literals (no ${ } wrapper) are written verbatim.
	// +kubebuilder:validation:MinLength=1
	Expression string `json:"expression"`

	// Type optionally pins the generated status property's scalar JSON-schema type. For
	// complex (object/array) outputs use Schema or PreserveUnknownFields instead.
	// +optional
	// +kubebuilder:validation:Enum=string;integer;number;boolean;object;array
	Type string `json:"type,omitempty"`

	// Schema optionally supplies the full JSON-schema for a complex output (object, array,
	// array of objects). Mutually exclusive with PreserveUnknownFields.
	// +optional
	// +kubebuilder:validation:Schemaless
	// +kubebuilder:pruning:PreserveUnknownFields
	Schema *apiextensionsv1.JSONSchemaProps `json:"schema,omitempty"`

	// PreserveUnknownFields generates the status node with
	// x-kubernetes-preserve-unknown-fields: true, retaining an arbitrary structure without
	// pruning. Mutually exclusive with Type/Schema.
	// +optional
	PreserveUnknownFields bool `json:"preserveUnknownFields,omitempty"`
}

// KubernetesTargetSpec describes how to reach a remote target cluster.
type KubernetesTargetSpec struct {
	// KubeconfigRef: reference to a Kubernetes Secret key holding the kubeconfig of the
	// target cluster. The Secret is the credential rotation seam - populate and rotate
	// it via External Secrets Operator.
	KubeconfigRef rtv1.SecretKeySelector `json:"kubeconfigRef"`
}

// KubernetesTargetStatus reports the observed reachability of the target cluster, refreshed by the
// KubernetesTarget reachability reconciler. It turns the passive reference into a registered spoke
// with lifecycle: a Ready condition, the reported Kubernetes version, and the credential the last
// probe used (for rotation traceability).
type KubernetesTargetStatus struct {
	rtv1.ConditionedStatus `json:",inline"`

	// ConnectionStatus is Healthy when the target cluster's API is reachable, Down otherwise.
	// +optional
	ConnectionStatus string `json:"connectionStatus,omitempty"`

	// Version is the Kubernetes version reported by the target cluster on the last successful probe.
	// +optional
	Version string `json:"version,omitempty"`

	// KubeconfigSecretResourceVersion is the resourceVersion of the kubeconfig Secret used by the
	// last successful probe, for rotation traceability.
	// +optional
	KubeconfigSecretResourceVersion string `json:"kubeconfigSecretResourceVersion,omitempty"`

	// LastProbeTime is when the target's reachability was last checked.
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced,categories={krateo,core}
//+kubebuilder:printcolumn:name="CONNECTION",type="string",JSONPath=".status.connectionStatus"
//+kubebuilder:printcolumn:name="VERSION",type="string",JSONPath=".status.version"
//+kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"

// KubernetesTarget is a namespaced reference to a remote cluster, used by a
// CompositionDefinition's spec.deploy.targetRef (resolved in the CompositionDefinition's
// own namespace) to deploy compositions remotely. It is namespaced so per-namespace
// writes through snowplow /call (which force-namespaces every write) can create it.
type KubernetesTarget struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec KubernetesTargetSpec `json:"spec,omitempty"`
	// +optional
	Status KubernetesTargetStatus `json:"status,omitempty"`
}

//+kubebuilder:object:root=true

// KubernetesTargetList is a list of KubernetesTarget.
type KubernetesTargetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []KubernetesTarget `json:"items"`
}

type VersionDetail struct {
	// Version: the version of the chart that is served. It is the version of the CRD.
	// +optional
	Version string `json:"version"`

	// Served: whether the version is served
	// +optional
	Served bool `json:"served"`

	// Stored: whether the version is stored
	// +optional
	Stored bool `json:"stored"`

	// Chart: the chart information
	// +optional
	Chart *ChartInfoProps `json:"chart"`
}

type Managed struct {
	// VersionInfo: the version information of the chart
	// +optional
	VersionInfo []VersionDetail `json:"versionInfo,omitempty"`

	// Group: the generated custom resource Group
	// +optional
	Group string `json:"group,omitempty"`

	// Kind: the generated custom resource Kind
	// +optional
	Kind string `json:"kind,omitempty"`
}

// TargetStatus reports the cluster the composition-dynamic-controller (and the
// generated CRD and RBAC) are deployed to.
type TargetStatus struct {
	// Mode: where the controller is deployed (Local or Remote).
	// +optional
	Mode string `json:"mode,omitempty"`

	// ConnectionStatus: Healthy when the target cluster is reachable, Down otherwise.
	// +optional
	ConnectionStatus string `json:"connectionStatus,omitempty"`

	// Version: the Kubernetes version reported by the target cluster.
	// +optional
	Version string `json:"version,omitempty"`

	// KubeconfigSecretResourceVersion: the resourceVersion of the kubeconfig Secret last
	// used to reach a remote target, for credential-rotation traceability.
	// +optional
	KubeconfigSecretResourceVersion string `json:"kubeconfigSecretResourceVersion,omitempty"`
}

// CompositionDefinitionStatus is the status of a CompositionDefinition.
type CompositionDefinitionStatus struct {
	rtv1.ConditionedStatus `json:",inline"`

	// Target: information about the cluster the controller is deployed to.
	// +optional
	Target *TargetStatus `json:"target,omitempty"`

	// Kind: the kind of the custom resource - Last applied kind
	Kind string `json:"kind,omitempty"`

	// Resource: the resource of the custom resource - Last applied resource
	Resource string `json:"resource,omitempty"`

	// ApiVersion: the api version of the custom resource - Last applied apiVersion
	ApiVersion string `json:"apiVersion,omitempty"`

	// Managed: information about the managed resources
	Managed Managed `json:"managed,omitempty"`

	// PackageURL: .tgz or oci chart direct url
	// +optional
	PackageURL string `json:"packageUrl,omitempty"`

	// Digest: the digest of the managed resources
	// +optional
	Digest string `json:"digest,omitempty"`

	// ScaleToZero is the scale-to-zero reconciler's state for a definition that opted in (#393).
	// Absent otherwise. Written ONLY by that reconciler, with a merge patch on this field; the
	// definition's main reconciler reads it to report Ready and ControllerActive.
	// +optional
	ScaleToZero *ScaleToZeroStatus `json:"scaleToZero,omitempty"`

	// ServedVersionsPendingPrune records, per served version of this definition's CRD that has
	// become prune-eligible (no definition references it, no instance is labelled with it, no stored
	// helm revision names it), when it first became so (#454). A version is pruned only after it has
	// stayed eligible for the grace window, so a render that read the CRD before the prune never
	// looks the version up after it. Each version has its own clock: a batch released at once by a
	// helm history rotation is N independent windows. A version that stops being eligible is
	// dropped from the list. Written by the definition's own reconciler, in Observe.
	// +optional
	ServedVersionsPendingPrune []PendingPrune `json:"servedVersionsPendingPrune,omitempty"`
}

// PendingPrune is one served version waiting out the prune grace window (#454).
type PendingPrune struct {
	// Version is the served version, e.g. v0-1-42.
	Version string `json:"version"`
	// EligibleSince is when this reconciler first saw the version prune-eligible.
	EligibleSince metav1.Time `json:"eligibleSince"`
}

// ScaleToZeroStatus records what provider-runtime's scaletozero helper last decided for this
// definition's controller.
type ScaleToZeroStatus struct {
	// LastSeen is when an instance of the Kind was last seen; the hold-down counts from it. It is the
	// helper's Result.LastSeen, persisted because the helper keeps no state.
	// +optional
	LastSeen *metav1.Time `json:"lastSeen,omitempty"`

	// Replicas is the controller Deployment's replicas after the last decision: 0 when idle.
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Reason is the helper's reason for that decision: Active, IdleHoldDown, Idle or Unknown.
	// +optional
	Reason string `json:"reason,omitempty"`

	// HeldBy names the live definitions that share this controller and have NOT opted in. While it is
	// non-empty the shared controller is kept running (Diego, 2026-11-11: every sharing definition
	// must opt in).
	// +optional
	HeldBy []string `json:"heldBy,omitempty"`
}

//+kubebuilder:object:root=true
//+kubebuilder:subresource:status
//+kubebuilder:resource:scope=Namespaced,categories={krateo,defs,core}
//+kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
//+kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
//+kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
//+kubebuilder:printcolumn:name="API VERSION",type="string",JSONPath=".status.apiVersion",priority=10
//+kubebuilder:printcolumn:name="KIND",type="string",JSONPath=".status.kind",priority=10
//+kubebuilder:printcolumn:name="PACKAGE URL",type="string",JSONPath=".status.packageUrl",priority=10
//+kubebuilder:printcolumn:name="TARGET",type="string",JSONPath=".status.target.mode",priority=10
//+kubebuilder:printcolumn:name="CONNECTION",type="string",JSONPath=".status.target.connectionStatus",priority=10

// CompositionDefinition is a definition type with a spec and a status.
type CompositionDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   CompositionDefinitionSpec   `json:"spec,omitempty"`
	Status CompositionDefinitionStatus `json:"status,omitempty"`
}

// SetConditions of this CompositionDefinition.
func (mg *CompositionDefinition) SetConditions(c ...rtv1.Condition) {
	mg.Status.SetConditions(c...)
}

// GetCondition of this CompositionDefinition.
func (mg *CompositionDefinition) GetCondition(ct rtv1.ConditionType) rtv1.Condition {
	return mg.Status.GetCondition(ct)
}
