package ocr

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeTool writes a file that isExecutableFile will accept, so the lookup can be
// tested without depending on what happens to be installed on the machine.
func fakeTool(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	return p
}

// TestLookToolFallback is the regression test for the failure that made OCR look
// broken on a machine where tesseract was correctly installed: the terminal had
// been open since before the installer ran, so PATH did not mention it and the
// app quietly fell back to manual entry. A tool that exists where its installer
// puts it must be found whatever PATH says.
func TestLookToolFallback(t *testing.T) {
	dir := t.TempDir()
	want := fakeTool(t, dir, "pretend-tesseract")

	// A name that is certainly not on PATH, so only the fallback can find it.
	got := lookTool("yaba-no-such-binary-anywhere", "YABA_TEST_UNSET", want)
	if got != want {
		t.Errorf("lookTool found %q, want the installed copy at %q", got, want)
	}

	// Nothing installed anywhere: still no crash, still empty.
	if got := lookTool("yaba-no-such-binary-anywhere", "YABA_TEST_UNSET",
		filepath.Join(dir, "not-here")); got != "" {
		t.Errorf("lookTool invented %q for a tool that is not installed", got)
	}

	// A directory is not a tool.
	if got := lookTool("yaba-no-such-binary-anywhere", "YABA_TEST_UNSET", dir); got != "" {
		t.Errorf("lookTool returned the directory %q as an executable", got)
	}
}

// TestLookToolEnvOverride covers the escape hatch, including the case that
// matters most: an override pointing at nothing must report the tool missing
// rather than silently searching on, so a typo is visible in the startup log.
func TestLookToolEnvOverride(t *testing.T) {
	dir := t.TempDir()
	real := fakeTool(t, dir, "elsewhere")
	other := fakeTool(t, dir, "fallback")

	t.Setenv("YABA_TEST_TOOL", real)
	if got := lookTool("yaba-no-such-binary-anywhere", "YABA_TEST_TOOL", other); got != real {
		t.Errorf("override ignored: got %q, want %q", got, real)
	}

	t.Setenv("YABA_TEST_TOOL", filepath.Join(dir, "typo"))
	if got := lookTool("yaba-no-such-binary-anywhere", "YABA_TEST_TOOL", other); got != "" {
		t.Errorf("a broken override fell through to %q; it should report the tool missing", got)
	}
}

// TestWindowsConvertIsNotImageMagick guards the check that stops Windows'
// FAT-to-NTFS convert.exe being mistaken for the image converter.
func TestWindowsConvertIsNotImageMagick(t *testing.T) {
	dir := t.TempDir()
	// A binary that exists but says nothing about ImageMagick when asked.
	notMagick := fakeTool(t, dir, "convert")
	if isImageMagick(notMagick) {
		t.Error("a binary that does not identify itself as ImageMagick was accepted")
	}
}

// TestTesseractPlacesAreAbsolute keeps the fallback list honest: a relative path
// would resolve against the working directory, which for a service is wherever
// systemd happened to start it.
func TestTesseractPlacesAreAbsolute(t *testing.T) {
	for _, list := range [][]string{tesseractPlaces(), pdftoppmPlaces(), convertPlaces("magick")} {
		for _, p := range list {
			if p == "" {
				continue // an unset LOCALAPPDATA yields an empty join; harmless
			}
			if !filepath.IsAbs(p) {
				t.Errorf("candidate %q is not an absolute path", p)
			}
		}
	}
}
