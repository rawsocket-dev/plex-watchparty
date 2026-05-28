package main

import (
	"container/list"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SegmentCache is a disk-backed cache for HLS segment files, indexed by
// (ratingKey, startMs, endMs). Segments persist across Plex transcoder
// sessions so that backward seek into a previously-watched range is
// instant — even if Plex has since restarted at a different offset.
type SegmentCache struct {
	dir      string
	maxBytes int64

	mu         sync.Mutex
	entries    map[cacheKey]*cacheEntry
	lru        *list.List // front = most recent
	totalBytes int64
	// rangesCache memoizes RangesFor results, keyed by ratingKey. The
	// broadcast loop calls RangesFor every 3s; over a 90-minute movie
	// the cache holds thousands of segments and the linear scan + sort
	// is non-trivial. Invalidated on every Put / evict / LoadFromDisk.
	rangesCache map[string][][2]float64
}

type cacheKey struct {
	ratingKey string
	startMs   int64
	endMs     int64
}

type cacheEntry struct {
	key   cacheKey
	path  string
	bytes int64
	elem  *list.Element
}

func NewSegmentCache(dir string, maxBytes int64) *SegmentCache {
	return &SegmentCache{
		dir:         dir,
		maxBytes:    maxBytes,
		entries:     make(map[cacheKey]*cacheEntry),
		lru:         list.New(),
		rangesCache: make(map[string][][2]float64),
	}
}

// invalidateRangesLocked drops the memoized RangesFor result for one
// movie. Called whenever the set of cached segments for that movie
// changes (Put, eviction). Must be called with c.mu held.
func (c *SegmentCache) invalidateRangesLocked(ratingKey string) {
	delete(c.rangesCache, ratingKey)
}

func (c *SegmentCache) Get(key cacheKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return "", false
	}
	c.lru.MoveToFront(e.elem)
	return e.path, true
}

// FindOverlapping returns the path of a cached segment whose time
// range overlaps the requested window for ratingKey, plus its
// (startMs, endMs) so the caller can log what was served. Used as a
// fallback when Plex 404s a segment that's been transcoded but
// evicted from its in-session cache, or when Plex's segment
// boundaries drift across sessions and an exact (startMs, endMs)
// match fails. Linear scan — N is bounded by cache size.
func (c *SegmentCache) FindOverlapping(ratingKey string, startMs, endMs int64) (path string, foundStart, foundEnd int64, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Pick the entry with the largest overlap with [startMs, endMs].
	// In practice the cache holds at most one segment per movie second
	// so the "best" overlap is usually unambiguous, but we tie-break
	// on overlap size in case sessions produced subtly different
	// boundaries that bracket the requested window.
	var bestOverlap int64
	for k, e := range c.entries {
		if k.ratingKey != ratingKey {
			continue
		}
		if k.endMs < startMs || k.startMs > endMs {
			continue
		}
		lo := startMs
		if k.startMs > lo {
			lo = k.startMs
		}
		hi := endMs
		if k.endMs < hi {
			hi = k.endMs
		}
		ov := hi - lo
		if ov > bestOverlap {
			bestOverlap = ov
			path = e.path
			foundStart = k.startMs
			foundEnd = k.endMs
			ok = true
		}
	}
	return
}

// Put streams src to a temp file, atomically renames to the cache path,
// and adds the entry to the index. Partial writes that don't reach the
// rename never appear in the cache.
func (c *SegmentCache) Put(key cacheKey, src io.Reader) (string, error) {
	movieDir := filepath.Join(c.dir, key.ratingKey)
	if err := os.MkdirAll(movieDir, 0o755); err != nil {
		return "", err
	}
	finalPath := filepath.Join(movieDir, fmt.Sprintf("seg_%d_%d.ts", key.startMs, key.endMs))
	tmpPath := finalPath + ".tmp"

	f, err := os.Create(tmpPath)
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(f, src)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmpPath)
		return "", copyErr
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return "", closeErr
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		os.Remove(tmpPath)
		return "", err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if old, ok := c.entries[key]; ok {
		c.lru.Remove(old.elem)
		c.totalBytes -= old.bytes
		delete(c.entries, key)
	}
	// Evict LRU entries until there's headroom for the new one.
	for c.totalBytes+n > c.maxBytes && c.lru.Len() > 0 {
		oldest := c.lru.Back()
		oe := oldest.Value.(*cacheEntry)
		os.Remove(oe.path)
		c.lru.Remove(oldest)
		delete(c.entries, oe.key)
		c.totalBytes -= oe.bytes
		c.invalidateRangesLocked(oe.key.ratingKey)
	}
	e := &cacheEntry{key: key, path: finalPath, bytes: n}
	e.elem = c.lru.PushFront(e)
	c.entries[key] = e
	c.totalBytes += n
	c.invalidateRangesLocked(key.ratingKey)
	return finalPath, nil
}

