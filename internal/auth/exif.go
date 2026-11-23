package auth

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"image"
	"io"
)

const maxEXIFScanBytes = 128 << 10 // 128 KiB

// readJPEGEXIFOrientation scans up to the first JPEG APP1 segment (or 128 KiB,
// whichever comes first) for the TIFF IFD0 Orientation tag (0x0112). Returns 1
// (normal) if absent or unreadable.
func readJPEGEXIFOrientation(r io.Reader) int {
	br := bufio.NewReader(io.LimitReader(r, maxEXIFScanBytes))

	var soi [2]byte
	if _, err := io.ReadFull(br, soi[:]); err != nil || soi[0] != 0xFF || soi[1] != 0xD8 {
		return 1
	}

	for {
		// Find marker prefix 0xFF.
		for {
			b, err := br.ReadByte()
			if err != nil {
				return 1
			}
			if b == 0xFF {
				break
			}
		}
		// Skip any fill 0xFF bytes.
		var marker byte
		for {
			b, err := br.ReadByte()
			if err != nil {
				return 1
			}
			if b != 0xFF {
				marker = b
				break
			}
		}

		// Stop at EOI or Start of Scan.
		if marker == 0xD9 || marker == 0xDA {
			return 1
		}
		// Standalone markers without payload.
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			continue
		}

		var lenBuf [2]byte
		if _, err := io.ReadFull(br, lenBuf[:]); err != nil {
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(lenBuf[:]))
		if segLen < 2 {
			return 1
		}
		payloadLen := segLen - 2

		if marker != 0xE1 {
			if _, err := io.CopyN(io.Discard, br, int64(payloadLen)); err != nil {
				return 1
			}
			continue
		}

		// Stop scanning after the first APP1 segment.
		payload := make([]byte, payloadLen)
		if _, err := io.ReadFull(br, payload); err != nil {
			return 1
		}
		if !bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
			return 1
		}
		return parseTIFFOrientation(payload[6:])
	}
}

func parseTIFFOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[0:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:4]) != 42 {
		return 1
	}

	ifd0 := int64(order.Uint32(tiff[4:8]))
	if ifd0 < 8 || ifd0 > int64(len(tiff))-2 {
		return 1
	}
	numEntries := int(order.Uint16(tiff[ifd0 : ifd0+2]))
	for i := 0; i < numEntries; i++ {
		off := ifd0 + 2 + int64(i)*12
		if off > int64(len(tiff))-12 {
			break
		}
		tag := order.Uint16(tiff[off : off+2])
		if tag != 0x0112 {
			continue
		}
		typ := order.Uint16(tiff[off+2 : off+4])
		count := order.Uint32(tiff[off+4 : off+8])
		if typ == 3 && count == 1 {
			val := int(order.Uint16(tiff[off+8 : off+10]))
			if val >= 1 && val <= 8 {
				return val
			}
		}
		return 1
	}
	return 1
}

// applyOrientation transforms src according to an EXIF Orientation tag (1..8)
// so the returned image is upright.
func applyOrientation(src image.Image, orientation int) image.Image {
	if orientation <= 1 || orientation > 8 {
		return src
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()

	var dst *image.RGBA
	if orientation >= 5 {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	}

	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := src.At(b.Min.X+x, b.Min.Y+y)
			switch orientation {
			case 2: // Flip horizontal
				dst.Set(w-1-x, y, c)
			case 3: // Rotate 180
				dst.Set(w-1-x, h-1-y, c)
			case 4: // Flip vertical
				dst.Set(x, h-1-y, c)
			case 5: // Transpose
				dst.Set(y, x, c)
			case 6: // Rotate 90 CW
				dst.Set(h-1-y, x, c)
			case 7: // Transverse
				dst.Set(h-1-y, w-1-x, c)
			case 8: // Rotate 270 CW (90 CCW)
				dst.Set(y, w-1-x, c)
			}
		}
	}
	return dst
}
