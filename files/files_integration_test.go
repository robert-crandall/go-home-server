package files

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/robert-crandall/go-home-server/db"
	"github.com/robert-crandall/go-home-server/migrations"
)

// These tests need a real Postgres. They skip cleanly when TEST_DATABASE_URL is
// unset so unit tests still run anywhere.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping DB integration test")
	}
	if err := db.Migrate(url, db.MigrationSource{
		FS: migrations.FS, Dir: migrations.Dir, TableName: migrations.TableName,
	}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `DELETE FROM users`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	return pool
}

func makeUser(t *testing.T, pool *pgxpool.Pool, email string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(),
		`INSERT INTO users (email, password_hash) VALUES ($1, 'x') RETURNING id`,
		email).Scan(&id)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id
}

// actingUser is the user the test harness's endpoints run as. Tests flip it to
// check cross-user isolation. authErr makes currentUser fail, standing in for
// an anonymous request.
type harness struct {
	t       *testing.T
	server  *httptest.Server
	dir     string
	pool    *pgxpool.Pool
	userID  int64
	authErr error
}

// newHarness stands up the real stack - chi + humachi + Register - so the
// multipart path, the upload middleware, and the streaming download are all
// exercised end to end rather than mocked.
func newHarness(t *testing.T, maxBytes int64) *harness {
	t.Helper()
	pool := testPool(t)
	dir := t.TempDir()

	svc, err := NewService(pool, Options{Dir: dir, MaxBytes: maxBytes})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	h := &harness{t: t, dir: dir, pool: pool}
	router := chi.NewMux()
	api := humachi.New(router, huma.DefaultConfig("test", "1.0.0"))
	Register(api, svc, func(context.Context) (int64, error) {
		if h.authErr != nil {
			return 0, h.authErr
		}
		return h.userID, nil
	})

	h.server = httptest.NewServer(router)
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) upload(filename string, body []byte) *http.Response {
	h.t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		h.t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		h.t.Fatal(err)
	}

	resp, err := http.Post(h.server.URL+"/api/files", mw.FormDataContentType(), &buf)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) get(path string) *http.Response {
	h.t.Helper()
	resp, err := http.Get(h.server.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	return resp
}

func (h *harness) blobCount() int {
	h.t.Helper()
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		h.t.Fatal(err)
	}
	return len(entries)
}

// The full round trip, with a payload comfortably over humachi's 8 KiB
// in-memory threshold so the upload really does spool to a temp file - the case
// a small fixture would silently skip.
func TestUploadRoundTrip(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	payload := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 64*1024)...)

	tmpBefore := tempFileCount(t)

	resp := h.upload("IMG_0001.JPG", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	var created File
	decodeJSON(t, resp.Body, &created)

	if created.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", created.Size, len(payload))
	}
	if created.ContentType != "image/jpeg" {
		t.Errorf("contentType = %q, want image/jpeg", created.ContentType)
	}
	if created.Filename != "IMG_0001.JPG" {
		t.Errorf("filename = %q", created.Filename)
	}

	// The multipart spool file must not survive the request; net/http's own
	// cleanup misses it because chi hands the handler a copied request.
	if after := tempFileCount(t); after > tmpBefore {
		t.Errorf("multipart temp files leaked: %d before, %d after", tmpBefore, after)
	}

	listResp := h.get("/api/files")
	defer listResp.Body.Close()
	var list []File
	decodeJSON(t, listResp.Body, &list)
	if len(list) != 1 || list[0].ID != created.ID {
		t.Fatalf("list = %+v, want the uploaded file", list)
	}

	dl := h.get(fmt.Sprintf("/api/files/%d", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d", dl.StatusCode)
	}
	got, err := io.ReadAll(dl.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("downloaded bytes differ from uploaded bytes")
	}
	if ct := dl.Header.Get("Content-Type"); ct != "image/jpeg" {
		t.Errorf("Content-Type = %q", ct)
	}
	if cd := dl.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "inline") {
		t.Errorf("Content-Disposition = %q, want inline", cd)
	}
	if dl.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff header")
	}
	// ServeContent adds this only when it can seek, i.e. Range works.
	if dl.Header.Get("Accept-Ranges") != "bytes" {
		t.Error("missing Accept-Ranges; Range requests would fail")
	}

	del, err := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/files/%d", h.server.URL, created.ID), nil)
	if err != nil {
		t.Fatal(err)
	}
	delResp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delResp.StatusCode)
	}
	if n := h.blobCount(); n != 0 {
		t.Errorf("%d blobs left on disk after delete", n)
	}
}

