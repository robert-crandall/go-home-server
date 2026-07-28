// Package files solves file uploads once, so apps don't have to.
//
// Bytes go to a directory on disk - in practice a host folder bind-mounted into
// the container - and metadata goes to Postgres. Files are owned by the user
// who uploaded them and are only readable by that user.
//
// Wiring in an app:
//
//	fsvc, err := files.NewService(pool, files.Options{Dir: cfg.UploadDir})
//	if err != nil { /* missing/unwritable mount: fail fast */ }
//	files.Register(api, fsvc, func(ctx context.Context) (int64, error) {
//	    u, err := auth.RequireUser(ctx)
//	    return u.ID, err
//	})
//
// Images we can decode also get a small JPEG thumbnail stored beside the
// original (see thumb.go), so a photo grid doesn't pull down full-size photos.
//
// Deliberate non-goals: no S3/storage abstraction (one implementation is not an
// interface), no dedup, no per-user quota (the volume's size is the quota), and
// no orphan reconciliation. Save cleans up after every handled copy or insert
// error, so it never leaves a stray blob or a row without one. Orphaned blobs
// are still possible: Delete drops the row before unlinking, so a failed unlink
// leaks one, and a hard crash leaks one per operation in flight (a partial
// .tmp-* mid-copy, a finished blob between rename and insert, or an
// already-unrowed blob between delete and unlink). That costs disk and nothing
// else, and is not worth a background reaper in a home server.
//
// Assumes a single writer: one app process owns the upload directory, so there
// is no locking. It also assumes the huma API is humachi-backed, which is true
// for anything built on this foundation's server package.
package files

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DefaultMaxBytes is the per-upload size cap when Options.MaxBytes is unset.
// Large enough for a phone photo or a short video, small enough that a stray
// upload can't fill a home server's disk.
const DefaultMaxBytes int64 = 25 << 20

// uploadReadTimeout replaces huma's 5s default (and the server's 30s
// ReadTimeout) for the upload operation. A 25 MiB photo over a slow mobile
// connection takes longer than either.
const uploadReadTimeout = 2 * time.Minute

// tmpPrefix marks in-progress uploads. Blobs are written under this name and
// renamed into place, so a partially written file is never visible as a real
// blob and is obvious to a human browsing the mounted directory.
const tmpPrefix = ".tmp-"

// Options configures the service.
type Options struct {
	// Dir is the directory blobs are written to. Required, and it must already
	// exist - see NewService.
	Dir string
	// MaxBytes caps a single upload *request body* - the file plus multipart
	// boundaries and part headers, a couple hundred bytes of overhead. It is
	// enforced by the HTTP layer, not by Save: in-process callers are trusted.
	// <= 0 uses DefaultMaxBytes.
	MaxBytes int64
}

// Service stores and serves uploaded files.
type Service struct {
	db       *pgxpool.Pool
	dir      string
	maxBytes int64
}

