package imagesafe

import (
	"encoding/binary"
	"image"
)

// jpegOrientation reads the EXIF orientation (1-8) from the first APP1 segment.
// It only reads; the EXIF block itself is never copied to the output.
func jpegOrientation(d []byte) int {
	i := 2
	for i+4 <= len(d) {
		if d[i] != 0xFF {
			return 1
		}
		m := d[i+1]
		if m == 0xDA || m == 0xD9 { // start of scan / end: no more headers
			return 1
		}
		l := int(binary.BigEndian.Uint16(d[i+2 : i+4]))
		if l < 2 || i+2+l > len(d) {
			return 1
		}
		if m == 0xE1 && l >= 16 && string(d[i+4:i+10]) == "Exif\x00\x00" {
			return exifOrientation(d[i+10 : i+2+l])
		}
		i += 2 + l
	}
	return 1
}

func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 {
			if v := int(bo.Uint16(t[e+8 : e+10])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient applies an EXIF orientation so the image still looks upright once
// the metadata is stripped.
func orient(src image.Image, o int) image.Image {
	if o <= 1 || o > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	nw, nh := w, h
	if o >= 5 {
		nw, nh = h, w
	}
	dst := image.NewNRGBA(image.Rect(0, 0, nw, nh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var dx, dy int
			switch o {
			case 2:
				dx, dy = w-1-x, y
			case 3:
				dx, dy = w-1-x, h-1-y
			case 4:
				dx, dy = x, h-1-y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = h-1-y, x
			case 7:
				dx, dy = h-1-y, w-1-x
			case 8:
				dx, dy = y, w-1-x
			}
			dst.Set(dx, dy, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
