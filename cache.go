package main

import (
	"container/list"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
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
