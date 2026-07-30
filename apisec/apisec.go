// Package apisec gives an app the same OpenAPI security requirements the
// foundation puts on its own operations, so a route the app registers next to
// them documents itself the same way instead of hand-writing the scheme names.
//
//	huma.Register(srv.API, huma.Operation{
//	    OperationID: "list-widgets",
//	    Method:      http.MethodGet,
//	    Path:        "/api/widgets",
//	    Errors:      []int{http.StatusUnauthorized},
//	    Security:    apisec.User(srv.API),
//	}, handler)
//
// The scheme names stay an implementation detail, and whether bearer tokens
// belong in the requirement is answered from the API rather than guessed.
package apisec

import (
	"github.com/danielgtaylor/huma/v2"

	"github.com/robert-crandall/go-home-server/internal/apisecurity"
)

// Public marks an operation as requiring no authentication. It returns an empty
// (non-nil) requirement list, which overrides any top-level security rather
// than inheriting it.
func Public() []map[string][]string {
	return apisecurity.Public()
}

// Session requires the session cookie, and nothing else. Use it for operations
// that manage credentials themselves - the foundation's token endpoints are the
// example, since issuing a token from a bearer token would defeat revocation.
func Session(api huma.API) []map[string][]string {
	return apisecurity.Session(api)
}

// User accepts the session cookie or, when auth.Service.TokenHumaConfig was
// applied to the huma config, a bearer token - matching what auth.Middleware
// plus auth.RequireUser actually let through. Apps that never enable API tokens
// get a session-only requirement, so the spec never references a scheme that
// isn't declared.
func User(api huma.API) []map[string][]string {
	return apisecurity.User(api)
}
