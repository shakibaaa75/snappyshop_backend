// Package imagesafe turns untrusted uploads into clean WebP images.
//
// Pipeline: size cap -> magic-byte sniff (JPEG/PNG/WebP only) -> header-only
// decode to enforce dimension/pixel limits BEFORE allocating pixels -> full
// decode under a timeout and a global concurrency cap -> EXIF orientation
// applied -> re-encode as WebP. Nothing from the original file (metadata,
// trailing bytes, embedded payloads) reaches the output.
package imagesafe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"golang.org/x/image/webp"
)

// Limits for one upload.
const (
	MaxBytes      = 5 << 20    // 5 MB input
	MaxSide       = 8192       // longest edge, pixels
	MaxPixels     = 16_000_000 // width*height (~64 MB decoded as RGBA)
	MaxOutBytes   = 12 << 20   // lossless WebP can be larger than the input
	ProcessTimout = 10 * time.Second
	MaxConcurrent = 4 // images decoded at once, process-wide
)

// Reject reasons. Messages are safe to show to the client.
var (
	ErrEmpty      = errors.New("file is empty")
	ErrTooLarge   = errors.New("file is too large (max 5 MB)")
	ErrType       = errors.New("only JPEG, PNG and WebP images are allowed")
	ErrCorrupt    = errors.New("image is corrupted or unreadable")
	ErrDimensions = errors.New("image dimensions are too large")
	ErrBusy       = errors.New("server is busy, try again shortly")
	ErrTimeout    = errors.New("image took too long to process")
)

var slots = make(chan struct{}, MaxConcurrent)

// Result is a sanitized image ready to store.
type Result struct {
	WebP          []byte
	Width, Height int
	InputFormat   string // jpeg | png | webp
}

// Sniff returns the real format from the file signature, or "".
// Declared MIME types and file names are never consulted.
func Sniff(b []byte) string {
	switch {
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "jpeg"
	case len(b) >= 8 && bytes.Equal(b[:8], []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp"
	}
	return ""
}

// ReadLimited reads at most MaxBytes; a longer stream is rejected without
// buffering the excess.
func ReadLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return nil, ErrEmpty
	}
	if len(b) > MaxBytes {
		return nil, ErrTooLarge
	}
	return b, nil
}

// Process validates and re-encodes data. It never returns the input bytes.
func Process(ctx context.Context, data []byte) (*Result, error) {
	if len(data) == 0 {
		return nil, ErrEmpty
	}
	if len(data) > MaxBytes {
		return nil, ErrTooLarge
	}
	format := Sniff(data)
	if format == "" {
		return nil, ErrType
	}
	// Header-only decode: gives dimensions without allocating the pixels, so
	// a tiny file that claims 60000x60000 is refused here (decompression bomb).
	var cfg image.Config
	var err error
	switch format {
	case "jpeg":
		cfg, err = jpeg.DecodeConfig(bytes.NewReader(data))
	case "png":
		cfg, err = png.DecodeConfig(bytes.NewReader(data))
	case "webp":
		cfg, err = webp.DecodeConfig(bytes.NewReader(data))
	}
	if err != nil {
		return nil, ErrCorrupt
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, ErrCorrupt
	}
	if cfg.Width > MaxSide || cfg.Height > MaxSide || int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return nil, ErrDimensions
	}

	select {
	case slots <- struct{}{}:
	case <-time.After(2 * time.Second):
		return nil, ErrBusy
	case <-ctx.Done():
		return nil, ErrTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, ProcessTimout)
	defer cancel()

	type out struct {
		res *Result
		err error
	}
	ch := make(chan out, 1)
	go func() {
		defer func() { <-slots }() // slot is held until the work really ends
		defer func() {
			if rec := recover(); rec != nil {
				ch <- out{nil, ErrCorrupt}
			}
		}()
		res, err := decodeEncode(data, format)
		ch <- out{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-ctx.Done():
		return nil, ErrTimeout
	}
}

func decodeEncode(data []byte, format string) (*Result, error) {
	var img image.Image
	var err error
	switch format {
	case "jpeg":
		img, err = jpeg.Decode(bytes.NewReader(data))
		if err == nil {
			img = orient(img, jpegOrientation(data))
		}
	case "png":
		img, err = png.Decode(bytes.NewReader(data))
	case "webp":
		img, err = webp.Decode(bytes.NewReader(data))
	}
	if err != nil || img == nil {
		return nil, ErrCorrupt
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || b.Dx() > MaxSide || b.Dy() > MaxSide || int64(b.Dx())*int64(b.Dy()) > MaxPixels {
		return nil, ErrDimensions
	}
	// Redraw into a fresh NRGBA: drops any non-pixel data the decoder kept.
	nrgba := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(nrgba, nrgba.Bounds(), img, b.Min, draw.Src)

	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, nrgba, nil); err != nil {
		return nil, fmt.Errorf("imagesafe: encode: %w", err)
	}
	if buf.Len() > MaxOutBytes {
		return nil, ErrTooLarge
	}
	return &Result{WebP: buf.Bytes(), Width: b.Dx(), Height: b.Dy(), InputFormat: format}, nil
}
