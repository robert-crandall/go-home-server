package files

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestCodecForCoversTheSniffableImageTypes(t *testing.T) {
	for _, ct := range []string{"image/jpeg", "image/png", "image/gif", "image/webp"} {
		if _, ok := codecFor(ct); !ok {
			t.Errorf("codecFor(%q) = not supported, want supported", ct)
		}
	}
	// Only JPEG carries an orientation tag we read.
	for ct, wantEXIF := range map[string]bool{
		"image/jpeg": true,
		"image/png":  false,
		"image/gif":  false,
		"image/webp": false,
	} {
		c, _ := codecFor(ct)
		if c.exif != wantEXIF {
			t.Errorf("codecFor(%q).exif = %v, want %v", ct, c.exif, wantEXIF)
		}
	}
}

func TestCodecForRejectsEverythingElse(t *testing.T) {
	// image/svg+xml and image/heic are the two that matter: both are things a
	// user will actually upload and neither can be decoded here.
	for _, ct := range []string{
		"image/svg+xml", "image/heic", "image/heif", "image/avif", "image/bmp",
		"image/tiff", "text/plain", "application/octet-stream", "video/mp4", "",
	} {
		if _, ok := codecFor(ct); ok {
			t.Errorf("codecFor(%q) = supported, want unsupported", ct)
		}
	}
}

func TestCodecForIgnoresMediaTypeParameters(t *testing.T) {
	// Sniffing returns "text/plain; charset=utf-8" shaped values, so the
	// switch has to compare the base type or every parameterised image type
	// silently loses its thumbnail.
	if _, ok := codecFor("image/jpeg; charset=binary"); !ok {
		t.Error("codecFor with a parameter = unsupported, want supported")
	}
}

func TestThumbNamesCannotCollideWithStorageKeys(t *testing.T) {
	// The whole "derive the thumbnail path, don't store it" design rests on
	// this: a thumbnail name must never be a name Save could hand to an
	// original. storageKey is 32 hex chars plus sanitizeExt's output, and
	// sanitizeExt emits at most one dot; ".thumb.jpg" adds two.
	keyShape := regexp.MustCompile(`^[0-9a-f]{32}(\.[a-z0-9]{1,8})?$`)

	for _, filename := range []string{
		"photo.jpg", "photo.JPEG", "no-extension", "photo.thumb.jpg",
		"weird.name.with.dots.png", ".hidden", strings.Repeat("x", 300) + ".png",
		"photo.thumb", "a.jpg.thumb.jpg",
	} {
		key, err := storageKey(filename)
		if err != nil {
			t.Fatalf("storageKey(%q): %v", filename, err)
		}
		if !keyShape.MatchString(key) {
			t.Fatalf("storageKey(%q) = %q, which is outside the shape this invariant assumes", filename, key)
		}
		if !keyShape.MatchString(thumbName(key)) {
			continue // The point: a thumbnail name is never a valid storage key.
		}
		t.Errorf("thumbName(%q) = %q, which matches the storage key shape - the namespaces collide", key, thumbName(key))
	}
}

func TestWithinPixelBudget(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want bool
	}{
		{"typical phone photo", 4032, 3024, true},
		{"48MP phone photo", 8000, 6000, true},
		{"exactly at budget", maxThumbPixels / 1000, 1000, true},
		{"over budget", 30000, 30000, false},
		{"tall sliver over budget", 2, maxThumbPixels, false},
		// DecodeConfig is decoding attacker-controlled headers, so these must
		// be rejected rather than reaching the multiply.
		{"zero width", 0, 100, false},
		{"zero height", 100, 0, false},
		{"negative width", -1, 100, false},
		{"negative height", 100, -1, false},
		{"both max int", math.MaxInt, math.MaxInt, false},
		{"max int width", math.MaxInt, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := withinPixelBudget(tt.w, tt.h); got != tt.want {
				t.Errorf("withinPixelBudget(%d, %d) = %v, want %v", tt.w, tt.h, got, tt.want)
			}
		})
	}
}

