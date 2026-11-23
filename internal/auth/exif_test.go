package auth

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func buildSeedTIFF(orientation uint16, bigEndian bool) []byte {
	var order binary.ByteOrder = binary.LittleEndian
	tag := "II"
	if bigEndian {
		order = binary.BigEndian
		tag = "MM"
	}
	var buf bytes.Buffer
	buf.WriteString(tag)
	_ = binary.Write(&buf, order, uint16(42))
	_ = binary.Write(&buf, order, uint32(8))
	_ = binary.Write(&buf, order, uint16(1))
	_ = binary.Write(&buf, order, uint16(0x0112))
	_ = binary.Write(&buf, order, uint16(3))
	_ = binary.Write(&buf, order, uint32(1))
	_ = binary.Write(&buf, order, orientation)
	_ = binary.Write(&buf, order, uint16(0))
	_ = binary.Write(&buf, order, uint32(0))
	return buf.Bytes()
}

func buildSeedJPEGEXIF(orientation uint16, bigEndian bool) []byte {
	payload := append([]byte("Exif\x00\x00"), buildSeedTIFF(orientation, bigEndian)...)
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1})
	_ = binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write([]byte{0xFF, 0xD9})
	return out.Bytes()
}

func FuzzParseOrientation(f *testing.F) {
	tiffII := buildSeedTIFF(6, false)
	tiffMM := buildSeedTIFF(8, true)
	jpegII := buildSeedJPEGEXIF(3, false)
	jpegMM := buildSeedJPEGEXIF(6, true)

	seeds := [][]byte{
		{},
		{0xFF},
		{0xFF, 0xD8},
		{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x04},
		tiffII,
		tiffMM,
		tiffII[:len(tiffII)/2],
		tiffMM[:10],
		jpegII,
		jpegMM,
		jpegII[:len(jpegII)/2],
		jpegMM[:14],
		// Huge IFD0 offset (0xFFFFFFFF) to exercise overflow guards.
		{'I', 'I', 42, 0, 0xFF, 0xFF, 0xFF, 0xFF, 0, 0},
		{'M', 'M', 0, 42, 0x7F, 0xFF, 0xFF, 0xFF, 0, 0},
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, b []byte) {
		if got := parseTIFFOrientation(b); got < 1 || got > 8 {
			t.Fatalf("parseTIFFOrientation returned out-of-range value %d", got)
		}
		if got := readJPEGEXIFOrientation(bytes.NewReader(b)); got < 1 || got > 8 {
			t.Fatalf("readJPEGEXIFOrientation returned out-of-range value %d", got)
		}
	})
}