// RangesFor returns the union of all cached time ranges for ratingKey,
// merged into the minimum number of contiguous intervals. Times are in
// seconds. Returned slice is sorted by start. Memoized — the
// broadcast loop hits this every 3s, and on a large library cache
// the underlying sort+merge isn't free.
func (c *SegmentCache) RangesFor(ratingKey string) [][2]float64 {
	c.mu.Lock()
	if cached, ok := c.rangesCache[ratingKey]; ok {
		c.mu.Unlock()
		return cached
	}
	type r struct{ s, e int64 }
	raw := make([]r, 0)
	for k := range c.entries {
		if k.ratingKey == ratingKey {
			raw = append(raw, r{k.startMs, k.endMs})
		}
	}
	if len(raw) == 0 {
		c.rangesCache[ratingKey] = nil
		c.mu.Unlock()
		return nil
	}
	sort.Slice(raw, func(i, j int) bool { return raw[i].s < raw[j].s })
	merged := raw[:1]
	for _, x := range raw[1:] {
		last := &merged[len(merged)-1]
		if x.s <= last.e {
			if x.e > last.e {
				last.e = x.e
			}
			continue
		}
		merged = append(merged, x)
	}
	out := make([][2]float64, len(merged))
	for i, m := range merged {
		out[i] = [2]float64{float64(m.s) / 1000.0, float64(m.e) / 1000.0}
	}
	c.rangesCache[ratingKey] = out
	c.mu.Unlock()
	return out
}

// LoadFromDisk walks the cache directory and rebuilds the in-memory
// index from existing files. Stale .tmp files are cleaned up. Garbage
// filenames are skipped (we don't delete them — could be user data
// the cache shares a dir with).
func (c *SegmentCache) LoadFromDisk() error {
	movieDirs, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, md := range movieDirs {
		if !md.IsDir() {
			continue
		}
		ratingKey := md.Name()
		movieDir := filepath.Join(c.dir, ratingKey)
		entries, err := os.ReadDir(movieDir)
		if err != nil {
			continue
		}
		for _, ent := range entries {
			name := ent.Name()
			full := filepath.Join(movieDir, name)
			if strings.HasSuffix(name, ".tmp") {
				_ = os.Remove(full) // stale write
				continue
			}
			startMs, endMs, ok := parseCacheName(name)
			if !ok {
				continue
			}
			info, err := ent.Info()
			if err != nil {
				continue
			}
			key := cacheKey{ratingKey: ratingKey, startMs: startMs, endMs: endMs}
			e := &cacheEntry{key: key, path: full, bytes: info.Size()}
			e.elem = c.lru.PushBack(e) // back = oldest; LoadFromDisk creates LRU as old
			c.entries[key] = e
			c.totalBytes += e.bytes
			c.invalidateRangesLocked(ratingKey)
		}
	}
	return nil
}

