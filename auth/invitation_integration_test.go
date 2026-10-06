package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

func invitationHarness(t *testing.T) *nameHarness {
	t.Helper()
	h := newNameHarness(t)
	h.svc.OpenRegistration = false
	ctx := context.Background()
	if _, err := h.svc.CreateUser(ctx, "owner@example.com", "supersecret"); err != nil {
		t.Fatal(err)
	}
	// App-owned data, deliberately absent from the shared migrations.
	_, err := h.svc.db.Exec(ctx, `
		CREATE TABLE invitation_test_tokens (token text PRIMARY KEY);
		CREATE TABLE invitation_test_members (
			user_id bigint PRIMARY KEY REFERENCES users(id) DEFERRABLE INITIALLY DEFERRED
		);
		INSERT INTO invitation_test_tokens VALUES ('valid')`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := h.svc.db.Exec(ctx, `DROP TABLE invitation_test_members, invitation_test_tokens`); err != nil {
			t.Error(err)
		}
	})
	h.svc.RegisterInvitation = func(ctx context.Context, tx pgx.Tx, u User, token string) error {
		if u.Name == "" {
			return huma.Error422UnprocessableEntity("name is required")
		}
		var consumed string
		err := tx.QueryRow(ctx, `DELETE FROM invitation_test_tokens WHERE token = $1 RETURNING token`, token).Scan(&consumed)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("invite: %w", huma.Error403Forbidden("invalid invitation"))
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO invitation_test_members VALUES ($1)`, u.ID)
		return err
	}
	return h
}

func assertInvitationRows(t *testing.T, h *nameHarness, users, sessions, invites, members int) {
	t.Helper()
	var got [4]int
	err := h.svc.db.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM users), (SELECT count(*) FROM sessions),
		(SELECT count(*) FROM invitation_test_tokens), (SELECT count(*) FROM invitation_test_members)`,
	).Scan(&got[0], &got[1], &got[2], &got[3])
	if err != nil {
		t.Fatal(err)
	}
	if want := [4]int{users, sessions, invites, members}; got != want {
		t.Fatalf("users/sessions/invites/members = %v, want %v", got, want)
	}
}

func TestInvitationRegistrationUsesNormalAuth(t *testing.T) {
	h := invitationHarness(t)
	u, cookie := h.register(`{"email":"guest@example.com","password":"supersecret","name":"  Guest  ","invitation":"valid"}`)
	if u.Name != "Guest" {
		t.Fatalf("name = %q, want trimmed name", u.Name)
	}
	assertInvitationRows(t, h, 2, 1, 0, 1)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	if got := decodeUser(t, h.do(req)); got.ID != u.ID {
		t.Fatalf("session user = %d, want %d", got.ID, u.ID)
	}
	loggedIn := decodeUser(t, h.json(http.MethodPost, "/api/auth/login",
		`{"email":"guest@example.com","password":"supersecret"}`))
	if loggedIn.ID != u.ID {
		t.Fatalf("password login user = %d, want %d", loggedIn.ID, u.ID)
	}
	if open, err := h.svc.RegistrationOpen(context.Background()); err != nil || open {
		t.Fatalf("uninvited registration open = %t, err = %v", open, err)
	}
}

func TestInvitationRegistrationPolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		existing   bool
		open       bool
		hook       bool
		invitation string
		want       int
		calls      int
	}{
		{"first user without invite", false, false, true, "", 200, 0},
		{"closed without invite", true, false, true, "", 403, 0},
		{"open without invite", true, true, true, "", 200, 0},
		{"closed with invite", true, false, true, "valid", 200, 1},
		{"open invalid invite", true, true, true, "invalid", 403, 1},
		{"first user invalid invite", false, false, true, "invalid", 403, 1},
		{"closed invalid invite", true, false, true, "invalid", 403, 1},
		{"first user unsupported invite", false, false, false, "valid", 403, 0},
		{"closed unsupported invite", true, false, false, "valid", 403, 0},
		{"open unsupported invite", true, true, false, "valid", 403, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newNameHarness(t)
			h.svc.OpenRegistration = tc.open
			if tc.existing {
				if _, err := h.svc.CreateUser(context.Background(), "owner@example.com", "supersecret"); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			if tc.hook {
				h.svc.RegisterInvitation = func(_ context.Context, _ pgx.Tx, _ User, token string) error {
					calls++
					if token != "valid" {
						return huma.Error403Forbidden("invalid invitation")
					}
					return nil
				}
			}
			rec := h.json(http.MethodPost, "/api/auth/register",
				fmt.Sprintf(`{"email":"guest@example.com","password":"supersecret","invitation":%q}`, tc.invitation))
			if rec.Code != tc.want || calls != tc.calls {
				t.Fatalf("status/calls = %d/%d, want %d/%d: %s", rec.Code, calls, tc.want, tc.calls, rec.Body.String())
			}
		})
	}
}

