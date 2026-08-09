package resources

type Resource struct {
	Group     string `json:"group"`
	Version   string `json:"version"`
	Resource  string `json:"resource"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	// Verbs the composition SA needs on this resource. The tracer sets [get,list,watch] for a
	// collection read (a Helm `lookup` of a set) and ["*"] for a named observation. Optional:
	// absent means ["*"] (an older inspector), so consumers stay backward-compatible.
	Verbs []string `json:"verbs,omitempty"`
}