func TestScaleToFitPreservesAspectRatioAndNeverUpscales(t *testing.T) {
	tests := []struct {
		name         string
		w, h         int
		wantW, wantH int
	}{
		{"landscape downscale", 4000, 3000, thumbMaxEdge, 384},
		{"portrait downscale", 3000, 4000, 384, thumbMaxEdge},
		{"square downscale", 2000, 2000, thumbMaxEdge, thumbMaxEdge},
		{"already small stays put", 100, 80, 100, 80},
		{"exactly at the limit stays put", thumbMaxEdge, 200, thumbMaxEdge, 200},
		{"single pixel stays put", 1, 1, 1, 1},
		// A 10000x1 panorama scales the short edge to 0.05px; clamping to 1
		// is what keeps image.NewRGBA from producing an empty image that
		// jpeg.Encode then rejects.
		{"extreme aspect ratio clamps the short edge to 1", 10000, 1, thumbMaxEdge, 1},
		{"extreme aspect ratio clamps the short edge to 1, rotated", 1, 10000, 1, thumbMaxEdge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := image.NewRGBA(image.Rect(0, 0, tt.w, tt.h))
			got := scaleToFit(src, thumbMaxEdge)
			if got.Bounds().Dx() != tt.wantW || got.Bounds().Dy() != tt.wantH {
				t.Errorf("scaleToFit(%dx%d) = %dx%d, want %dx%d",
					tt.w, tt.h, got.Bounds().Dx(), got.Bounds().Dy(), tt.wantW, tt.wantH)
			}
		})
	}
}

func TestScaleToFitCompositesTransparencyOverWhite(t *testing.T) {
	// JPEG has no alpha channel, so a fully transparent PNG would otherwise
	// encode as black - which looks like a broken thumbnail, not a
	// transparent one.
	src := image.NewRGBA(image.Rect(0, 0, 10, 10))
	// Leave it fully transparent (the zero value).
	got := scaleToFit(src, thumbMaxEdge)
	r, g, b, a := got.At(5, 5).RGBA()
	if r != 0xffff || g != 0xffff || b != 0xffff || a != 0xffff {
		t.Errorf("transparent pixel composited to (%d,%d,%d,%d), want opaque white", r, g, b, a)
	}
}

// grid renders a labelled image so an orientation transform can be asserted
// against what a human would draw. Each rune becomes one pixel with a unique
// colour.
func grid(t *testing.T, rows []string) *image.RGBA {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, len(rows[0]), len(rows)))
	for y, row := range rows {
		for x, r := range row {
			img.Set(x, y, color.RGBA{R: uint8(r), G: 0, B: 0, A: 255})
		}
	}
	return img
}

func gridString(img *image.RGBA) []string {
	rows := make([]string, img.Bounds().Dy())
	for y := range img.Bounds().Dy() {
		var b strings.Builder
		for x := range img.Bounds().Dx() {
			c := img.RGBAAt(x, y)
			b.WriteRune(rune(c.R))
		}
		rows[y] = b.String()
	}
	return rows
}

func TestApplyOrientation(t *testing.T) {
	// Source is deliberately non-square so a dimension swap is visible:
	//   ABC
	//   DEF
	src := []string{"ABC", "DEF"}

	tests := []struct {
		orientation int
		want        []string
	}{
		{1, []string{"ABC", "DEF"}},     // as stored
		{2, []string{"CBA", "FED"}},     // mirror horizontal
		{3, []string{"FED", "CBA"}},     // rotate 180
		{4, []string{"DEF", "ABC"}},     // mirror vertical
		{5, []string{"AD", "BE", "CF"}}, // transpose (main diagonal)
		{6, []string{"DA", "EB", "FC"}}, // rotate 90 clockwise
		{7, []string{"FC", "EB", "DA"}}, // transverse (anti-diagonal)
		{8, []string{"CF", "BE", "AD"}}, // rotate 90 counter-clockwise
		{0, []string{"ABC", "DEF"}},     // absent: leave alone
		{9, []string{"ABC", "DEF"}},     // out of range: leave alone
		{-1, []string{"ABC", "DEF"}},    // nonsense: leave alone
	}
	for _, tt := range tests {
		got := gridString(applyOrientation(grid(t, src), tt.orientation))
		if strings.Join(got, "/") != strings.Join(tt.want, "/") {
			t.Errorf("applyOrientation(orientation=%d) = %v, want %v", tt.orientation, got, tt.want)
		}
	}
}