// NewService constructs a file service, failing loudly if the upload directory
// isn't usable.
//
// It deliberately does not create the directory. The production shape is a host
// folder bind-mounted into the container; if that mount is missing or
// misspelled, MkdirAll would cheerfully create the path on the container's
// ephemeral layer and every uploaded photo would vanish on the next redeploy.
// Requiring the directory to already exist turns a broken mount into a startup
// crash. The probe write catches the read-only case for the same reason.
func NewService(db *pgxpool.Pool, opts Options) (*Service, error) {
	if opts.Dir == "" {
		return nil, errors.New("files: Dir is required (set UPLOAD_DIR)")
	}
	info, err := os.Stat(opts.Dir)
	if err != nil {
		return nil, fmt.Errorf("files: upload dir %q is not accessible (is the volume mounted?): %w", opts.Dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("files: upload dir %q is not a directory", opts.Dir)
	}

	probe := filepath.Join(opts.Dir, tmpPrefix+"probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return nil, fmt.Errorf("files: upload dir %q is not writable: %w", opts.Dir, err)
	}
	if err := os.Remove(probe); err != nil {
		return nil, fmt.Errorf("files: cannot clean up in upload dir %q: %w", opts.Dir, err)
	}

	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Service{db: db, dir: opts.Dir, maxBytes: maxBytes}, nil
}

// MaxBytes returns the per-upload size cap in bytes.
func (s *Service) MaxBytes() int64 { return s.maxBytes }

// File is the metadata for one stored file.
type File struct {
	ID          int64     `json:"id"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"contentType"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"createdAt"`
	// HasThumbnail reports whether GET /api/files/{id}/thumbnail will serve
	// something. It is false for formats we can't decode and for files stored
	// before thumbnails existed; clients fall back to the original.
	HasThumbnail bool `json:"hasThumbnail"`
}

// ErrNotFound is returned when a file doesn't exist or belongs to another user.
// Those two cases are deliberately indistinguishable so file IDs can't be
// probed for existence.
var ErrNotFound = errors.New("files: not found")

// List returns a user's files, newest first.
func (s *Service) List(ctx context.Context, userID int64) ([]File, error) {
	rows, err := s.db.Query(ctx,
		`SELECT id, filename, content_type, size_bytes, created_at, has_thumbnail
		   FROM files WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.Filename, &f.ContentType, &f.Size, &f.CreatedAt, &f.HasThumbnail); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Save streams one uploaded file to disk and records it. The client's filename
// is stored for display but never used to build a path.
func (s *Service) Save(ctx context.Context, userID int64, filename string, r io.Reader) (File, error) {
	key, err := storageKey(filename)
	if err != nil {
		return File{}, err
	}

	tmpPath := filepath.Join(s.dir, tmpPrefix+key)
	dstPath := filepath.Join(s.dir, key)

	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return File{}, fmt.Errorf("files: create: %w", err)
	}

	size, contentType, err := copyAndSniff(tmp, r)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return File{}, err
	}

	if err := os.Rename(tmpPath, dstPath); err != nil {
		_ = os.Remove(tmpPath)
		return File{}, fmt.Errorf("files: store: %w", err)
	}

	// Before the insert, so has_thumbnail is only ever true for a thumbnail
	// already renamed into place - the row can't outrun the blob.
	hasThumb := s.writeThumbnail(key, contentType)

	f := File{Filename: displayName(filename), ContentType: contentType, Size: size, HasThumbnail: hasThumb}
	err = s.db.QueryRow(ctx,
		`INSERT INTO files (user_id, storage_key, filename, content_type, size_bytes, has_thumbnail)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id, created_at`,
		userID, key, f.Filename, f.ContentType, f.Size, f.HasThumbnail,
	).Scan(&f.ID, &f.CreatedAt)
	if err != nil {
		// Both blobs are unreferenced now, so drop them rather than leak them.
		_ = os.Remove(dstPath)
		_ = s.removeThumbnail(key)
		return File{}, err
	}
	return f, nil
}

// Delete removes a user's file, row first. If an unlink fails the row is
// already gone, which is the right way round: the app never shows a broken
// entry, and the worst case is one orphaned blob.
func (s *Service) Delete(ctx context.Context, userID, id int64) error {
	var key string
	err := s.db.QueryRow(ctx,
		`DELETE FROM files WHERE id = $1 AND user_id = $2 RETURNING storage_key`,
		id, userID).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	// Both unlinks are attempted even if the first fails, so one failure can't
	// strand the other blob forever.
	var blobErr error
	if err := os.Remove(filepath.Join(s.dir, key)); err != nil && !os.IsNotExist(err) {
		blobErr = fmt.Errorf("files: delete blob: %w", err)
	}
	return errors.Join(blobErr, s.removeThumbnail(key))
}

// meta fetches a user's file row along with its storage key. A file that
// doesn't exist and one owned by someone else are deliberately the same error.
func (s *Service) meta(ctx context.Context, userID, id int64) (File, string, error) {
	var f File
	var key string
	err := s.db.QueryRow(ctx,
		`SELECT id, storage_key, filename, content_type, size_bytes, created_at, has_thumbnail
		   FROM files WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&f.ID, &key, &f.Filename, &f.ContentType, &f.Size, &f.CreatedAt, &f.HasThumbnail)
	if errors.Is(err, pgx.ErrNoRows) {
		return File{}, "", ErrNotFound
	}
	if err != nil {
		return File{}, "", err
	}
	return f, key, nil
}

// openBlob opens one blob belonging to an already-fetched row. The caller
// closes the handle.
func (s *Service) openBlob(ctx context.Context, userID, id int64, name string) (*os.File, error) {
	fh, err := os.Open(filepath.Join(s.dir, name))
	if err == nil {
		return fh, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("files: open blob: %w", err)
	}

	// Either a concurrent delete landed between the query and the open (the row
	// is gone too - a real 404), or the row outlived its blob, which means the
	// upload directory is damaged or unmounted. Those must not look alike: the
	// first is normal, the second is the kind of storage failure NewService
	// refuses to start on. Deliberately not %w-wrapping the *fs.PathError: huma
	// returns error text to the client, and the path is the only thing the wrap
	// would add - the branch condition already says it was ENOENT.
	var exists bool
	qerr := s.db.QueryRow(ctx,
		`SELECT true FROM files WHERE id = $1 AND user_id = $2`, id, userID,
	).Scan(&exists)
	if errors.Is(qerr, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if qerr != nil {
		return nil, fmt.Errorf("files: recheck missing blob for file %d: %w", id, qerr)
	}
	return nil, fmt.Errorf("files: blob %s for file %d is missing from the upload directory", name, id)
}

// open returns an open handle to a user's file plus its metadata. The caller
// closes the handle.
func (s *Service) open(ctx context.Context, userID, id int64) (*os.File, File, error) {
	f, key, err := s.meta(ctx, userID, id)
	if err != nil {
		return nil, File{}, err
	}
	fh, err := s.openBlob(ctx, userID, id, key)
	if err != nil {
		return nil, File{}, err
	}
	return fh, f, nil
}

// openThumb returns an open handle to a user's thumbnail. A file that has no
// thumbnail is ErrNotFound - the client is expected to fall back to the
// original, which File.HasThumbnail already told it about.
func (s *Service) openThumb(ctx context.Context, userID, id int64) (*os.File, File, error) {
	f, key, err := s.meta(ctx, userID, id)
	if err != nil {
		return nil, File{}, err
	}
	if !f.HasThumbnail {
		return nil, File{}, ErrNotFound
	}
	// has_thumbnail is only ever written once, at insert, so a missing blob
	// here can only mean a concurrent delete or a damaged directory - the same
	// two cases openBlob already tells apart.
	fh, err := s.openBlob(ctx, userID, id, thumbName(key))
	if err != nil {
		return nil, File{}, err
	}
	return fh, f, nil
}

// --- storage helpers -------------------------------------------------------

// storageKey builds an on-disk name: random bytes plus a sanitized extension.
// The extension is cosmetic - it only exists so the mounted folder is legible
// to a human - and the client's filename never contributes a path segment, so
// traversal is impossible by construction rather than by filtering.
func storageKey(filename string) (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("files: rand: %w", err)
	}
	return hex.EncodeToString(b[:]) + sanitizeExt(filename), nil
}

// sanitizeExt returns a safe ".ext" (possibly empty) for a client filename.
func sanitizeExt(filename string) string {
	ext := strings.ToLower(path.Ext(displayName(filename)))
	if ext == "" {
		return ""
	}
	ext = ext[1:] // drop the dot
	if len(ext) == 0 || len(ext) > 8 {
		return ""
	}
	for _, r := range ext {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return ""
		}
	}
	return "." + ext
}

