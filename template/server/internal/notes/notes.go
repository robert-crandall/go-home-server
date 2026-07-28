// Package notes is a sample per-user feature that demonstrates the app pattern:
// a pgx-backed store, huma handlers gated by auth.RequireUser, its own goose
// migration, and an optional push notification on write.
//
// Delete this package when starting a real app; it exists to show the shape.
package notes

import (
	"context"
	"embed"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robert-crandall/go-home-server/auth"
	"github.com/robert-crandall/go-home-server/notify"
)

// MigrationsFS holds this feature's own migrations, applied under the app's
// default goose version table (separate from the foundation's shared ones).
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// Note is a single user note.
type Note struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// Service is the notes feature.
type Service struct {
	db     *pgxpool.Pool
	notify *notify.Service
}

// NewService constructs the notes service. notify may be nil.
func NewService(db *pgxpool.Pool, n *notify.Service) *Service {
	return &Service{db: db, notify: n}
}

func (s *Service) list(ctx context.Context, userID int64) ([]Note, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, body, created_at FROM notes
		  WHERE user_id = $1 AND deleted_at IS NULL
		  ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	notes := []Note{}
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.Body, &n.CreatedAt); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
}

func (s *Service) create(ctx context.Context, userID int64, body string) (Note, error) {
	var n Note
	err := s.db.QueryRow(ctx,
		`INSERT INTO notes (user_id, body) VALUES ($1, $2)
		 RETURNING id, body, created_at`,
		userID, body,
	).Scan(&n.ID, &n.Body, &n.CreatedAt)
	return n, err
}

func (s *Service) softDelete(ctx context.Context, userID, id int64) error {
	_, err := s.db.Exec(ctx,
		`UPDATE notes SET deleted_at = now()
		  WHERE id = $1 AND user_id = $2 AND deleted_at IS NULL`,
		id, userID)
	return err
}

// Register mounts the notes endpoints under /api/notes.
func (s *Service) Register(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-notes",
		Method:      http.MethodGet,
		Path:        "/api/notes",
		Summary:     "List your notes",
		Tags:        []string{"notes"},
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []Note }, error) {
		u, err := auth.RequireUser(ctx)
		if err != nil {
			return nil, err
		}
		list, err := s.list(ctx, u.ID)
		if err != nil {
			return nil, err
		}
		return &struct{ Body []Note }{Body: list}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-note",
		Method:        http.MethodPost,
		Path:          "/api/notes",
		Summary:       "Create a note",
		Tags:          []string{"notes"},
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *struct {
		Body struct {
			Body string `json:"body" minLength:"1" doc:"Note text"`
		}
	}) (*struct{ Body Note }, error) {
		u, err := auth.RequireUser(ctx)
		if err != nil {
			return nil, err
		}
		n, err := s.create(ctx, u.ID, in.Body.Body)
		if err != nil {
			return nil, err
		}
		if s.notify != nil && s.notify.Enabled() {
			// Fire-and-forget so a slow push provider can't delay the write.
			// Detach from the request context (so it isn't cancelled when the
			// handler returns) but bound it with a timeout so a hung provider
			// can't leak the goroutine indefinitely.
			bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
			go func() {
				defer cancel()
				_ = s.notify.Send(bg, u.ID, notify.Payload{
					Title: "Note added",
					Body:  n.Body,
					URL:   "/notes",
				})
			}()
		}
		return &struct{ Body Note }{Body: n}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-note",
		Method:        http.MethodDelete,
		Path:          "/api/notes/{id}",
		Summary:       "Delete a note",
		Tags:          []string{"notes"},
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*struct{}, error) {
		u, err := auth.RequireUser(ctx)
		if err != nil {
			return nil, err
		}
		if err := s.softDelete(ctx, u.ID, in.ID); err != nil {
			return nil, err
		}
		return &struct{}{}, nil
	})
}
