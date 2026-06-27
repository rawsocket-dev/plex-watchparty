package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxPosterBytes caps a single poster we'll buffer/cache. Plex thumbs are
// well under this; the limit just stops a misbehaving upstream from filling
// memory or disk.
const maxPosterBytes = 10 << 20 // 10 MiB

// PosterCache serves Plex poster art backed by an on-disk cache so repeated
// requests — and restarts — don't re-fetch the same artwork from Plex. Each
// poster is stored once per ratingKey and reused until it ages past ttl, at
// which point the next request refetches and rewrites it.
//
// No size cap or eviction sweep: posters are small and the working set is
// naturally bounded by the library size (a few hundred titles × ~100 KB), so
// the directory stays tiny on its own. Stale files for removed titles simply
// age out by ttl.
type PosterCache struct {
	plex *Plex
	dir  string
	ttl  time.Duration // <= 0 means cached posters never expire
}

func NewPosterCache(p *Plex, dir string, ttl time.Duration) *PosterCache {
	return &PosterCache{plex: p, dir: dir, ttl: ttl}
}

func (c *PosterCache) path(ratingKey string) string {
	return filepath.Join(c.dir, ratingKey+".jpg")
}

// Stream returns the poster bytes for a (pre-validated) ratingKey along with
// a content type. It serves a fresh-enough file from disk when present;
// otherwise it fetches from Plex, writes the bytes to disk (best-effort), and
// returns them. Returns errNoPoster when the title has no art — that result
// is never cached, so art added later shows up on the next request.
func (c *PosterCache) Stream(ratingKey string) (io.ReadCloser, string, error) {
	fpath := c.path(ratingKey)
	if data, ct, ok := c.readDisk(fpath); ok {
		return io.NopCloser(bytes.NewReader(data)), ct, nil
	}

	body, ct, err := c.plex.PosterStream(ratingKey)
	if err != nil {
		return nil, "", err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, maxPosterBytes))
	if err != nil {
		return nil, "", err
	}
	c.writeDisk(fpath, data) // best-effort; a write failure just means no caching
	return io.NopCloser(bytes.NewReader(data)), ct, nil
}

// readDisk returns the cached bytes for fpath when the file exists, is
// non-empty, and is within ttl. The content type is sniffed from the bytes so
// we don't need a sidecar; non-image sniffs fall back to image/jpeg.
func (c *PosterCache) readDisk(fpath string) (data []byte, ct string, ok bool) {
	fi, err := os.Stat(fpath)
	if err != nil {
		return nil, "", false
	}
	if c.ttl > 0 && time.Since(fi.ModTime()) >= c.ttl {
		return nil, "", false
	}
	data, err = os.ReadFile(fpath)
	if err != nil || len(data) == 0 {
		return nil, "", false
	}
	ct = http.DetectContentType(data)
	if !strings.HasPrefix(ct, "image/") {
		ct = "image/jpeg"
	}
	return data, ct, true
}

// writeDisk atomically stores data at fpath (temp file + rename) so a reader
// never sees a half-written poster. Errors are logged, not returned: caching
// is an optimization, and the caller still has the freshly-fetched bytes.
func (c *PosterCache) writeDisk(fpath string, data []byte) {
	if err := os.MkdirAll(filepath.Dir(fpath), 0o755); err != nil {
		log.Printf("poster: cache mkdir: %v", err)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(fpath), ".poster-*")
	if err != nil {
		log.Printf("poster: cache temp: %v", err)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("poster: cache write: %v", err)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		log.Printf("poster: cache close: %v", err)
		return
	}
	if err := os.Rename(tmpName, fpath); err != nil {
		os.Remove(tmpName)
		log.Printf("poster: cache rename: %v", err)
	}
}

// posterHandler serves Plex poster art at /poster/<ratingKey>.jpg through the
// on-disk PosterCache. It is mounted UNAUTHENTICATED so Discord's servers
// (which fetch embed images from the public internet) can render the
// thumbnail. Safe to expose: the rating key is validated as a bounded
// [A-Za-z0-9] token before any Plex call or filesystem access, the response
// is image bytes only, and the Plex token never appears in it.
func posterHandler(c *PosterCache) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/poster/"), ".jpg")
		if !validRatingKey(key) {
			http.Error(w, "invalid ratingKey", http.StatusBadRequest)
			return
		}
		body, ct, err := c.Stream(key)
		if err != nil {
			if err != errNoPoster {
				log.Printf("poster: %s: %v", key, err)
			}
			http.NotFound(w, r)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if _, err := io.Copy(w, body); err != nil {
			log.Printf("poster: copy %s: %v", key, err)
		}
	}
}
