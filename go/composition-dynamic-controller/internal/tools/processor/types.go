package processor

var _ MinimalMetaObject = &MinimalMetadata{}

type MinimalMetaObject interface {
	GetAnnotations() map[string]string
	GetNamespace() string
	GetName() string
	GetKind() string
	GetAPIVersion() string

	SetNamespace(namespace string)
	SetName(name string)
	SetAnnotations(annotations map[string]string)
}

type Metadata struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Use the map to capture annotations, including the helm hook
	Annotations map[string]string `json:"annotations"`
	// Labels carries the post-renderer's krateo.io/composition-* stamps, which are the only
	// RECORDED statement of which composition a stored release belongs to. The release name is
	// not that statement: it is derived from the CR's UID, which a recreated CR does not have, and
	// it can be overridden outright by the krateo.io/release-name label. See
	// composition.compositionIdentity.
	Labels map[string]string `json:"labels"`
}

// MinimalMetadata holds only the necessary fields for reference extraction.
// Decoding into this struct is significantly cheaper than map[string]any.
type MinimalMetadata struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
}

func (m *MinimalMetadata) GetAnnotations() map[string]string {
	return m.Metadata.Annotations
}

func (m *MinimalMetadata) GetLabels() map[string]string {
	return m.Metadata.Labels
}

func (m *MinimalMetadata) GetNamespace() string {
	return m.Metadata.Namespace
}

func (m *MinimalMetadata) GetName() string {
	return m.Metadata.Name
}

func (m *MinimalMetadata) GetKind() string {
	return m.Kind
}

func (m *MinimalMetadata) GetAPIVersion() string {
	return m.APIVersion
}

func (m *MinimalMetadata) SetNamespace(namespace string) {
	m.Metadata.Namespace = namespace
}

func (m *MinimalMetadata) SetName(name string) {
	m.Metadata.Name = name
}
func (m *MinimalMetadata) SetAnnotations(annotations map[string]string) {
	m.Metadata.Annotations = annotations
}
