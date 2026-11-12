package auditrec

import (
	"context"

	"github.com/krateo-platformops/audit"
	"go.opentelemetry.io/otel"
	logglobal "go.opentelemetry.io/otel/log/global"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var namespacesGVR = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}

// Actor is the controller's principal, system:serviceaccount:<namespace>:<name>, or "" when either
// is unset: an unknown actor is left for the emitter to mark, never guessed.
func Actor(saNamespace, saName string) string {
	if saNamespace == "" || saName == "" {
		return ""
	}
	return "system:serviceaccount:" + saNamespace + ":" + saName
}

// New builds the cdc's Recorder for the compositions of resource (the plural): records go to the
// process LoggerProvider under Scope (OTLP when OTEL_LOGS_ENABLED; a no-op provider otherwise), the
// counters to the process MeterProvider, and preview is read from the Namespaces of the cluster cfg
// points at, the one helm writes to.
func New(cfg *rest.Config, resource, saNamespace, saName string) (*Recorder, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	opts := []audit.Option{audit.WithWriter(audit.WriterController)}
	if a := Actor(saNamespace, saName); a != "" {
		opts = append(opts, audit.WithActor(a, audit.ClassController))
	}
	e := audit.New(logglobal.GetLoggerProvider().Logger(Scope), opts...)
	return NewRecorder(resource, e, otel.GetMeterProvider(), func(ctx context.Context, name string) (map[string]string, error) {
		ns, err := dyn.Resource(namespacesGVR).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}
		return ns.GetLabels(), nil
	})
}