func TestInvitationValidationRollsBack(t *testing.T) {
	h := invitationHarness(t)
	for _, tc := range []struct {
		name, token string
		status      int
		message     string
	}{
		{"Guest", "invalid", 403, "invalid invitation"},
		{"  ", "valid", 422, "name is required"},
		{"Guest", strings.Repeat("a", 129), 422, "128"},
	} {
		rec := h.json(http.MethodPost, "/api/auth/register",
			fmt.Sprintf(`{"email":"guest@example.com","password":"supersecret","name":%q,"invitation":%q}`, tc.name, tc.token))
		if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.message) {
			t.Fatalf("status/body = %d/%s, want %d containing %q", rec.Code, rec.Body.String(), tc.status, tc.message)
		}
		if rec.Header().Get("Set-Cookie") != "" {
			t.Fatal("failed registration set a cookie")
		}
		assertInvitationRows(t, h, 1, 0, 1, 0)
	}
}

func TestInvitationTransactionRollsBackEveryWrite(t *testing.T) {
	for _, stage := range []string{"hook", "session", "commit"} {
		t.Run(stage, func(t *testing.T) {
			h := invitationHarness(t)
			consume := h.svc.RegisterInvitation
			h.svc.RegisterInvitation = func(ctx context.Context, tx pgx.Tx, u User, token string) error {
				if err := consume(ctx, tx, u, token); err != nil {
					return err
				}
				switch stage {
				case "hook":
					return errors.New("app membership failed")
				case "session":
					// NOT VALID skips existing rows but rejects the upcoming
					// session insert. Transaction rollback removes the constraint.
					_, err := tx.Exec(ctx, `ALTER TABLE sessions ADD CONSTRAINT invitation_test_reject CHECK (false) NOT VALID`)
					return err
				default:
					// Deferred FK failure happens at commit, after the session
					// insert succeeded.
					_, err := tx.Exec(ctx, `INSERT INTO invitation_test_members VALUES (-1)`)
					return err
				}
			}
			rec := h.json(http.MethodPost, "/api/auth/register",
				`{"email":"guest@example.com","password":"supersecret","name":"Guest","invitation":"valid"}`)
			if rec.Code != http.StatusInternalServerError || rec.Header().Get("Set-Cookie") != "" {
				t.Fatalf("failed %s: status %d, cookie %q: %s", stage, rec.Code, rec.Header().Get("Set-Cookie"), rec.Body.String())
			}
			assertInvitationRows(t, h, 1, 0, 1, 0)
			h.svc.RegisterInvitation = consume
			h.register(`{"email":"guest@example.com","password":"supersecret","name":"Guest","invitation":"valid"}`)
			assertInvitationRows(t, h, 2, 1, 0, 1)
		})
	}
}

func TestInvitationIsSingleUseUnderConcurrency(t *testing.T) {
	for _, open := range []bool{false, true} {
		t.Run(fmt.Sprintf("open=%t", open), func(t *testing.T) {
			h := invitationHarness(t)
			h.svc.OpenRegistration = open
			const n = 6
			start := make(chan struct{})
			results := make(chan int, n)
			var wg sync.WaitGroup
			for i := range n {
				wg.Go(func() {
					<-start
					rec := h.json(http.MethodPost, "/api/auth/register",
						fmt.Sprintf(`{"email":"guest%d@example.com","password":"supersecret","name":"Guest","invitation":"valid"}`, i))
					results <- rec.Code
				})
			}
			close(start)
			wg.Wait()
			close(results)
			success := 0
			for status := range results {
				switch status {
				case http.StatusOK:
					success++
				case http.StatusForbidden:
				default:
					t.Errorf("unexpected status: %d", status)
				}
			}
			if success != 1 {
				t.Fatalf("successful registrations = %d, want 1", success)
			}
			assertInvitationRows(t, h, 2, 1, 0, 1)
		})
	}
}
