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
		dir:      dir,
		maxBytes: maxBytes,
		entries:  make(map[cacheKey]*cacheEntry),
		lru:      list.New(),
	}
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
	}
	e := &cacheEntry{key: key, path: finalPath, bytes: n}
	e.elem = c.lru.PushFront(e)
	c.entries[key] = e
	c.totalBytes += n
	return finalPath, nil
}

// RangesFor returns the union of all cached time ranges for ratingKey,
// merged into the minimum number of contiguous intervals. Times are in
// seconds. Returned slice is sorted by start.
func (c *SegmentCache) RangesFor(ratingKey string) [][2]float64 {
	c.mu.Lock()
	type r struct{ s, e int64 }
	raw := make([]r, 0)
	for k := range c.entries {
		if k.ratingKey == ratingKey {
			raw = append(raw, r{k.startMs, k.endMs})
		}
	}
	c.mu.Unlock()
	if len(raw) == 0 {
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
