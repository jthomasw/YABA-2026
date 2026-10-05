// Package envfile loads settings from a .env file into the process
// environment, so `cp .env.example .env`, filling it in and starting the
// server is all it takes -- on Windows and in a terminal as well as under
// systemd or Docker, which read the file themselves.
//
// Before this existed .env.example said "copy to .env", but nothing read the
// file: a key written there was silently ignored, and receipt reading, mail and
// the session key all fell back to their unconfigured defaults.
package envfile

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// DefaultPath is where Load looks when YABA_ENV_FILE does not say otherwise:
// .env in the directory the program was started from.
const DefaultPath = ".env"

// Path is the file to load: YABA_ENV_FILE if set, else DefaultPath.
func Path() string {
	if p := strings.TrimSpace(os.Getenv("YABA_ENV_FILE")); p != "" {
		return p
	}
	return DefaultPath
}

// Load reads KEY=VALUE lines from path and sets each variable that is not
// already set in the environment, returning the names it set. A real
// environment variable always wins, so a systemd unit or a one-off
// `YABA_ADDR=:9000 yaba-server` still overrides the file.
//
// A missing file is not an error: the file is optional. Blank lines and lines
// starting with # are skipped, an optional leading "export " is allowed, and a
// value wrapped in matching single or double quotes has them removed. Values
// are never logged by this package or its callers.
func Load(path string) ([]string, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// Notepad on Windows can save as UTF-16, which reads as garbage here.
	// Say so, rather than reporting a confusing parse error on line 1.
	br := bufio.NewReader(f)
	if head, _ := br.Peek(2); len(head) == 2 &&
		((head[0] == 0xFF && head[1] == 0xFE) || (head[0] == 0xFE && head[1] == 0xFF)) {
		return nil, fmt.Errorf("%s is saved as UTF-16; save it as UTF-8 (in Notepad: Save As, Encoding: UTF-8)", path)
	}

	var set []string
	sc := bufio.NewScanner(br)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "\ufeff"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t") {
			return set, fmt.Errorf("%s line %d: expected KEY=VALUE", path, n)
		}
		value = unquote(strings.TrimSpace(value))
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if value == "" {
			// "YABA_GEMINI_KEY=" in a copied example means "not set", not
			// "set to nothing"; leaving it unset keeps every default intact.
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return set, fmt.Errorf("%s line %d: %w", path, n, err)
		}
		set = append(set, key)
	}
	return set, sc.Err()
}

// unquote removes one pair of matching surrounding quotes.
func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return v
}