// byteOrder is binary.ByteOrder plus the Append helpers, which live on a
// separate interface in encoding/binary.
type byteOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// exifJPEG builds a JPEG whose APP1 segment declares the given orientation.
// Building the bytes by hand (rather than shipping binary fixtures) is what
// makes the byte-order and truncation cases below writable at all.
func exifJPEG(t *testing.T, bo byteOrder, orientation uint16, opts ...func(*[]byte)) []byte {
	t.Helper()

	tiff := make([]byte, 0, 64)
	if bo == binary.BigEndian {
		tiff = append(tiff, 'M', 'M')
	} else {
		tiff = append(tiff, 'I', 'I')
	}
	tiff = bo.AppendUint16(tiff, 42)
	tiff = bo.AppendUint32(tiff, 8) // IFD0 starts right after the header.

	// Two entries, so the scan has to walk past one to find orientation.
	tiff = bo.AppendUint16(tiff, 2)
	// ImageWidth (0x0100), LONG, count 1.
	tiff = bo.AppendUint16(tiff, 0x0100)
	tiff = bo.AppendUint16(tiff, 4)
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint32(tiff, 640)
	// Orientation (0x0112), SHORT, count 1. The value sits in the high or low
	// half of the 4-byte value field depending on byte order.
	tiff = bo.AppendUint16(tiff, 0x0112)
	tiff = bo.AppendUint16(tiff, 3)
	tiff = bo.AppendUint32(tiff, 1)
	tiff = bo.AppendUint16(tiff, orientation)
	tiff = bo.AppendUint16(tiff, 0)
	tiff = bo.AppendUint32(tiff, 0) // No IFD1.

	app1 := append([]byte("Exif\x00\x00"), tiff...)
	for _, opt := range opts {
		opt(&app1)
	}

	out := []byte{0xff, 0xd8} // SOI
	// A real camera JPEG has JFIF APP0 before APP1, so include one: it proves
	// the scanner walks segments rather than assuming APP1 comes first.
	jfif := []byte("JFIF\x00\x01\x02\x00\x00\x01\x00\x01\x00\x00")
	out = append(out, 0xff, 0xe0)
	out = binary.BigEndian.AppendUint16(out, uint16(len(jfif)+2))
	out = append(out, jfif...)

	out = append(out, 0xff, 0xe1)
	out = binary.BigEndian.AppendUint16(out, uint16(len(app1)+2))
	out = append(out, app1...)
	out = append(out, 0xff, 0xda) // SOS: the scanner stops here.
	return out
}

func TestJPEGOrientationReadsBothByteOrders(t *testing.T) {
	for _, bo := range []byteOrder{binary.LittleEndian, binary.BigEndian} {
		for want := 1; want <= 8; want++ {
			got := jpegOrientation(exifJPEG(t, bo, uint16(want)))
			if got != want {
				t.Errorf("jpegOrientation(%v, orientation=%d) = %d, want %d", bo, want, got, want)
			}
		}
	}
}

