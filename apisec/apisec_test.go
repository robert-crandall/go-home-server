package apisec_test

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"

	"github.com/robert-crandall/go-home-server/apisec"
	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/server"
)

// newAPI builds the API an app would get from server.New. Passing
// authSvc.TokenHumaConfig as the HumaConfig hook is what enables API tokens, so
// withAPITokens is the only difference between an app that has them and one
// that doesn't.
func newAPI(t *testing.T, withAPITokens bool) huma.API {
	t.Helper()

	opts := server.Options{Title: "apisec test", Version: "0.0.0"}
	if withAPITokens {
		opts.HumaConfig = auth.NewService(nil, true).TokenHumaConfig
	}
	return server.New(opts).API
}

func declaredSchemes(t *testing.T, api huma.API) map[string]bool {
	t.Helper()

	declared := map[string]bool{}
	components := api.OpenAPI().Components
	if components == nil {
		return declared
	}
	for name := range components.SecuritySchemes {
		declared[name] = true
	}
	return declared
}

// assertSchemesDeclared is the invariant a hand-written security literal breaks:
// an operation must never reference a scheme that components.securitySchemes
// doesn't define.
func assertSchemesDeclared(t *testing.T, api huma.API, requirements []map[string][]string) {
	t.Helper()

	declared := declaredSchemes(t, api)
	for _, requirement := range requirements {
		for name := range requirement {
			if !declared[name] {
				t.Errorf("security requirement names scheme %q, which is not in components.securitySchemes", name)
			}
		}
	}
}

func TestPublicOverridesTopLevelSecurity(t *testing.T) {
	requirements := apisec.Public()
	if requirements == nil {
		t.Fatal("Public() = nil, want an empty list - nil inherits the top-level requirement instead of clearing it")
	}
	if len(requirements) != 0 {
		t.Fatalf("Public() = %v, want an empty list", requirements)
	}

	// The empty list only does its job if it survives into the spec as
	// "security": [], so check the marshalled document rather than the value.
	api := newAPI(t, false)
	huma.Register(api, huma.Operation{
		OperationID: "public-operation",
		Method:      http.MethodGet,
		Path:        "/api/public",
		Security:    apisec.Public(),
	}, func(context.Context, *struct{}) (*struct{}, error) {
		return &struct{}{}, nil
	})

	data, err := json.Marshal(api.OpenAPI())
	if err != nil {
		t.Fatalf("marshal OpenAPI spec: %v", err)
	}
	var document struct {
		Paths map[string]map[string]struct {
			Security json.RawMessage `json:"security"`
		} `json:"paths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("unmarshal OpenAPI spec: %v", err)
	}
	if got := string(document.Paths["/api/public"]["get"].Security); got != "[]" {
		t.Errorf("public operation security = %s, want []", got)
	}
}

func TestSessionRequiresTheCookieAlone(t *testing.T) {
	api := newAPI(t, false)

	got := apisec.Session(api)
	want := []map[string][]string{{"session": {}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Session() = %v, want %v", got, want)
	}
	assertSchemesDeclared(t, api, got)
}

func TestUserWithoutAPITokensIsSessionOnly(t *testing.T) {
	api := newAPI(t, false)

	got := apisec.User(api)
	// One map per alternative: session OR bearer. A single map holding both
	// would mean session AND bearer.
	want := []map[string][]string{{"session": {}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("User() = %v, want %v", got, want)
	}
	assertSchemesDeclared(t, api, got)
}

func TestUserWithAPITokensAlsoAcceptsBearer(t *testing.T) {
	api := newAPI(t, true)

	got := apisec.User(api)
	want := []map[string][]string{{"session": {}}, {"bearer": {}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("User() = %v, want %v", got, want)
	}
	assertSchemesDeclared(t, api, got)
}