// parseCacheName extracts (startMs, endMs) from "seg_<startMs>_<endMs>.ts".
// Returns ok=false for any filename that doesn't match.
func parseCacheName(name string) (int64, int64, bool) {
	if !strings.HasPrefix(name, "seg_") || !strings.HasSuffix(name, ".ts") {
		return 0, 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, "seg_"), ".ts")
	parts := strings.Split(mid, "_")
	if len(parts) != 2 {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(parts[0], 10, 64)
	e, err2 := strconv.ParseInt(parts[1], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return s, e, true
}

// CacheMovieStat is one movie's contribution to the cache, returned
// by Stats() for display in the admin panel.
type CacheMovieStat struct {
	RatingKey string  `json:"ratingKey"`
	Entries   int     `json:"entries"`
	Bytes     int64   `json:"bytes"`
	OldestAge float64 `json:"oldestAgeSec"` // seconds since the oldest file's mtime
	NewestAge float64 `json:"newestAgeSec"` // seconds since the most-recent file's mtime
}

// CacheStats is the snapshot of cache state returned to the admin
// panel. PerMovie is sorted by Bytes descending so the heaviest
// titles surface first.
type CacheStats struct {
	Entries    int              `json:"entries"`
	TotalBytes int64            `json:"totalBytes"`
	MaxBytes   int64            `json:"maxBytes"`
	PerMovie   []CacheMovieStat `json:"perMovie"`
}

// Stats returns a per-movie aggregate snapshot of the cache. Walks
// the index under c.mu and stats each file once to capture ages.
func (c *SegmentCache) Stats() CacheStats {
	c.mu.Lock()
	entries := make(map[cacheKey]*cacheEntry, len(c.entries))
	for k, v := range c.entries {
		entries[k] = v
	}
	total := c.totalBytes
	maxBytes := c.maxBytes
	c.mu.Unlock()

	now := time.Now()
	per := make(map[string]*CacheMovieStat)
	for k, e := range entries {
		st, ok := per[k.ratingKey]
		if !ok {
			st = &CacheMovieStat{RatingKey: k.ratingKey}
			per[k.ratingKey] = st
		}
		st.Entries++
		st.Bytes += e.bytes
		// File-mtime drives age. Skip silently on stat failure — a
		// missing file gets eviction on the next touch.
		if info, err := os.Stat(e.path); err == nil {
			age := now.Sub(info.ModTime()).Seconds()
			if age > st.OldestAge {
				st.OldestAge = age
			}
			if st.NewestAge == 0 || age < st.NewestAge {
				st.NewestAge = age
			}
		}
	}
	out := make([]CacheMovieStat, 0, len(per))
	for _, st := range per {
		out = append(out, *st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return CacheStats{
		Entries:    len(entries),
		TotalBytes: total,
		MaxBytes:   maxBytes,
		PerMovie:   out,
	}
}

// Clear removes every cached segment from memory and disk. Returns
// the entry count + byte total removed so the admin panel can log /
// confirm what was wiped. Safe to call while transcoding — Plex will
// just refill the cache as segments are re-requested.
func (c *SegmentCache) Clear() (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entries = len(c.entries)
	bytes = c.totalBytes
	for _, e := range c.entries {
		_ = os.Remove(e.path)
	}
	c.entries = make(map[cacheKey]*cacheEntry)
	c.lru = list.New()
	c.totalBytes = 0
	c.rangesCache = make(map[string][][2]float64)
	return entries, bytes
}

// ClearMovie removes every cached segment belonging to one movie.
// Returns the count + bytes removed. The movie directory itself is
// also rm'd if it's now empty.
func (c *SegmentCache) ClearMovie(ratingKey string) (entries int, bytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, e := range c.entries {
		if k.ratingKey != ratingKey {
			continue
		}
		_ = os.Remove(e.path)
		c.lru.Remove(e.elem)
		delete(c.entries, k)
		c.totalBytes -= e.bytes
		entries++
		bytes += e.bytes
	}
	c.invalidateRangesLocked(ratingKey)
	if entries > 0 {
		_ = os.Remove(filepath.Join(c.dir, ratingKey)) // best-effort, only succeeds if empty
	}
	return entries, bytes
}

// Prune removes cache entries whose file mtime is older than the
// given duration. Useful for "clear anything older than 30 days" style
// maintenance. The LRU usually keeps things fresh enough on its own,
// but an explicit prune is the right tool when the cache hasn't hit
// the size cap yet still has stale junk.
func (c *SegmentCache) Prune(olderThan time.Duration) (entries int, bytes int64) {
	cutoff := time.Now().Add(-olderThan)
	c.mu.Lock()
	defer c.mu.Unlock()
	affectedMovies := make(map[string]struct{})
	for k, e := range c.entries {
		info, err := os.Stat(e.path)
		if err != nil {
			// Missing file — drop the index entry, no eviction count
			// because the bytes were never recoverable.
			c.lru.Remove(e.elem)
			delete(c.entries, k)
			c.totalBytes -= e.bytes
			affectedMovies[k.ratingKey] = struct{}{}
			continue
		}
		if !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(e.path)
		c.lru.Remove(e.elem)
		delete(c.entries, k)
		c.totalBytes -= e.bytes
		entries++
		bytes += e.bytes
		affectedMovies[k.ratingKey] = struct{}{}
	}
	for rk := range affectedMovies {
		c.invalidateRangesLocked(rk)
	}
	return entries, bytes
}