func TestJPEGOrientationSurvivesHostileInput(t *testing.T) {
	// Every one of these is reachable: the bytes come straight off an
	// authenticated user's upload. None may panic, and all must report
	// specifically "no orientation" - a parser that returned some other
	// in-range value would silently rotate thumbnails of corrupt files.
	full := exifJPEG(t, binary.LittleEndian, 6)

	tests := map[string][]byte{
		"empty":                      {},
		"SOI only":                   {0xff, 0xd8},
		"marker with no length":      {0xff, 0xd8, 0xff, 0xe1},
		"length truncated":           {0xff, 0xd8, 0xff, 0xe1, 0x00},
		"length longer than payload": {0xff, 0xd8, 0xff, 0xe1, 0x7f, 0xff, 'E', 'x'},
		"length below its own size":  {0xff, 0xd8, 0xff, 0xe1, 0x00, 0x01, 'E', 'x'},
		"zero length":                {0xff, 0xd8, 0xff, 0xe1, 0x00, 0x00},
		"no SOI":                     append([]byte{0x00, 0x00}, full[2:]...),
		"all 0xff":                   bytes.Repeat([]byte{0xff}, 64),
		"all zero":                   make([]byte, 64),
		"APP1 without the Exif tag":  {0xff, 0xd8, 0xff, 0xe1, 0x00, 0x08, 'N', 'o', 't', 'E', 'x', 'i'},
		"bad TIFF byte order": exifJPEG(t, binary.LittleEndian, 6, func(a *[]byte) {
			(*a)[6], (*a)[7] = 'X', 'Y'
		}),
		"bad TIFF magic": exifJPEG(t, binary.LittleEndian, 6, func(a *[]byte) {
			(*a)[8], (*a)[9] = 0xff, 0xff
		}),
		"IFD offset past the buffer": exifJPEG(t, binary.LittleEndian, 6, func(a *[]byte) {
			binary.LittleEndian.PutUint32((*a)[10:], 0xfffffff0)
		}),
		"IFD offset before the header": exifJPEG(t, binary.LittleEndian, 6, func(a *[]byte) {
			binary.LittleEndian.PutUint32((*a)[10:], 0)
		}),
		"orientation value out of range": exifJPEG(t, binary.LittleEndian, 99),
		"orientation value zero":         exifJPEG(t, binary.LittleEndian, 0),
	}
	for name, in := range tests {
		if got := jpegOrientation(in); got != orientationNormal {
			t.Errorf("jpegOrientation(%s) = %d, want %d", name, got, orientationNormal)
		}
	}

	// Every truncation of a valid file, which covers "the stream was cut
	// short" exhaustively rather than at a few hand-picked offsets. Once the
	// APP1 segment is complete the tag is readable even with the rest of the
	// file missing, so the expected answer flips at that boundary.
	app1End := bytes.LastIndex(full, []byte{0xff, 0xda})
	if app1End <= 0 {
		t.Fatal("fixture has no SOS marker")
	}
	for n := range len(full) {
		want := orientationNormal
		if n >= app1End {
			want = 6
		}
		if got := jpegOrientation(full[:n]); got != want {
			t.Errorf("jpegOrientation(truncated to %d of %d bytes) = %d, want %d",
				n, len(full), got, want)
		}
	}
}

// A directory header can claim more entries than the buffer holds. The parser
// clamps the count to what is physically there rather than bailing, so the
// entries that do exist are still read - and, more to the point, the clamp is
// what keeps the loop's slicing in range.
func TestJPEGOrientationClampsAnOverstatedEntryCount(t *testing.T) {
	in := exifJPEG(t, binary.LittleEndian, 6, func(a *[]byte) {
		binary.LittleEndian.PutUint16((*a)[14:], 0xffff)
	})
	if got := jpegOrientation(in); got != 6 {
		t.Errorf("jpegOrientation = %d, want 6 - the entry is present, just after a bogus count", got)
	}

	// The case that actually exercises the clamp's bound: a directory claiming
	// 65535 entries with room for none. Built as a bare APP1 payload because
	// shortening a JPEG segment would just trip the segment-length check
	// first, never reaching this code.
	var payload []byte
	payload = append(payload, "Exif\x00\x00"...)
	payload = append(payload, 'I', 'I')
	payload = binary.LittleEndian.AppendUint16(payload, 42)
	payload = binary.LittleEndian.AppendUint32(payload, 8)
	payload = binary.LittleEndian.AppendUint16(payload, 0xffff) // and nothing after it
	if got := exifOrientation(payload); got != orientationNormal {
		t.Errorf("exifOrientation with an empty but overstated directory = %d, want %d",
			got, orientationNormal)
	}
}

