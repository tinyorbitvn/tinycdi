package api

import (
	"context"
	"net/http"
	"strings"
)

type actionIDKey struct{}

// ActionRouter dispatches POST {collection}/{id}:{verb} custom methods.
// ServeMux cannot match a wildcard that is only part of a segment, so the
// route registers {ref} and this splits it. An id must not contain ':'.
type ActionRouter struct {
	verbs map[string]http.HandlerFunc
}

func NewActionRouter() *ActionRouter { return &ActionRouter{verbs: map[string]http.HandlerFunc{}} }

func (a *ActionRouter) Handle(verb string, h http.HandlerFunc) { a.verbs[verb] = h }

func (a *ActionRouter) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, verb, ok := strings.Cut(r.PathValue("ref"), ":")
	h := a.verbs[verb]
	if !ok || id == "" || strings.Contains(verb, ":") || h == nil {
		writeError(w, r, CodeNotFound, "not found")
		return
	}
	h(w, r.WithContext(context.WithValue(r.Context(), actionIDKey{}, id)))
}

// ActionID is the resource id of an ActionRouter request.
func ActionID(r *http.Request) string {
	id, _ := r.Context().Value(actionIDKey{}).(string)
	return id
}
