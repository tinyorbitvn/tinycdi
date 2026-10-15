// Copyright (c) 2026 TinyOrbit
// SPDX-License-Identifier: MIT

package api

import "net/http"

// RouteMux is what the Mount* functions register routes on. *http.ServeMux
// satisfies it; tests wrap one to record every mounted pattern and compare
// the set with openapi.yaml.
type RouteMux interface {
	Handle(pattern string, handler http.Handler)
}
