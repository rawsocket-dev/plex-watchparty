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
type bwTracker struct {
	mu      sync.Mutex
	clients map[string][]bwSample
	window  time.Duration
}

type bwSample struct {
	at    time.Time
	bytes int64
}

func newBwTracker() *bwTracker {
	return &bwTracker{
		clients: make(map[string][]bwSample),
		window:  10 * time.Second,
	}
}

// record appends a sample for ip. Empty IPs and zero-byte writes are dropped.
// Samples older than the window are pruned in-place to bound memory.
func (b *bwTracker) record(ip string, bytes int64) {
	if bytes <= 0 || ip == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-b.window)
	samples := b.clients[ip]
	keep := samples[:0]
	for _, s := range samples {
		if s.at.After(cutoff) {
			keep = append(keep, s)
		}
	}
	keep = append(keep, bwSample{at: now, bytes: bytes})
	b.clients[ip] = keep
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