// maxFilenameBytes bounds the stored display name. A hand-rolled client can
// send a filename up to Go's 10 MB multipart header limit, which fits under the
// body cap, and that name is echoed in a Content-Disposition header on every
// download - measured, an 8 KB name percent-encodes into a 24 KB header. 255 is
// the usual filesystem limit, so no real filename is affected. Cutting here can
// leave a name with a truncated extension, which sanitizeExt then carries onto
// the storage key; that only affects how the blob looks to a human browsing the
// mounted folder, since the served content type comes from sniffing the bytes.
const maxFilenameBytes = 255

// displayName reduces a client-supplied filename to its base name for display.
// Browsers send just a base name, but a hand-rolled client can send anything,
// so normalize both separators and use path.Base (not filepath.Base, which is
// a no-op on backslashes when the server runs on Linux). The result is forced
// to valid UTF-8 because filename is a text column and Postgres rejects raw
// bytes outright, and bounded because nothing else bounds it.
func displayName(filename string) string {
	name := path.Base(strings.ReplaceAll(filename, `\`, "/"))
	if name == "" || name == "." || name == "/" {
		return "upload"
	}

	name = strings.ToValidUTF8(name, "")
	if len(name) > maxFilenameBytes {
		// Cut on a rune boundary; a split rune would be invalid UTF-8 again.
		cut := maxFilenameBytes
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = name[:cut]
	}
	if name == "" {
		return "upload"
	}
	return name
}

// copyAndSniff streams r into w, detecting the content type from the leading
// bytes without consuming them.
func copyAndSniff(w io.Writer, r io.Reader) (int64, string, error) {
	head := make([]byte, 512)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return 0, "", fmt.Errorf("files: read upload: %w", err)
	}
	head = head[:n]

	written, err := w.Write(head)
	if err != nil {
		return int64(written), "", fmt.Errorf("files: write: %w", err)
	}
	rest, err := io.Copy(w, r)
	total := int64(written) + rest
	if err != nil {
		return total, "", fmt.Errorf("files: write: %w", err)
	}
	return total, detectContentType(head), nil
}

// detectContentType sniffs the leading bytes. Go's sniffer doesn't know the
// HEIF family, so an iPhone photo comes back as application/octet-stream and
// would render as a download link instead of an image. Fill that specific gap
// by reading the ISO base media format brand, which is in the same buffer.
// The client's declared Content-Type is never trusted.
func detectContentType(head []byte) string {
	ct := http.DetectContentType(head)
	if ct != "application/octet-stream" {
		return ct
	}
	switch isoBrand(head) {
	case "heic", "heix", "hevc", "hevx", "heim", "heis", "hevm", "hevs", "mif1", "msf1":
		return "image/heic"
	case "avif", "avis":
		return "image/avif"
	}
	return ct
}

// isoBrand returns the major brand of an ISO base media file ("ftyp" box), or
// "" if the bytes aren't one.
func isoBrand(head []byte) string {
	if len(head) < 12 || string(head[4:8]) != "ftyp" {
		return ""
	}
	return strings.ToLower(string(head[8:12]))
}

// baseMediaType strips any parameters from a media type and normalizes case,
// turning "text/plain; charset=utf-8" into "text/plain".
func baseMediaType(contentType string) string {
	base, _, _ := strings.Cut(contentType, ";")
	return strings.TrimSpace(strings.ToLower(base))
}

// contentDisposition decides whether a file renders in the browser or
// downloads. Only media types that can't execute script on this origin are
// inlined - an uploaded .html served inline would be stored XSS, and this
// foundation supports open registration, so multiple users can share an origin.
func contentDisposition(contentType, filename string) string {
	base := baseMediaType(contentType)

	// SVG is an image that can execute script, so it must never inline.
	inline := base != "image/svg+xml" &&
		(base == "text/plain" ||
			strings.HasPrefix(base, "image/") ||
			strings.HasPrefix(base, "video/") ||
			strings.HasPrefix(base, "audio/"))

	kind := "attachment"
	if inline {
		kind = "inline"
	}
	// FormatMediaType handles quoting and RFC 2231 encoding, so a filename with
	// a quote or non-ASCII characters can't produce a malformed header. It
	// returns "" if it can't, in which case fall back to the bare disposition.
	if h := mime.FormatMediaType(kind, map[string]string{"filename": filename}); h != "" {
		return h
	}
	return kind
}

// --- huma endpoints --------------------------------------------------------

// CurrentUserFunc resolves the acting user's ID from the request context,
// typically wrapping auth.RequireUser. Keeping it a function avoids a hard
// dependency from files onto a specific auth package.
type CurrentUserFunc func(ctx context.Context) (int64, error)

// uploadInput takes the raw multipart form rather than huma's
// MultipartFormFiles helper: that helper opens each part twice (once in
// readFile, once in its MIME validator) and never closes the second handle, so
// every upload over humachi's 8 KiB memory threshold - i.e. every photo - leaks
// a file descriptor until the finalizer runs.
type uploadInput struct {
	RawBody multipart.Form
}

// uploadRequestBody replaces the vague schema huma generates for a raw
// multipart body with the actual field this endpoint takes, so the generated
// client types are accurate. huma keeps a pre-set multipart schema as-is.
func uploadRequestBody() *huma.RequestBody {
	return &huma.RequestBody{
		Required: true,
		Content: map[string]*huma.MediaType{
			"multipart/form-data": {
				Schema: &huma.Schema{
					Type:     "object",
					Required: []string{"file"},
					Properties: map[string]*huma.Schema{
						"file": {
							Type:        "string",
							Format:      "binary",
							Description: "the file to upload",
						},
					},
				},
			},
		},
	}
}

// Register mounts file endpoints under /api/files.
func Register(api huma.API, svc *Service, currentUser CurrentUserFunc) {
	huma.Register(api, huma.Operation{
		OperationID:     "upload-file",
		Method:          http.MethodPost,
		Path:            "/api/files",
		Summary:         "Upload a file",
		Tags:            []string{"files"},
		DefaultStatus:   http.StatusCreated,
		RequestBody:     uploadRequestBody(),
		BodyReadTimeout: uploadReadTimeout,
		Middlewares:     huma.Middlewares{svc.guardUpload(api, currentUser)},
	}, func(ctx context.Context, in *uploadInput) (*struct{ Body File }, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}

		headers := in.RawBody.File["file"]
		if len(headers) == 0 {
			return nil, huma.Error422UnprocessableEntity(`multipart field "file" is required`)
		}
		fh := headers[0]

		part, err := fh.Open()
		if err != nil {
			return nil, huma.Error422UnprocessableEntity("cannot read uploaded file", err)
		}
		defer part.Close()

		f, err := svc.Save(ctx, userID, fh.Filename, part)
		if err != nil {
			return nil, err
		}
		return &struct{ Body File }{Body: f}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-files",
		Method:      http.MethodGet,
		Path:        "/api/files",
		Summary:     "List your files",
		Tags:        []string{"files"},
	}, func(ctx context.Context, _ *struct{}) (*struct{ Body []File }, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		list, err := svc.List(ctx, userID)
		if err != nil {
			return nil, err
		}
		return &struct{ Body []File }{Body: list}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "download-file",
		Method:      http.MethodGet,
		Path:        "/api/files/{id}",
		Summary:     "Download a file's contents",
		Tags:        []string{"files"},
		// huma can't infer a body schema from StreamResponse, so without this
		// the spec claims the endpoint returns nothing. The runtime
		// Content-Type is whatever was sniffed at upload; octet-stream is the
		// standard way to say "arbitrary bytes" in a spec.
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The file's raw bytes. The response Content-Type is the type detected at upload time, not necessarily application/octet-stream.",
				Content: map[string]*huma.MediaType{
					"application/octet-stream": {
						Schema: &huma.Schema{Type: "string", Format: "binary"},
					},
				},
			},
		},
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*huma.StreamResponse, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		fh, meta, err := svc.open(ctx, userID, in.ID)
		if errors.Is(err, ErrNotFound) {
			return nil, huma.Error404NotFound("file not found")
		}
		if err != nil {
			return nil, err
		}
		return streamBlob(fh, meta, meta.ContentType,
			contentDisposition(meta.ContentType, meta.Filename)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "download-file-thumbnail",
		Method:      http.MethodGet,
		Path:        "/api/files/{id}/thumbnail",
		Summary:     "Download a file's thumbnail",
		Description: "Serves a small JPEG preview. 404 when the file has no thumbnail - check hasThumbnail and fall back to the full file.",
		Tags:        []string{"files"},
		// Same reason as download-file: StreamResponse carries no inferable
		// schema. This one is always a JPEG.
		Responses: map[string]*huma.Response{
			"200": {
				Description: "The thumbnail's raw JPEG bytes.",
				Content: map[string]*huma.MediaType{
					"image/jpeg": {
						Schema: &huma.Schema{Type: "string", Format: "binary"},
					},
				},
			},
		},
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*huma.StreamResponse, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		fh, meta, err := svc.openThumb(ctx, userID, in.ID)
		if errors.Is(err, ErrNotFound) {
			return nil, huma.Error404NotFound("thumbnail not found")
		}
		if err != nil {
			return nil, err
		}
		// Always inline: it's a JPEG we generated, so it can't be the stored
		// XSS case contentDisposition guards against.
		return streamBlob(fh, meta, thumbContentType, "inline"), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "delete-file",
		Method:      http.MethodDelete,
		Path:        "/api/files/{id}",
		Summary:     "Delete a file",
		Tags:        []string{"files"},
	}, func(ctx context.Context, in *struct {
		ID int64 `path:"id"`
	}) (*struct{}, error) {
		userID, err := currentUser(ctx)
		if err != nil {
			return nil, err
		}
		if err := svc.Delete(ctx, userID, in.ID); err != nil {
			if errors.Is(err, ErrNotFound) {
				return nil, huma.Error404NotFound("file not found")
			}
			return nil, err
		}
		return &struct{}{}, nil
	})
}

// streamBlob builds the response that streams an open blob to the client. The
// handle is closed when the body has been written.
func streamBlob(fh *os.File, meta File, contentType, disposition string) *huma.StreamResponse {
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer fh.Close()
		r, w := humachi.Unwrap(hctx)
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", disposition)
		// A given id's bytes never change, but the response is per-user and
		// deletable, so it must NOT be cached without revalidation: browser
		// caches key on URL, not on session, so an `immutable` blob would still
		// be readable after logging out and back in as someone else, and a
		// deleted photo would linger client-side. `no-cache` still gets the
		// bandwidth win - the revalidation re-enters the handler, passes the
		// auth and ownership checks, and ServeContent answers 304 from
		// Last-Modified.
		w.Header().Set("Cache-Control", "private, no-cache")
		// ServeContent gives Range support (Safari's <video> requires it) and
		// conditional GETs. huma runs this func before writing status or
		// headers, so its 206/304 responses land intact. The modtime must be
		// non-zero or If-Modified-Since is skipped entirely.
		http.ServeContent(w, r, meta.Filename, meta.CreatedAt, fh)
	}}
}

// guardUpload builds the upload operation's middleware: it rejects anonymous
// requests, caps the request body, and cleans up multipart spool files. All
// three have to happen before huma parses the body, and huma runs operation
// middleware outside the body-parsing handler, so this is the only place that
// sees the request in time.
//
// Auth is checked here rather than only in the handler because huma resolves
// the multipart body first: without this, an unauthenticated POST would be
// spooled to disk in full and only then rejected with a 401.
//
// The size cap exists because huma only applies Operation.MaxBodyBytes on the
// non-multipart path; multipart goes to r.ParseMultipartForm, which spools
// anything past humachi's 8 KiB threshold to unbounded temp files.
//
// Those temp files are not reclaimed for us: net/http's cleanup calls RemoveAll
// on the request it parsed, but chi hands the handler a shallow copy via
// WithContext, so the original's MultipartForm stays nil and every spooled
// upload would otherwise sit in TempDir forever.
func (s *Service) guardUpload(api huma.API, currentUser CurrentUserFunc) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		r, w := humachi.Unwrap(ctx)

		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()

		if _, err := currentUser(r.Context()); err != nil {
			huma.WriteErr(api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}

		if r.ContentLength > s.maxBytes {
			// Reject before reading a byte so the client learns immediately.
			huma.WriteErr(api, ctx, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload is too large (max %d bytes)", s.maxBytes))
			return
		}
		// Backstop for chunked requests and clients that lie about
		// Content-Length. Overflow surfaces as huma's generic 422 rather than a
		// 413, because huma wraps multipart parse failures as plain validation
		// errors; capping the bytes is the part that matters. Passing the
		// writer lets net/http mark the request too large and close the
		// connection instead of trying to drain the rest of the body.
		r.Body = http.MaxBytesReader(w, r.Body, s.maxBytes)

		next(ctx)
	}
}
