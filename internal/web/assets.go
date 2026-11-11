package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// deriveKey produces a 32-byte subkey from the session secret, separated by
// purpose so two uses of the secret can never be the same key.
func deriveKey(secret []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(purpose))
	return mac.Sum(nil)
}

// assetFingerprints maps a static filename to a short hash of its contents.
type assetFingerprints map[string]string

// buildFingerprints hashes every embedded static file once, at startup.
func buildFingerprints(fsys fs.FS) (assetFingerprints, error) {
	out := assetFingerprints{}

	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read asset %s: %w", p, err)
		}
		sum := sha256.Sum256(b)
		// Eight hex characters is 32 bits: ample to notice a change, and short
		// enough to keep the URL readable in devtools.
		out[p] = hex.EncodeToString(sum[:])[:8]
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// url returns the cache-busted path for one asset.
func (a assetFingerprints) url(name string) string {
	name = strings.TrimPrefix(name, "/")
	name = path.Clean(name)
	if v, ok := a[name]; ok {
		return "/static/" + name + "?v=" + v
	}
	return "/static/" + name
}

// staticHandler serves the embedded assets with a long cache lifetime, which is safe
// because the URL carries the content hash: a changed file is a different URL.
func staticHandler(fsys fs.FS) http.Handler {
	files := http.FileServer(http.FS(fsys))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("v") != "" {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// Reached without a fingerprint -- a hand-typed URL, or a stale page still
			// referencing the old form.
			w.Header().Set("Cache-Control", "no-cache")
		}
		files.ServeHTTP(w, r)
	})
}
