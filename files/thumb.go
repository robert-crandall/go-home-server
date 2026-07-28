package files

// Thumbnail generation. A photo grid that loads full-size originals costs tens
// of megabytes per screen on a phone, so every image we can decode gets a small
// JPEG written beside it at upload time.
//
// Deliberately narrow: one size, one output format, generated once, stored on
// disk. No queue, no cache, no on-the-fly resizing, no srcset. Generation never
// fails an upload - a file we can't decode is stored exactly as before and the
// UI falls back to the original.

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

// thumbSuffix names a thumbnail relative to its original.
//
// The two namespaces can't collide: an original storage key is 32 hex
// characters optionally followed by one dot and 1-8 characters of [a-z0-9]
// (see sanitizeExt), so it contains at most one dot. This suffix adds two, and
// is server-generated - a client filename never reaches it.
const thumbSuffix = ".thumb.jpg"

// thumbMaxEdge bounds the thumbnail's longest side. The grid is 2-3 columns, so
// a tile is roughly 150 CSS pixels; 512 covers that at 3x device pixel ratio.
const thumbMaxEdge = 512

// thumbQuality is the output JPEG quality. 80 is the usual point where further
// bytes stop buying visible detail at this size.
const thumbQuality = 80

// thumbContentType is what every generated thumbnail is served as.
const thumbContentType = "image/jpeg"

// maxThumbPixels caps the decoded image, which the upload limit does not: a
// 25 MiB PNG can declare 30000x30000 and decode to gigabytes, and this
// foundation supports open registration, so an authenticated hostile user is in
// scope. 50 MP clears a 48 MP phone photo with room to spare while bounding a
// decode at roughly 200 MB of RGBA. Over budget means no thumbnail, not a
// failed upload.
const maxThumbPixels = 50_000_000

// exifScanLimit bounds how far into a JPEG we look for the orientation tag. A
// single APP1 segment can't exceed 65533 bytes, and in a camera JPEG it sits
// within the first segment or two, so 128 KiB covers the real cases. It is a
// pragmatic cap, not a proof: a file could pad enough leading segments to push
// EXIF past it, and that file simply gets an unrotated thumbnail.
const exifScanLimit = 128 << 10

// thumbName returns the on-disk name of a storage key's thumbnail.
func thumbName(key string) string { return key + thumbSuffix }

// codec is the decoder pair for one image format. Formats are dispatched
// explicitly rather than through image.Decode with blank imports, because those
// register into a process-global table: an app that imported another decoder
// would silently change which uploads this package thumbnails.
type codec struct {
	config func(io.Reader) (image.Config, error)
	decode func(io.Reader) (image.Image, error)
	// exif marks formats that carry an EXIF orientation tag we honor.
	exif bool
}

// codecFor returns the decoder for a sniffed content type, if we have one.
// HEIC and AVIF are absent on purpose: decoding them needs cgo (libheif) or a
// wasm runtime, and the deployment target is a distroless image.
func codecFor(contentType string) (codec, bool) {
	switch baseMediaType(contentType) {
	case "image/jpeg":
		return codec{config: jpeg.DecodeConfig, decode: jpeg.Decode, exif: true}, true
	case "image/png":
		return codec{config: png.DecodeConfig, decode: png.Decode}, true
	case "image/gif":
		// gif.Decode returns the first frame, so an animated GIF gets a still
		// thumbnail. That's the right trade for a grid tile.
		return codec{config: gif.DecodeConfig, decode: gif.Decode}, true
	case "image/webp":
		return codec{config: webp.DecodeConfig, decode: webp.Decode}, true
	}
	return codec{}, false
}

// writeThumbnail generates a thumbnail for an already-stored blob and reports
// whether one was produced. Every failure - unsupported format, corrupt bytes,
// an image too large to decode safely, a write error - means "no thumbnail",
// never a failed upload.
func (s *Service) writeThumbnail(key, contentType string) bool {
	img, ok := s.renderThumbnail(key, contentType)
	if !ok {
		return false
	}

	tmpPath := filepath.Join(s.dir, tmpPrefix+thumbName(key))
	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return false
	}
	err = jpeg.Encode(tmp, img, &jpeg.Options{Quality: thumbQuality})
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmpPath)
		return false
	}
	if err := os.Rename(tmpPath, filepath.Join(s.dir, thumbName(key))); err != nil {
		_ = os.Remove(tmpPath)
		return false
	}
	return true
}