// Files are private: another user's ID must be indistinguishable from a
// nonexistent one.
func TestOtherUsersFileIsNotFound(t *testing.T) {
	h := newHarness(t, 0)
	alice := makeUser(t, h.pool, "alice@example.com")
	bob := makeUser(t, h.pool, "bob@example.com")

	h.userID = alice
	resp := h.upload("secret.txt", []byte("alice's data"))
	defer resp.Body.Close()
	var created File
	decodeJSON(t, resp.Body, &created)

	h.userID = bob
	dl := h.get(fmt.Sprintf("/api/files/%d", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusNotFound {
		t.Errorf("download status = %d, want 404", dl.StatusCode)
	}

	listResp := h.get("/api/files")
	defer listResp.Body.Close()
	var list []File
	decodeJSON(t, listResp.Body, &list)
	if len(list) != 0 {
		t.Errorf("bob sees %d of alice's files", len(list))
	}

	del, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/files/%d", h.server.URL, created.ID), nil)
	delResp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNotFound {
		t.Errorf("delete status = %d, want 404", delResp.StatusCode)
	}
	if n := h.blobCount(); n != 1 {
		t.Errorf("alice's blob count = %d, want 1 (bob deleted it)", n)
	}
}

// The two ways a blob can be missing at open time must not look alike. A row
// that outlived its blob means the upload directory is damaged, and that has to
// be loud; a file whose row is also gone is a normal 404.
//
// The true interleaving - open() reads the row, a delete removes both, then
// open() hits ENOENT - can't be forced without a hook in production code, so
// this covers the two reachable end states instead. The damaged-storage half is
// the one that exercises the ENOENT branch.
func TestMissingBlobIsNotFoundOnlyWhenTheRowIsGoneToo(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("photo.png", []byte("some bytes"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", resp.StatusCode)
	}
	var created File
	decodeJSON(t, resp.Body, &created)
	if created.ID == 0 {
		t.Fatal("upload returned no file id")
	}
	if n := h.blobCount(); n != 1 {
		t.Fatalf("blob count = %d, want 1", n)
	}

	// Damaged storage: the blob is gone but the row is not.
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if err := os.Remove(filepath.Join(h.dir, e.Name())); err != nil {
			t.Fatal(err)
		}
	}
	dl := h.get(fmt.Sprintf("/api/files/%d", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusInternalServerError {
		t.Errorf("damaged-storage download status = %d, want 500", dl.StatusCode)
	}

	// A deleted file is gone for good: row and blob both absent.
	if _, err := h.pool.Exec(context.Background(), `DELETE FROM files WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	gone := h.get(fmt.Sprintf("/api/files/%d", created.ID))
	defer gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Errorf("deleted-file download status = %d, want 404", gone.StatusCode)
	}
}

// An oversized upload is refused before the body is read, and leaves nothing on
// disk.
func TestOversizedUploadIsRejected(t *testing.T) {
	h := newHarness(t, 1024)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("big.bin", bytes.Repeat([]byte{0x01}, 4096))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
	if n := h.blobCount(); n != 0 {
		t.Errorf("%d blobs written for a rejected upload", n)
	}
}

// A chunked request has no Content-Length, so the cheap check can't fire and
// MaxBytesReader is the only thing standing between a lying client and the
// disk. The status is huma's generic validation failure, not 413; what matters
// is that the write never happens.
func TestChunkedOversizedUploadIsRejected(t *testing.T) {
	h := newHarness(t, 1024)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		part, err := mw.CreateFormFile("file", "big.bin")
		if err == nil {
			_, err = part.Write(bytes.Repeat([]byte{0x01}, 64*1024))
		}
		if err == nil {
			err = mw.Close()
		}
		_ = pw.CloseWithError(err)
	}()

	req, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/files", pr)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.ContentLength = -1 // force chunked

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want a client error", resp.StatusCode)
	}
	if n := h.blobCount(); n != 0 {
		t.Errorf("%d blobs written for a rejected upload", n)
	}
}

// An anonymous upload must be rejected *before* huma parses the multipart body,
// otherwise any unauthenticated client can make the server spool a full
// MaxBytes to disk just to be told 401.
func TestAnonymousUploadIsRejectedBeforeSpooling(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")
	h.authErr = errors.New("no session")

	// Comfortably past humachi's 8 KiB in-memory threshold, so parsing this
	// body would spool it to a temp file.
	payload := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 64*1024)...)

	tmpBefore := tempFileCount(t)

	resp := h.upload("IMG_0001.JPG", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	if got := tempFileCount(t); got != tmpBefore {
		t.Errorf("body was spooled before the auth check: %d temp files before, %d after", tmpBefore, got)
	}
	if n := h.blobCount(); n != 0 {
		t.Errorf("blobCount = %d, want 0", n)
	}
}

func TestUploadWithoutFileField(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if err := mw.WriteField("notafile", "hello"); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(h.server.URL+"/api/files", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", resp.StatusCode)
	}
	if n := h.blobCount(); n != 0 {
		t.Errorf("%d blobs written", n)
	}
}

// An uploaded HTML file must download rather than render, or it's stored XSS on
// the app's own origin.
func TestHTMLUploadIsServedAsAttachment(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("evil.html", []byte("<html><script>alert(1)</script></html>"))
	defer resp.Body.Close()
	var created File
	decodeJSON(t, resp.Body, &created)

	dl := h.get(fmt.Sprintf("/api/files/%d", created.ID))
	defer dl.Body.Close()
	if cd := dl.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Errorf("Content-Disposition = %q, want attachment", cd)
	}
}

// The client filename must never reach the filesystem path.
func TestTraversalFilenameStaysInDir(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("../../../../tmp/pwned.txt", []byte("hello"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 blob, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), "pwned") {
		t.Errorf("client filename reached the path: %q", entries[0].Name())
	}
}

// --- helpers ---------------------------------------------------------------

func tempFileCount(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "multipart-*"))
	if err != nil {
		t.Fatal(err)
	}
	return len(matches)
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func decodeJSON(t *testing.T, r io.Reader, v any) {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decode %q: %v", b, err)
	}
}

// A hand-rolled client can send a filename up to Go's 10 MB multipart header
// limit, which is well under the body cap. Unbounded, that name lands in a text
// column and then in a Content-Disposition header on every download.
func TestOversizedFilenameIsTruncated(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "longname@example.com")

	long := strings.Repeat("é", 4000) + ".jpg"
	resp := h.upload(long, []byte("hello"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	var f File
	decodeJSON(t, resp.Body, &f)

	if len(f.Filename) > 255 {
		t.Errorf("stored filename is %d bytes, want <= 255", len(f.Filename))
	}
	if !utf8.ValidString(f.Filename) {
		t.Errorf("stored filename is not valid UTF-8: %q", f.Filename)
	}

	dl := h.get(fmt.Sprintf("/api/files/%d", f.ID))
	defer dl.Body.Close()
	// RFC 5987 percent-encoding inflates a non-ASCII name roughly 3x, so the
	// header is bounded by a small constant multiple of maxFilenameBytes.
	if got := len(dl.Header.Get("Content-Disposition")); got > 4*maxFilenameBytes {
		t.Errorf("Content-Disposition is %d bytes, want a bounded header", got)
	}
}

// Postgres rejects invalid UTF-8 in a text column, so a hand-rolled client that
// puts raw bytes in the multipart filename must not turn into a 500.
func TestInvalidUTF8FilenameIsAccepted(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "badname@example.com")

	resp := h.upload("photo\xff\xfe.jpg", []byte("hello"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	var f File
	decodeJSON(t, resp.Body, &f)
	if !utf8.ValidString(f.Filename) {
		t.Errorf("stored filename is not valid UTF-8: %q", f.Filename)
	}
}

// The thumbnail path end to end: a decodable image gets a second blob on disk,
// the row says so, and the endpoint streams a real JPEG with the same
// per-user cache posture as the original.
func TestThumbnailRoundTrip(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("IMG_0002.png", pngBytes(t, 1200, 900))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	var created File
	decodeJSON(t, resp.Body, &created)
	if !created.HasThumbnail {
		t.Fatal("hasThumbnail = false, want true for a decodable PNG")
	}
	if n := h.blobCount(); n != 2 {
		t.Errorf("blob count = %d, want 2 (original + thumbnail)", n)
	}

	// The list endpoint has to carry the flag too - it's what the grid reads
	// to decide which URL to request.
	listResp := h.get("/api/files")
	defer listResp.Body.Close()
	var list []File
	decodeJSON(t, listResp.Body, &list)
	if len(list) != 1 || !list[0].HasThumbnail {
		t.Errorf("list = %+v, want one entry with hasThumbnail true", list)
	}

	dl := h.get(fmt.Sprintf("/api/files/%d/thumbnail", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusOK {
		t.Fatalf("thumbnail status = %d, want 200: %s", dl.StatusCode, readAll(t, dl.Body))
	}
	for header, want := range map[string]string{
		"Content-Type":           "image/jpeg",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "private, no-cache",
		"Content-Disposition":    "inline",
		// Proves ServeContent is wired rather than a plain io.Copy, which is
		// what gets conditional GETs and Range support.
		"Accept-Ranges": "bytes",
	} {
		if got := dl.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if dl.Header.Get("Last-Modified") == "" {
		t.Error("Last-Modified is empty, so revalidation can never produce a 304")
	}

	body, err := io.ReadAll(dl.Body)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("thumbnail body is not a decodable JPEG: %v", err)
	}
	if cfg.Width != thumbMaxEdge || cfg.Height != 384 {
		t.Errorf("thumbnail is %dx%d, want 512x384", cfg.Width, cfg.Height)
	}
	// Deliberately no assertion that the thumbnail is smaller than the
	// original: for a flat or synthetic image a PNG can compress far below its
	// own JPEG thumbnail. That's harmless - both files are a few KB - and
	// adding a "skip if larger" branch would buy nothing on the photos this
	// feature exists for, where the original is megabytes.
}

// An upload we can't decode still succeeds; it just has no thumbnail to serve.
func TestUndecodableUploadHasNoThumbnail(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	// Sniffs as image/jpeg on the magic bytes, then fails to decode.
	payload := append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x42}, 4096)...)
	resp := h.upload("broken.jpg", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201: %s", resp.StatusCode, readAll(t, resp.Body))
	}
	var created File
	decodeJSON(t, resp.Body, &created)
	if created.HasThumbnail {
		t.Error("hasThumbnail = true for an undecodable image, want false")
	}
	if n := h.blobCount(); n != 1 {
		t.Errorf("blob count = %d, want 1 (no thumbnail, no leftover tmp file)", n)
	}

	dl := h.get(fmt.Sprintf("/api/files/%d/thumbnail", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusNotFound {
		t.Errorf("thumbnail status = %d, want 404", dl.StatusCode)
	}
	// The original is still fully downloadable.
	orig := h.get(fmt.Sprintf("/api/files/%d", created.ID))
	defer orig.Body.Close()
	if orig.StatusCode != http.StatusOK {
		t.Errorf("original download status = %d, want 200", orig.StatusCode)
	}
}

// Delete has to reap both blobs, or the upload directory grows forever with
// thumbnails whose originals are gone.
func TestDeleteRemovesTheThumbnailToo(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("photo.png", pngBytes(t, 900, 900))
	defer resp.Body.Close()
	var created File
	decodeJSON(t, resp.Body, &created)
	if n := h.blobCount(); n != 2 {
		t.Fatalf("blob count = %d, want 2 before delete", n)
	}

	del, _ := http.NewRequest(http.MethodDelete,
		fmt.Sprintf("%s/api/files/%d", h.server.URL, created.ID), nil)
	delResp, err := http.DefaultClient.Do(del)
	if err != nil {
		t.Fatal(err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", delResp.StatusCode)
	}
	if n := h.blobCount(); n != 0 {
		t.Errorf("blob count = %d after delete, want 0", n)
	}
}

// The thumbnail endpoint runs the same ownership check as the original; it
// would be a neat hole to leave a downscaled copy of every photo readable.
func TestOtherUsersThumbnailIsNotFound(t *testing.T) {
	h := newHarness(t, 0)
	alice := makeUser(t, h.pool, "alice@example.com")
	bob := makeUser(t, h.pool, "bob@example.com")

	h.userID = alice
	resp := h.upload("private.png", pngBytes(t, 800, 600))
	defer resp.Body.Close()
	var created File
	decodeJSON(t, resp.Body, &created)
	if !created.HasThumbnail {
		t.Fatal("setup: expected a thumbnail")
	}

	h.userID = bob
	dl := h.get(fmt.Sprintf("/api/files/%d/thumbnail", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusNotFound {
		t.Errorf("bob's fetch of alice's thumbnail = %d, want 404", dl.StatusCode)
	}

	h.authErr = errors.New("no session")
	anon := h.get(fmt.Sprintf("/api/files/%d/thumbnail", created.ID))
	defer anon.Body.Close()
	if anon.StatusCode == http.StatusOK {
		t.Errorf("anonymous fetch = %d, want a failure", anon.StatusCode)
	}
}

// Same distinction the original download makes: a thumbnail blob that vanished
// while the row still claims one means the upload directory is damaged, and
// that must not be quietly reported as a missing thumbnail.
func TestMissingThumbnailBlobIsLoud(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")

	resp := h.upload("photo.png", pngBytes(t, 800, 600))
	defer resp.Body.Close()
	var created File
	decodeJSON(t, resp.Body, &created)
	if !created.HasThumbnail {
		t.Fatal("setup: expected a thumbnail")
	}

	entries, err := os.ReadDir(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	var removed bool
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), thumbSuffix) {
			if err := os.Remove(filepath.Join(h.dir, e.Name())); err != nil {
				t.Fatal(err)
			}
			removed = true
		}
	}
	if !removed {
		t.Fatalf("no file ending in %q in %v", thumbSuffix, entries)
	}

	dl := h.get(fmt.Sprintf("/api/files/%d/thumbnail", created.ID))
	defer dl.Body.Close()
	if dl.StatusCode != http.StatusInternalServerError {
		t.Errorf("damaged-storage thumbnail status = %d, want 500", dl.StatusCode)
	}

	// With the row gone too, it's an ordinary 404.
	if _, err := h.pool.Exec(context.Background(),
		`DELETE FROM files WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	gone := h.get(fmt.Sprintf("/api/files/%d/thumbnail", created.ID))
	defer gone.Body.Close()
	if gone.StatusCode != http.StatusNotFound {
		t.Errorf("deleted-file thumbnail status = %d, want 404", gone.StatusCode)
	}
}

// The thumbnail is written before the row exists, so a failed INSERT has to
// reap both blobs or the upload directory keeps a pair of files nothing can
// ever reference. A user_id with no matching users row trips the foreign key
// after everything on disk has already been written, which is the only way to
// reach this path without a hook in production code.
func TestFailedInsertRemovesBothBlobs(t *testing.T) {
	h := newHarness(t, 0)
	h.userID = makeUser(t, h.pool, "owner@example.com")
	// Delete the user out from under the request. The session is faked by the
	// harness, so the id survives into Save and only the FK notices.
	if _, err := h.pool.Exec(context.Background(),
		`DELETE FROM users WHERE id = $1`, h.userID); err != nil {
		t.Fatal(err)
	}

	resp := h.upload("photo.png", pngBytes(t, 800, 600))
	defer resp.Body.Close()
	if resp.StatusCode < 500 {
		t.Fatalf("upload status = %d, want a server error: %s",
			resp.StatusCode, readAll(t, resp.Body))
	}
	if n := h.blobCount(); n != 0 {
		entries, _ := os.ReadDir(h.dir)
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("upload directory holds %v after a failed insert, want it empty", names)
	}
}
