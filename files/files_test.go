package files

import (
	"bytes"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeExt(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "photo.jpg", ".jpg"},
		{"uppercase", "PHOTO.JPEG", ".jpeg"},
		{"heic", "IMG_1234.HEIC", ".heic"},
		{"no extension", "README", ""},
		{"trailing dot", "weird.", ""},
		{"hidden file", ".env", ".env"},
		{"too long", "archive.superlongext", ""},
		{"non-alnum", "sneaky.j-pg", ""},
		{"unicode ext", "photo.jpé", ""},
		{"traversal", "../../../etc/passwd.sh", ".sh"},
		{"windows traversal", `..\..\windows\system32\evil.exe`, ".exe"},
		{"nul-ish", "photo.jp\x00g", ""},
		{"double extension", "photo.jpg.php", ".php"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeExt(c.in); got != c.want {
				t.Errorf("sanitizeExt(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// The storage key must never let a client filename influence the path, no
// matter what it contains.
func TestStorageKeyStaysInDir(t *testing.T) {
	dir := "/srv/uploads"
	for _, name := range []string{
		"../../etc/passwd",
		`..\..\evil.exe`,
		"/etc/shadow",
		"a/b/c.jpg",
		"photo.jpg",
		"",
	} {
		key, err := storageKey(name)
		if err != nil {
			t.Fatalf("storageKey(%q): %v", name, err)
		}
		if strings.ContainsAny(key, `/\`) {
			t.Fatalf("storageKey(%q) = %q contains a separator", name, key)
		}
		joined := filepath.Join(dir, key)
		if filepath.Dir(joined) != dir {
			t.Fatalf("storageKey(%q) escaped: %q", name, joined)
		}
	}
}

func TestStorageKeyIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		k, err := storageKey("photo.jpg")
		if err != nil {
			t.Fatal(err)
		}
		if seen[k] {
			t.Fatalf("duplicate storage key %q", k)
		}
		seen[k] = true
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"photo.jpg":                  "photo.jpg",
		"../../etc/passwd":           "passwd",
		`C:\Users\bob\photo.jpg`:     "photo.jpg",
		"/absolute/path/to/file.png": "file.png",
		"":                           "upload",
		"/":                          "upload",
		"..":                         "..",
	}
	for in, want := range cases {
		if got := displayName(in); got != want {
			t.Errorf("displayName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ftyp-based detection exists specifically so iPhone photos render as images
// instead of downloads. Go's own sniffer returns octet-stream for these.
func TestDetectContentType(t *testing.T) {
	// A real ftyp box, padded to its declared size - Go's mp4 sniffer bails if
	// the buffer is shorter than the box header claims.
	ftyp := func(brand string) []byte {
		b := append([]byte{0, 0, 0, 0x18}, []byte("ftyp")...)
		b = append(b, []byte(brand)...)
		return append(b, make([]byte, 24-len(b))...)
	}
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"heic", ftyp("heic"), "image/heic"},
		{"heix", ftyp("heix"), "image/heic"},
		{"mif1", ftyp("mif1"), "image/heic"},
		{"avif", ftyp("avif"), "image/avif"},
		{"uppercase brand", ftyp("HEIC"), "image/heic"},
		{"mp4 still wins", ftyp("mp42"), "video/mp4"},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0, 0, 0, 0, 0, 0, 0}, "image/jpeg"},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"), "image/png"},
		{"unknown ftyp brand", ftyp("qt  "), "application/octet-stream"},
		{"random bytes", []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0a, 0x0b, 0x0c}, "application/octet-stream"},
		{"too short for ftyp", []byte("ftyp"), "text/plain; charset=utf-8"},
		{"empty", nil, "text/plain; charset=utf-8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := detectContentType(c.head); got != c.want {
				t.Errorf("detectContentType(%q) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// Anything that could execute script on this origin must download, not render.
func TestContentDisposition(t *testing.T) {
	cases := []struct {
		contentType string
		filename    string
		wantPrefix  string
	}{
		{"image/jpeg", "photo.jpg", "inline"},
		{"image/heic", "IMG_1.heic", "inline"},
		{"video/mp4", "clip.mp4", "inline"},
		{"audio/mpeg", "song.mp3", "inline"},
		{"text/plain; charset=utf-8", "notes.txt", "inline"},
		{"text/html; charset=utf-8", "evil.html", "attachment"},
		{"image/svg+xml", "evil.svg", "attachment"},
		{"application/octet-stream", "blob.bin", "attachment"},
		{"application/pdf", "doc.pdf", "attachment"},
	}
	for _, c := range cases {
		got := contentDisposition(c.contentType, c.filename)
		if !strings.HasPrefix(got, c.wantPrefix) {
			t.Errorf("contentDisposition(%q, %q) = %q, want prefix %q",
				c.contentType, c.filename, got, c.wantPrefix)
		}
	}
}

// A hostile filename must not be able to break out of the header value.
func TestContentDispositionEscapesFilename(t *testing.T) {
	for _, name := range []string{
		`ev"il.png`,
		"ev\r\nX-Injected: yes.png",
		"наташа.png",
		"a;b=c.png",
	} {
		got := contentDisposition("image/png", name)
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("contentDisposition(%q) leaked a newline: %q", name, got)
		}
		if got == "" {
			t.Errorf("contentDisposition(%q) returned empty", name)
		}
		if _, _, err := mime.ParseMediaType(got); err != nil {
			t.Errorf("contentDisposition(%q) = %q is unparseable: %v", name, got, err)
		}
	}
}

func TestCopyAndSniff(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
		wantCT  string
	}{
		{"empty", nil, "text/plain; charset=utf-8"},
		{"short", []byte("hello"), "text/plain; charset=utf-8"},
		{"exactly 512", bytes.Repeat([]byte("a"), 512), "text/plain; charset=utf-8"},
		{"over 512", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, bytes.Repeat([]byte{0x11}, 5000)...), "image/jpeg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			n, ct, err := copyAndSniff(&buf, bytes.NewReader(c.payload))
			if err != nil {
				t.Fatal(err)
			}
			if n != int64(len(c.payload)) {
				t.Errorf("size = %d, want %d", n, len(c.payload))
			}
			if !bytes.Equal(buf.Bytes(), c.payload) {
				t.Error("bytes written differ from bytes read")
			}
			if ct != c.wantCT {
				t.Errorf("content type = %q, want %q", ct, c.wantCT)
			}
		})
	}
}

type errReader struct{ afterN int }

func (e *errReader) Read(p []byte) (int, error) {
	if e.afterN <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	n := min(len(p), e.afterN)
	e.afterN -= n
	return n, nil
}

func TestCopyAndSniffPropagatesReadError(t *testing.T) {
	var buf bytes.Buffer
	// Fails partway through the tail copy, past the 512-byte sniff window.
	if _, _, err := copyAndSniff(&buf, &errReader{afterN: 600}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestNewServiceRejectsBadDir(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		if _, err := NewService(nil, Options{Dir: ""}); err == nil {
			t.Fatal("expected an error for an empty dir")
		}
	})

	// The whole point: a missing bind mount must crash at boot rather than get
	// created on the container's ephemeral layer.
	t.Run("missing", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "not-mounted")
		if _, err := NewService(nil, Options{Dir: missing}); err == nil {
			t.Fatal("expected an error for a missing dir")
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Fatal("NewService created the directory; it must not")
		}
	})

	t.Run("not a directory", func(t *testing.T) {
		f := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewService(nil, Options{Dir: f}); err == nil {
			t.Fatal("expected an error for a non-directory")
		}
	})

	t.Run("read only", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		dir := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		if _, err := NewService(nil, Options{Dir: dir}); err == nil {
			t.Fatal("expected an error for a read-only dir")
		}
	})
}

func TestNewServiceLeavesNoProbeFile(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(nil, Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("upload dir is not clean: %v", entries)
	}
	if svc.MaxBytes() != DefaultMaxBytes {
		t.Errorf("MaxBytes = %d, want %d", svc.MaxBytes(), DefaultMaxBytes)
	}
}

func TestNewServiceHonorsMaxBytes(t *testing.T) {
	svc, err := NewService(nil, Options{Dir: t.TempDir(), MaxBytes: 1234})
	if err != nil {
		t.Fatal(err)
	}
	if svc.MaxBytes() != 1234 {
		t.Errorf("MaxBytes = %d, want 1234", svc.MaxBytes())
	}
}
