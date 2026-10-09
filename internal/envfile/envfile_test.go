package envfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSetsOnlyWhatIsNotAlreadySet(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "\ufeff# comment\n\nYABA_T_KEY=abc123\r\nexport YABA_T_FROM=\"YABA <y@example.com>\"\nYABA_T_EMPTY=\nYABA_T_KEEP=from-file\nYABA_T_SINGLE='x y'\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("YABA_T_KEEP", "from-environment")
	for _, k := range []string{"YABA_T_KEY", "YABA_T_FROM", "YABA_T_EMPTY", "YABA_T_SINGLE"} {
		os.Unsetenv(k)
		t.Cleanup(func() { os.Unsetenv(k) })
	}

	set, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(set) != 3 {
		t.Errorf("set %v, want 3 names", set)
	}
	for k, want := range map[string]string{
		"YABA_T_KEY":    "abc123", // CRLF line ending stripped
		"YABA_T_FROM":   "YABA <y@example.com>",
		"YABA_T_KEEP":   "from-environment", // the environment wins
		"YABA_T_SINGLE": "x y",
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if _, ok := os.LookupEnv("YABA_T_EMPTY"); ok {
		t.Error("an empty value was set; it should leave the variable unset")
	}
}

func TestLoadIgnoresAMissingFile(t *testing.T) {
	set, err := Load(filepath.Join(t.TempDir(), "nope.env"))
	if err != nil || set != nil {
		t.Errorf("Load(missing) = %v, %v", set, err)
	}
}

func TestLoadRejectsAMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("JUST SOME TEXT\n"), 0o600)
	if _, err := Load(path); err == nil {
		t.Error("a line without = was accepted")
	}
}
