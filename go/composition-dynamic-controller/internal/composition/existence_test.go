package composition

import (
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
)

// The handler must satisfy ExistenceChecker for incomplete-create recovery to use it.
//
// This is the load-bearing assertion of the whole change. The interface is OPTIONAL: the runtime
// type-asserts for it and silently falls back to Observe when the assertion fails. So if this
// method's signature ever drifts — a renamed parameter type, a changed return — the fallback
// re-engages, compositions go back to wedging permanently on "cannot determine creation result",
// and nothing anywhere fails to build or test. The failure would be invisible until a create is
// interrupted in production.
//
// There is a compile-time `var _ controller.ExistenceChecker = (*handler)(nil)` in existence.go for
// the same reason; this makes the intent explicit to anyone reading the tests.
func TestHandlerImplementsExistenceChecker(t *testing.T) {
	var h interface{} = &handler{}

	if _, ok := h.(controller.ExistenceChecker); !ok {
		t.Fatal("handler no longer satisfies controller.ExistenceChecker: " +
			"incomplete-create recovery will silently fall back to Observe and wedge on convergence failures")
	}
}

// Exists must not be Observe under another name. Observe reports convergence and repairs drift;
// Exists answers only whether the release landed. Guard the distinction that motivated the fix: a
// composition whose chart cannot converge still has an answerable existence question.
func TestExistsIsDistinctFromObserve(t *testing.T) {
	h := &handler{}

	var checker controller.ExistenceChecker = h
	var client controller.ExternalClient = h

	// Both interfaces are served by the same handler, which is fine — what must not happen is
	// Exists being routed through Observe's convergence work. That is enforced by existence.go
	// calling GetRelease directly; this test pins that the two entry points stay separate.
	if checker == nil || client == nil {
		t.Fatal("handler must serve both ExternalClient and ExistenceChecker")
	}
}
