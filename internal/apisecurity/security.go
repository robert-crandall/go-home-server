// Package apisecurity keeps the foundation's runtime authentication policy and
// OpenAPI security metadata on the same scheme names.
package apisecurity

import "github.com/danielgtaylor/huma/v2"

const (
	sessionScheme = "session"
	bearerScheme  = "bearer"
)

// ConfigureTokenAuth installs the session-cookie and bearer-token schemes.
func ConfigureTokenAuth(cfg huma.Config) huma.Config {
	if cfg.Components == nil {
		cfg.Components = &huma.Components{}
	}
	if cfg.Components.SecuritySchemes == nil {
		cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	cfg.Components.SecuritySchemes[sessionScheme] = sessionDefinition()
	cfg.Components.SecuritySchemes[bearerScheme] = &huma.SecurityScheme{
		Type:   "http",
		Scheme: "bearer",
	}
	return cfg
}

// Public overrides any top-level security requirement.
func Public() []map[string][]string {
	return []map[string][]string{}
}

// Session documents session-cookie authentication.
func Session(api huma.API) []map[string][]string {
	ensureSessionScheme(api)
	return []map[string][]string{
		{sessionScheme: []string{}},
	}
}

// User documents every credential type enabled on the API.
func User(api huma.API) []map[string][]string {
	requirements := Session(api)
	if BearerConfigured(api) {
		requirements = append(requirements, map[string][]string{
			bearerScheme: []string{},
		})
	}
	return requirements
}

// BearerConfigured reports whether the API advertises bearer authentication.
func BearerConfigured(api huma.API) bool {
	components := api.OpenAPI().Components
	return components != nil &&
		components.SecuritySchemes != nil &&
		components.SecuritySchemes[bearerScheme] != nil
}

func ensureSessionScheme(api huma.API) {
	spec := api.OpenAPI()
	if spec.Components == nil {
		spec.Components = &huma.Components{}
	}
	if spec.Components.SecuritySchemes == nil {
		spec.Components.SecuritySchemes = map[string]*huma.SecurityScheme{}
	}
	if spec.Components.SecuritySchemes[sessionScheme] == nil {
		spec.Components.SecuritySchemes[sessionScheme] = sessionDefinition()
	}
}

func sessionDefinition() *huma.SecurityScheme {
	return &huma.SecurityScheme{
		Type: "apiKey",
		In:   "cookie",
		Name: "session",
	}
}