func TestJPEGOrientationIgnoresATagOfTheWrongType(t *testing.T) {
	// A LONG-typed orientation is malformed; reading its first two bytes as a
	// SHORT would produce a plausible-looking wrong answer on one byte order
	// and a wildly wrong one on the other.
	in := exifJPEG(t, binary.LittleEndian, 6, func(a *[]byte) {
		// Entry 2 starts at 6 (Exif hdr) + 8 (TIFF hdr) + 2 (count) + 12 = 28.
		binary.LittleEndian.PutUint16((*a)[28+2:], 4) // type LONG
	})
	if got := jpegOrientation(in); got != orientationNormal {
		t.Errorf("jpegOrientation with a LONG orientation = %d, want %d", got, orientationNormal)
	}
}

// serviceForThumbs builds a Service backed by a temp dir. renderThumbnail and
// writeThumbnail never touch the database, so a nil pool is fine here.
func serviceForThumbs(t *testing.T) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	svc, err := NewService(nil, Options{Dir: dir})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc, dir
}

func writeBlob(t *testing.T, dir, key string, b []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, key), b, 0o600); err != nil {
		t.Fatalf("write blob: %v", err)
	}
}

func pngBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode: %v", err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatalf("jpeg.Encode: %v", err)
	}
	return buf.Bytes()
}

func TestWriteThumbnailAcrossEveryDecodableFormat(t *testing.T) {
	gifBytes := func() []byte {
		img := image.NewPaletted(image.Rect(0, 0, 800, 600), color.Palette{color.Black, color.White})
		var buf bytes.Buffer
		if err := gif.Encode(&buf, img, nil); err != nil {
			t.Fatalf("gif.Encode: %v", err)
		}
		return buf.Bytes()
	}()
	// A 40x20 lossless WebP, produced with Pillow. Kept as bytes because
	// x/image can decode WebP but cannot encode it, so it can't be generated
	// at test time.
	webpBytes, err := hex.DecodeString(
		"524946462e000000574542505650384c220000002f27c00400b93244f43f7611" +
			"d1ff0091b64d05dcbfe8c1a331188f9800aa3a50fd33")
	if err != nil {
		t.Fatalf("decode webp fixture: %v", err)
	}

	tests := []struct {
		contentType string
		body        []byte
	}{
		{"image/png", pngBytes(t, 800, 600)},
		{"image/jpeg", jpegBytes(t, 800, 600)},
		{"image/gif", gifBytes},
		{"image/webp", webpBytes},
	}
	for _, tt := range tests {
		t.Run(tt.contentType, func(t *testing.T) {
			svc, dir := serviceForThumbs(t)
			writeBlob(t, dir, "k", tt.body)

			if !svc.writeThumbnail("k", tt.contentType) {
				t.Fatal("writeThumbnail = false, want true")
			}
			out, err := os.ReadFile(filepath.Join(dir, thumbName("k")))
			if err != nil {
				t.Fatalf("read thumbnail: %v", err)
			}
			cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
			if err != nil {
				t.Fatalf("thumbnail is not a decodable JPEG: %v", err)
			}
			if cfg.Width > thumbMaxEdge || cfg.Height > thumbMaxEdge {
				t.Errorf("thumbnail is %dx%d, want both edges <= %d", cfg.Width, cfg.Height, thumbMaxEdge)
			}
		})
	}
}

