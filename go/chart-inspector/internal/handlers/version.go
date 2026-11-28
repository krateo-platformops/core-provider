package handlers

import "net/http"

// VersionHeader carries this chart-inspector's build version on every response (core-provider#697).
//
// The composition controller keys its cache of derived RBAC on it: the answer is a function of this
// binary's logic as well as of the chart, so an upgrade of chart-inspector must read as a change.
// An inspector that predates the header sends none, which the controller reads as the version "".
// Set on /healthz and /readyz as well, so the controller can learn a new version without a render.
const VersionHeader = "X-Chart-Inspector-Version"

// WithVersionHeader sets VersionHeader to version on every response next handler writes.
func WithVersionHeader(version string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, version)
		next.ServeHTTP(w, r)
	})
}