// renderThumbnail decodes a stored blob and returns the scaled, correctly
// oriented image. It reads from disk rather than from the upload stream so the
// original never has to be held in memory.
func (s *Service) renderThumbnail(key, contentType string) (image.Image, bool) {
	c, ok := codecFor(contentType)
	if !ok {
		return nil, false
	}

	src, err := os.Open(filepath.Join(s.dir, key))
	if err != nil {
		return nil, false
	}
	defer src.Close()

	// Read the header first: DecodeConfig gives the dimensions without
	// allocating the pixel buffer, which is the whole point of the budget.
	cfg, err := c.config(src)
	if err != nil || !withinPixelBudget(cfg.Width, cfg.Height) {
		return nil, false
	}

	orient := orientationNormal
	if c.exif {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return nil, false
		}
		head, err := io.ReadAll(io.LimitReader(src, exifScanLimit))
		if err != nil {
			return nil, false
		}
		orient = jpegOrientation(head)
	}

	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return nil, false
	}
	img, err := c.decode(src)
	if err != nil {
		return nil, false
	}

	// Scale before rotating: a quarter turn doesn't change the longest edge, so
	// the result is identical either way and the rotation runs over 512px
	// instead of the full image.
	return applyOrientation(scaleToFit(img, thumbMaxEdge), orient), true
}

// withinPixelBudget reports whether an image's declared dimensions are safe to
// decode. The comparison is a division so the product can't overflow.
func withinPixelBudget(w, h int) bool {
	if w <= 0 || h <= 0 {
		return false
	}
	return w <= maxThumbPixels/h
}

// scaleToFit downscales src so its longest edge is at most maxEdge, preserving
// aspect ratio. Images already within the bound are re-encoded at their own
// size rather than upscaled.
func scaleToFit(src image.Image, maxEdge int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	dw, dh := w, h
	if w > maxEdge || h > maxEdge {
		if w >= h {
			dw, dh = maxEdge, h*maxEdge/w
		} else {
			dw, dh = w*maxEdge/h, maxEdge
		}
	}
	// An extreme aspect ratio can round the short edge to zero.
	dw, dh = max(dw, 1), max(dh, 1)

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	// JPEG has no alpha channel, so a transparent PNG would otherwise encode as
	// black. Compositing over white matches how a browser renders it on a light
	// page and is the less surprising of the two wrong answers.
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, b, xdraw.Over, nil)
	return dst
}

// --- EXIF orientation ------------------------------------------------------
//
// Browsers rotate an image according to its EXIF orientation tag, but
// re-encoding drops that tag. Without this, a grid mixing thumbnails with
// fall-back originals would show some phone photos on their side.
//
// This reads exactly one tag out of a JPEG's first APP1 segment. It is not a
// general EXIF parser: anything unexpected - a truncated segment, a bogus
// offset, an unfamiliar tag type - returns "no orientation" rather than an
// error, and every read is bounds-checked because the bytes are attacker
// controlled.

const (
	orientationNormal = 1 // also the value used for "absent or unreadable"
	orientationMax    = 8
)

// jpegOrientation returns the EXIF orientation (1-8) from the leading bytes of
// a JPEG, or orientationNormal if there isn't a readable one.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return orientationNormal
	}
	for i := 2; i+4 <= len(b); {
		if b[i] != 0xFF {
			return orientationNormal // not aligned to a marker; don't guess
		}
		marker := b[i+1]
		switch {
		case marker == 0xFF:
			i++ // fill byte
			continue
		case marker == 0x01, marker >= 0xD0 && marker <= 0xD9:
			i += 2 // standalone marker, no length or payload
			continue
		case marker == 0xDA:
			return orientationNormal // start of scan: metadata is behind us
		}

		segLen := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if segLen < 2 {
			return orientationNormal
		}
		start, end := i+4, i+2+segLen
		if end > len(b) {
			return orientationNormal // truncated segment
		}
		if marker == 0xE1 {
			if o := exifOrientation(b[start:end]); o != orientationNormal {
				return o
			}
		}
		i = end
	}
	return orientationNormal
}

