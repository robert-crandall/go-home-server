package auth

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
)

func TestTokenHumaConfigEnablesBearerAuth(t *testing.T) {
	svc := NewService(nil, false)
	cfg := huma.DefaultConfig("test", "1.0.0")
	cfg.Components.SecuritySchemes = map[string]*huma.SecurityScheme{
		"existing": {Type: "http", Scheme: "basic"},
	}

	cfg = svc.TokenHumaConfig(cfg)

	if !svc.apiTokensEnabled {
		t.Fatal("TokenHumaConfig did not enable bearer authentication")
	}
	if cfg.Components.SecuritySchemes["existing"] == nil {
		t.Fatal("TokenHumaConfig discarded an existing security scheme")
	}
	session := cfg.Components.SecuritySchemes["session"]
	if session == nil || session.Type != "apiKey" || session.In != "cookie" || session.Name != "session" {
		t.Fatalf("session scheme = %#v, want session cookie apiKey", session)
	}
	bearer := cfg.Components.SecuritySchemes["bearer"]
	if bearer == nil || bearer.Type != "http" || bearer.Scheme != "bearer" {
		t.Fatalf("bearer scheme = %#v, want HTTP bearer", bearer)
	}
}

func TestRegisterTokensRequiresTokenHumaConfig(t *testing.T) {
	svc := NewService(nil, false)
	api := humachi.New(chi.NewMux(), huma.DefaultConfig("test", "1.0.0"))

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("RegisterTokens did not reject missing TokenHumaConfig")
		}
		if message := fmt.Sprint(recovered); !strings.Contains(message, "HumaConfig: authSvc.TokenHumaConfig") {
			t.Fatalf("RegisterTokens panic = %q, want TokenHumaConfig migration guidance", message)
		}
	}()

	svc.RegisterTokens(api)
}
