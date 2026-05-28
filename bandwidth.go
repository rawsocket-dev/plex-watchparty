package main

import (
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// bwTracker counts /hls/* bytes-served per client over a rolling window so
// the player can show "you X kbps / room Y kbps / N viewers". Clients are
// keyed by best-effort IP (X-Forwarded-For / X-Real-IP / RemoteAddr).
//
// bwTracker also keeps a per-second total-kbps ring buffer for the admin
// panel's bandwidth sparkline. Sized to historyBuckets seconds (default
// 120 = a 2-minute window) — the ring index is now/sec % len so writes
// are O(1).
type bwTracker struct {
	mu      sync.Mutex
	clients map[string][]bwSample
	window  time.Duration

	// History ring. history[i] holds the total bytes served at second
	// timestamps[i]; total kbps for the second is history[i]*8/1000.
	// We compute the second-aggregate lazily at snapshot/history read
	// time so record() stays cheap.
	history     []int64
	timestamps  []int64 // unix-seconds; aligned with history index
	historyHead int     // most-recently-written slot (clockwise)
}

type bwSample struct {
	at    time.Time
	bytes int64
}

const historyBuckets = 120 // 2 minutes at 1-second resolution

func newBwTracker() *bwTracker {
	return &bwTracker{
		clients:    make(map[string][]bwSample),
		window:     10 * time.Second,
		history:    make([]int64, historyBuckets),
		timestamps: make([]int64, historyBuckets),
	}
}

// record appends a sample for ip. Empty IPs and zero-byte writes are
// dropped. Samples older than the window are pruned lazily — only when
// the slice grows past a soft cap — so the common case is a tiny O(1)
// append. snapshot() filters by age at read time so a never-pruned
// tail can't inflate reported kbps.
func (b *bwTracker) record(ip string, bytes int64) {
	if bytes <= 0 || ip == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	samples := b.clients[ip]
	// Lazy prune: ~4 samples per window is typical (one per ~2.5s
	// segment at ~10s window); 32 leaves 8× headroom before we walk
	// the slice. Bounds memory at ~1KB per active IP in the worst
	// case (sample is 24 bytes + slice overhead).
	if len(samples) > 32 {
		cutoff := now.Add(-b.window)
		kept := samples[:0]
		for _, s := range samples {
			if s.at.After(cutoff) {
				kept = append(kept, s)
			}
		}
		samples = kept
	}
	b.clients[ip] = append(samples, bwSample{at: now, bytes: bytes})

	// Update the per-second history ring. New second = advance head
	// and seed; same second as last write = accumulate into the
	// current slot. The ring wraps at historyBuckets so the oldest
	// slot is naturally overwritten without bookkeeping.
	sec := now.Unix()
	if b.timestamps[b.historyHead] != sec {
		b.historyHead = (b.historyHead + 1) % len(b.history)
		b.timestamps[b.historyHead] = sec
		b.history[b.historyHead] = 0
	}
	b.history[b.historyHead] += bytes
}

// BwHistorySample is one second's total throughput for the
// /admin/api/bandwidth/history sparkline. Ts is unix-seconds; Kbps is
// computed from bytes*8/1000.
type BwHistorySample struct {
	Ts   int64 `json:"ts"`
	Kbps int64 `json:"kbps"`
}

// History returns the per-second total-kbps ring buffer, ordered
// oldest-first. Slots with no traffic (no record() call that second)
// are returned with Kbps=0 — the timeline is dense so the sparkline
// renders contiguously. Pads to historyBuckets so the caller always
// sees a fixed-width window.
func (b *bwTracker) History() []BwHistorySample {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now().Unix()
	out := make([]BwHistorySample, historyBuckets)
	// Walk back from "now" filling each second slot. If the slot in
	// the ring matches the second we're looking for, use its bytes;
	// otherwise it's a zero second.
	for i := 0; i < historyBuckets; i++ {
		ts := now - int64(historyBuckets-1-i) // oldest first
		// Scan a few slots back from head to find a match. With dense
		// traffic this is O(1) since head is right next to current.
		var bytes int64
		for j := 0; j < len(b.history); j++ {
			idx := (b.historyHead - j + len(b.history)) % len(b.history)
			if b.timestamps[idx] == ts {
				bytes = b.history[idx]
				break
			}
			if b.timestamps[idx] < ts {
				break // slots only get older as we walk back
			}
		}
		out[i] = BwHistorySample{Ts: ts, Kbps: bytes * 8 / 1000}
	}
	return out
}

// snapshot returns this caller's current kbps, the room total, and the
// number of distinct viewers seen within the window.
func (b *bwTracker) snapshot(forIP string) (mineKbps, totalKbps int64, viewers int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := time.Now().Add(-b.window)
	windowSec := int64(b.window.Seconds())
	if windowSec == 0 {
		windowSec = 1
	}
	for ip, samples := range b.clients {
		var bytes int64
		for _, s := range samples {
			if s.at.After(cutoff) {
				bytes += s.bytes
			}
		}
		if bytes == 0 {
			continue
		}
		viewers++
		kbps := bytes * 8 / windowSec / 1000
		totalKbps += kbps
		if ip == forIP {
			mineKbps = kbps
		}
	}
	return
}

// clientIP returns the most plausible source IP, honoring the common
// reverse-proxy headers when present.
func clientIP(r *http.Request) string {
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if i := strings.Index(h, ","); i >= 0 {
			return strings.TrimSpace(h[:i])
		}
		return strings.TrimSpace(h)
	}
	if h := r.Header.Get("X-Real-IP"); h != "" {
		return h
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	return ip
}

// countingResponseWriter wraps http.ResponseWriter so we can record the
// number of body bytes sent to the client. We implement BOTH Write and
// io.ReaderFrom so the underlying sendfile() zero-copy path is preserved
// for big segment payloads — Go's net/http detects ReaderFrom on the
// writer and uses it; the inner ResponseWriter typically forwards
// ReadFrom to the TCP socket's ReadFrom, which uses sendfile on Linux.
//
// Without the ReaderFrom implementation, io.Copy falls back to a
// userspace 32KB-buffer Read+Write loop — correct, but each byte makes
// a round trip through userspace. Negligible at watch-party scale, but
// free to fix.
type countingResponseWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingResponseWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// ReadFrom satisfies io.ReaderFrom. If the wrapped writer also
// implements ReaderFrom (the standard library's *response does), we
// forward to it — which on Linux uses sendfile() under the hood.
// Otherwise we fall back to io.Copy through our Write method, so the
// byte counter still works on platforms without zero-copy support.
func (c *countingResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if rf, ok := c.ResponseWriter.(io.ReaderFrom); ok {
		n, err := rf.ReadFrom(r)
		c.n += n
		return n, err
	}
	// Fallback path: forces traffic through Write so it's counted.
	n, err := io.Copy(writerFunc(c.Write), r)
	return n, err
}

// writerFunc adapts a Write function back to an io.Writer for io.Copy.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