// exifOrientation reads the orientation tag out of an APP1 payload: the "Exif"
// identifier, then a TIFF header, then IFD0.
func exifOrientation(app1 []byte) int {
	const id = "Exif\x00\x00"
	const tiffHeaderLen = 8
	if len(app1) < len(id)+tiffHeaderLen || string(app1[:len(id)]) != id {
		return orientationNormal
	}
	tiff := app1[len(id):]

	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return orientationNormal
	}
	if bo.Uint16(tiff[2:4]) != 42 { // the TIFF magic number
		return orientationNormal
	}

	// Offsets are relative to the start of the TIFF header. Compare with
	// subtraction rather than addition so a hostile offset can't overflow.
	off := int64(bo.Uint32(tiff[4:8]))
	if off < tiffHeaderLen || off > int64(len(tiff))-2 {
		return orientationNormal
	}
	entries := tiff[off+2:]
	count := int(bo.Uint16(tiff[off : off+2]))
	// Clamp rather than bail. This is what keeps the loop's slicing in range,
	// and reading the entries that are physically present matches what the
	// browser's own forgiving EXIF parser will do with the same file. Bailing
	// would put the thumbnail's orientation at odds with the original's, which
	// is the exact mismatch reading EXIF here exists to prevent.
	if count > len(entries)/12 {
		count = len(entries) / 12
	}

	const (
		tagOrientation = 0x0112
		typeShort      = 3
	)
	for i := range count {
		e := entries[i*12 : i*12+12]
		if bo.Uint16(e[0:2]) != tagOrientation {
			continue
		}
		// A SHORT with count 1 is stored inline in the value field.
		if bo.Uint16(e[2:4]) != typeShort || bo.Uint32(e[4:8]) != 1 {
			return orientationNormal
		}
		v := int(bo.Uint16(e[8:10]))
		if v < orientationNormal || v > orientationMax {
			return orientationNormal
		}
		return v
	}
	return orientationNormal
}

// applyOrientation rewrites src into the upright image its EXIF orientation
// describes. Values 5-8 transpose the axes, so the result's dimensions swap.
func applyOrientation(src *image.RGBA, orientation int) *image.RGBA {
	if orientation <= orientationNormal || orientation > orientationMax {
		return src
	}

	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}

	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range dh {
		for x := range dw {
			sx, sy := sourcePixel(orientation, x, y, w, h)
			dst.SetRGBA(x, y, src.RGBAAt(b.Min.X+sx, b.Min.Y+sy))
		}
	}
	return dst
}

// sourcePixel maps a destination pixel back to its source pixel for one EXIF
// orientation. Written as an inverse map so each destination pixel is written
// exactly once.
func sourcePixel(orientation, x, y, w, h int) (int, int) {
	switch orientation {
	case 2: // mirrored horizontally
		return w - 1 - x, y
	case 3: // rotated 180
		return w - 1 - x, h - 1 - y
	case 4: // mirrored vertically
		return x, h - 1 - y
	case 5: // transposed across the main diagonal
		return y, x
	case 6: // rotated 90 clockwise
		return y, h - 1 - x
	case 7: // transposed across the anti-diagonal
		return w - 1 - y, h - 1 - x
	case 8: // rotated 90 counter-clockwise
		return w - 1 - y, x
	}
	return x, y
}

// removeThumbnail deletes a storage key's thumbnail if it has one. A file that
// never had a thumbnail is not an error, so callers don't need the flag.
func (s *Service) removeThumbnail(key string) error {
	if err := os.Remove(filepath.Join(s.dir, thumbName(key))); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("files: delete thumbnail: %w", err)
	}
	return nil
}