func TestWriteThumbnailAppliesEXIFOrientation(t *testing.T) {
	// Browsers rotate the original using the EXIF tag, but a re-encoded
	// thumbnail has no tag - so without this the grid shows some phone photos
	// sideways next to correctly-oriented ones.
	svc, dir := serviceForThumbs(t)

	body := jpegBytes(t, 800, 400)
	// Splice a real APP1 orientation=6 (rotate 90 CW) segment in after SOI.
	app1 := exifJPEG(t, binary.LittleEndian, 6)
	// exifJPEG's output is SOI + APP0 + APP1 + SOS; take the APP1 marker
	// through to just before SOS and graft it onto a real image.
	start := bytes.Index(app1, []byte{0xff, 0xe1})
	end := bytes.LastIndex(app1, []byte{0xff, 0xda})
	oriented := append([]byte{}, body[:2]...)
	oriented = append(oriented, app1[start:end]...)
	oriented = append(oriented, body[2:]...)

	writeBlob(t, dir, "k", oriented)
	if !svc.writeThumbnail("k", "image/jpeg") {
		t.Fatal("writeThumbnail = false, want true")
	}
	out, err := os.ReadFile(filepath.Join(dir, thumbName("k")))
	if err != nil {
		t.Fatalf("read thumbnail: %v", err)
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	// 800x400 landscape, rotated a quarter turn, becomes portrait.
	if cfg.Width >= cfg.Height {
		t.Errorf("thumbnail is %dx%d, want portrait after a 90 degree rotation", cfg.Width, cfg.Height)
	}
}

func TestWriteThumbnailSkipsAnImageDeclaringTooManyPixels(t *testing.T) {
	// The 25 MiB upload cap bounds compressed bytes, not decoded ones. This is
	// a valid PNG header claiming 30000x30000 - 3.6 GB of RGBA - in 33 bytes.
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 0, 13)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 30000)
	ihdr = binary.BigEndian.AppendUint32(ihdr, 30000)
	ihdr = append(ihdr, 8, 6, 0, 0, 0) // 8-bit RGBA
	b.Write(binary.BigEndian.AppendUint32(nil, uint32(len(ihdr))))
	b.Write([]byte("IHDR"))
	b.Write(ihdr)
	b.Write([]byte{0, 0, 0, 0}) // CRC: DecodeConfig doesn't check it.

	svc, dir := serviceForThumbs(t)
	writeBlob(t, dir, "k", b.Bytes())

	if svc.writeThumbnail("k", "image/png") {
		t.Error("writeThumbnail = true for an over-budget image, want false")
	}
	if _, err := os.Stat(filepath.Join(dir, thumbName("k"))); !os.IsNotExist(err) {
		t.Errorf("thumbnail exists after an over-budget skip: %v", err)
	}
}

func TestWriteThumbnailSkipsUndecodableBytes(t *testing.T) {
	// Sniffing only reads the first 512 bytes, so a file can be typed as an
	// image and still fail to decode. Each of these fails during decode -
	// before any tmp file exists - so this covers the "no thumbnail, upload
	// survives" contract, not tmp-file cleanup.
	tests := map[string][]byte{
		"truncated png":  pngBytes(t, 800, 600)[:200],
		"truncated jpeg": jpegBytes(t, 800, 600)[:200],
		"png header then garbage": append(
			append([]byte{}, pngBytes(t, 80, 60)[:40]...),
			bytes.Repeat([]byte{0xde, 0xad}, 200)...),
		"empty file": {},
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			svc, dir := serviceForThumbs(t)
			writeBlob(t, dir, "k", body)

			if svc.writeThumbnail("k", "image/png") {
				t.Error("writeThumbnail = true for undecodable bytes, want false")
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("read dir: %v", err)
			}
			if len(entries) != 1 {
				names := make([]string, len(entries))
				for i, e := range entries {
					names[i] = e.Name()
				}
				t.Errorf("upload directory holds %v, want just the original blob", names)
			}
		})
	}
}

func TestWriteThumbnailReturnsFalseWhenTheBlobIsMissing(t *testing.T) {
	svc, _ := serviceForThumbs(t)
	if svc.writeThumbnail("nope", "image/png") {
		t.Error("writeThumbnail = true for a missing blob, want false")
	}
}

func TestRemoveThumbnailToleratesAMissingFile(t *testing.T) {
	// Delete calls this unconditionally rather than branching on
	// has_thumbnail, so the no-thumbnail case has to be a no-op.
	svc, _ := serviceForThumbs(t)
	if err := svc.removeThumbnail("nope"); err != nil {
		t.Errorf("removeThumbnail on a missing file = %v, want nil", err)
	}
}
