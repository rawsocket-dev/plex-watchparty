package main

import (
	"net/http/httptest"
	"testing"
	"time"
)

func TestBwTrackerKbpsMath(t *testing.T) {
	b := newBwTracker()
	// 10 s window, 12 Mb of data → 12 Mbps average.
	bytes := int64(12_000_000 * 10 / 8) // 15 MB
	b.record("1.2.3.4", bytes)
	mine, total, viewers := b.snapshot("1.2.3.4")
	if mine != 12000 {
		t.Errorf("mine = %d kbps, want 12000", mine)
	}
	if total != 12000 {
		t.Errorf("total = %d kbps, want 12000", total)
	}
	if viewers != 1 {
		t.Errorf("viewers = %d, want 1", viewers)
	}
}

func TestBwTrackerSumsAcrossClients(t *testing.T) {
	b := newBwTracker()
	b.record("a", int64(1_000_000*10/8)) // 1 Mbps
	b.record("b", int64(3_000_000*10/8)) // 3 Mbps
	mine, total, _ := b.snapshot("a")
	if mine != 1000 {
		t.Errorf("mine for a = %d, want 1000", mine)
	}
	if total != 4000 {
		t.Errorf("total = %d, want 4000 (1+3)", total)
	}
}

func TestBwTrackerExpiresOldSamples(t *testing.T) {
	b := newBwTracker()
	b.window = 50 * time.Millisecond
	b.record("ip", 1_000_000)
	time.Sleep(80 * time.Millisecond)
	mine, _, viewers := b.snapshot("ip")
	if mine != 0 || viewers != 0 {
		t.Errorf("after window expiry mine=%d viewers=%d, want 0/0", mine, viewers)
	}
}

func TestClientIPHonorsForwardedFor(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "10.0.0.1:54321"
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 10.0.0.1")
	if got := clientIP(r); got != "8.8.8.8" {
		t.Errorf("clientIP with X-Forwarded-For = %q, want 8.8.8.8", got)
	}
}

func TestClientIPFallsBackToRemoteAddr(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.5:54321"
	if got := clientIP(r); got != "192.168.1.5" {
		t.Errorf("clientIP no headers = %q, want 192.168.1.5", got)
	}
}
