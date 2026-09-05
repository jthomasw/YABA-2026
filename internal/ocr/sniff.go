package ocr

import (
	"bytes"
	"fmt"
	"os"
)

// Kind is a receipt file format, named by its MIME type.
//
// This exists because http.DetectContentType cannot recognise HEIC, the format
// every recent iPhone photographs in by default. It reports HEIC as
// application/octet-stream, so an allowlist built on it silently rejects the
// single most likely thing a user will try to upload.
type Kind string

const (
	KindJPEG    Kind = "image/jpeg"
	KindPNG     Kind = "image/png"
	KindGIF     Kind = "image/gif"
	KindWebP    Kind = "image/webp"
	KindHEIC    Kind = "image/heic"
	KindAVIF    Kind = "image/avif"
	KindPDF     Kind = "application/pdf"
	KindUnknown Kind = ""
)

// Ext is the extension a file of this kind is stored with.
func (k Kind) Ext() string {
	switch k {
	case KindJPEG:
		return ".jpg"
	case KindPNG:
		return ".png"
	case KindGIF:
		return ".gif"
	case KindWebP:
		return ".webp"
	case KindHEIC:
		return ".heic"
	case KindAVIF:
		return ".avif"
	case KindPDF:
		return ".pdf"
	}
	return ""
}

// NativelyDecodable reports whether Go's own image package can read this format,
// which decides whether an external converter is needed before OCR.
func (k Kind) NativelyDecodable() bool {
	switch k {
	case KindJPEG, KindPNG, KindGIF:
		return true
	}
	return false
}

// Describe names the format for a message shown to the user.
func (k Kind) Describe() string {
	switch k {
	case KindHEIC:
		return "HEIC"
	case KindAVIF:
		return "AVIF"
	case KindWebP:
		return "WebP"
	case KindPDF:
		return "PDF"
	case KindJPEG:
		return "JPEG"
	case KindPNG:
		return "PNG"
	case KindGIF:
		return "GIF"
	}
	return "that file type"
}

// heicBrands are the ISO base media file format brands that mean a still image
// this application should accept. The brand sits at bytes 8..12, immediately
// after the "ftyp" box header.
var heicBrands = map[string]Kind{
	"heic": KindHEIC, "heix": KindHEIC, "heim": KindHEIC, "heis": KindHEIC,
	"hevc": KindHEIC, "hevx": KindHEIC, "hevm": KindHEIC, "hevs": KindHEIC,
	"mif1": KindHEIC, "msf1": KindHEIC,
	"avif": KindAVIF, "avis": KindAVIF,
}

// Sniff identifies a file from its leading bytes. 32 bytes is enough for every
// format here; pass more and the extra is ignored.
func Sniff(head []byte) Kind {
	switch {
	case len(head) >= 3 && bytes.Equal(head[:3], []byte{0xFF, 0xD8, 0xFF}):
		return KindJPEG

	case len(head) >= 8 && bytes.Equal(head[:8], []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return KindPNG

	case len(head) >= 6 && (bytes.Equal(head[:6], []byte("GIF87a")) || bytes.Equal(head[:6], []byte("GIF89a"))):
		return KindGIF

	case len(head) >= 5 && bytes.Equal(head[:5], []byte("%PDF-")):
		return KindPDF

	// RIFF....WEBP
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return KindWebP

	// ISO base media: a four-byte big-endian box size, then "ftyp", then the
	// brand. The size is not checked, because a truncated upload should still
	// be identified so the user is told what it was rather than that it was
	// unrecognisable.
	case len(head) >= 12 && bytes.Equal(head[4:8], []byte("ftyp")):
		if k, ok := heicBrands[string(head[8:12])]; ok {
			return k
		}
		return KindUnknown
	}
	return KindUnknown
}

// sniffFile reads a file's header and identifies it.
func sniffFile(path string) (Kind, error) {
	f, err := os.Open(path)
	if err != nil {
		return KindUnknown, fmt.Errorf("open receipt: %w", err)
	}
	defer f.Close()

	head := make([]byte, 32)
	n, err := f.Read(head)
	if err != nil && n == 0 {
		return KindUnknown, fmt.Errorf("read receipt: %w", err)
	}
	return Sniff(head[:n]), nil
}
