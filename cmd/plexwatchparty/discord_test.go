package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestQualityLine(t *testing.T) {
	cases := []struct {
		name string
		si   StreamInfo
		want string
	}{
		{"4k hevc", StreamInfo{VideoCodec: "hevc", Width: 3840, Height: 2160}, "4K HEVC → 1080p"},
		{"1080 h264", StreamInfo{VideoCodec: "h264", Width: 1920, Height: 1080}, "1080p H264 → 1080p"},
		{"720", StreamInfo{VideoCodec: "h264", Width: 1280, Height: 720}, "720p H264 → 1080p"},
		{"no dims", StreamInfo{VideoCodec: "h264"}, ""},
		{"no codec", StreamInfo{Width: 1920, Height: 1080}, "1080p → 1080p"},
	}
	for _, c := range cases {
		if got := qualityLine(c.si); got != c.want {
			t.Errorf("%s: qualityLine = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestResBucket(t *testing.T) {
	cases := []struct {
		w, h int
		want string
	}{
		{3840, 2160, "4K"},
		{1920, 1080, "1080p"},
		{1280, 720, "720p"},
		{720, 480, "480p"}, // the %dp fallback
		{0, 0, ""},         // unknown
	}
	for _, c := range cases {
		if got := resBucket(c.w, c.h); got != c.want {
			t.Errorf("resBucket(%d,%d) = %q, want %q", c.w, c.h, got, c.want)
		}
	}
}

func TestPublicBaseURL(t *testing.T) {
	cases := []struct {
		explicit, redirect, want string
	}{
		{"https://p.example/", "", "https://p.example"},
		{"", "https://party.bsd-unix.net/oauth/callback", "https://party.bsd-unix.net"},
		{"http://x.local:8080", "ignored", "http://x.local:8080"},
		{"", "relative/no-scheme", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := publicBaseURL(c.explicit, c.redirect); got != c.want {
			t.Errorf("publicBaseURL(%q,%q) = %q, want %q", c.explicit, c.redirect, got, c.want)
		}
	}
}

func findField(fields []discordField, name string) (string, bool) {
	for _, f := range fields {
		if f.Name == name {
			return f.Value, true
		}
	}
	return "", false
}

func TestBuildPayloadStart(t *testing.T) {
	ev := notifyEvent{
		Kind: notifyStart, Title: "Blade Runner 2049", Year: 2017, RatingKey: "42",
		Actor: "Brian", RuntimeSec: 9840, ResumeSec: 0, Quality: "4K HEVC → 1080p",
		Tagline: "There's a storm coming.", Summary: "A young blade runner unearths a secret.",
		ContentRating: "R", CriticRating: 8.0, AudienceRating: 8.1,
		Genres: []string{"Sci-Fi", "Drama"}, IMDbID: "tt1856101", TMDBID: "335984",
	}
	p := buildPayload(ev, "https://party.example")
	if len(p.Embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(p.Embeds))
	}
	e := p.Embeds[0]
	if e.Author == nil || e.Author.Name != "▶ Now Playing" {
		t.Errorf("author = %+v", e.Author)
	}
	if e.Title != "Blade Runner 2049 (2017)" {
		t.Errorf("title = %q", e.Title)
	}
	if e.URL != "https://www.imdb.com/title/tt1856101/" {
		t.Errorf("title url = %q", e.URL)
	}
	if e.Color != colorGreen {
		t.Errorf("color = %d, want green", e.Color)
	}
	// Big poster goes in Image, not the small Thumbnail.
	if e.Image == nil || e.Image.URL != "https://party.example/poster/42.jpg" {
		t.Errorf("image = %+v", e.Image)
	}
	if e.Thumbnail != nil {
		t.Errorf("start should use Image, not Thumbnail: %+v", e.Thumbnail)
	}
	if !strings.Contains(e.Description, "There's a storm coming.") ||
		!strings.Contains(e.Description, "A young blade runner") {
		t.Errorf("description = %q", e.Description)
	}
	if v, ok := findField(e.Fields, "Started by"); !ok || v != "Brian" {
		t.Errorf("started-by field = %q,%v", v, ok)
	}
	if v, ok := findField(e.Fields, "Runtime"); !ok || v != "2:44:00" {
		t.Errorf("runtime field = %q,%v", v, ok)
	}
	if v, ok := findField(e.Fields, "Quality"); !ok || v != "4K HEVC → 1080p" {
		t.Errorf("quality field = %q,%v", v, ok)
	}
	if v, ok := findField(e.Fields, "Rating"); !ok ||
		!strings.Contains(v, "R") || !strings.Contains(v, "8.0") || !strings.Contains(v, "8.1") {
		t.Errorf("rating field = %q,%v", v, ok)
	}
	if v, ok := findField(e.Fields, "Genres"); !ok || v != "Sci-Fi · Drama" {
		t.Errorf("genres field = %q,%v", v, ok)
	}
	v, ok := findField(e.Fields, "Links")
	if !ok || !strings.Contains(v, "[IMDb](https://www.imdb.com/title/tt1856101/)") ||
		!strings.Contains(v, "Rotten Tomatoes") ||
		!strings.Contains(v, "[TMDB](https://www.themoviedb.org/movie/335984)") {
		t.Errorf("links field = %q,%v", v, ok)
	}
	if _, ok := findField(e.Fields, "Resuming at"); ok {
		t.Error("resume field present for zero offset")
	}
}

func TestBuildPayloadStartResumeOffset(t *testing.T) {
	ev := notifyEvent{Kind: notifyStart, Title: "X", RatingKey: "1", ResumeSec: 4320}
	e := buildPayload(ev, "https://p").Embeds[0]
	if v, ok := findField(e.Fields, "Resuming at"); !ok || v != "1:12:00" {
		t.Errorf("resume field = %q,%v", v, ok)
	}
}

func TestBuildPayloadStop(t *testing.T) {
	stop := buildPayload(notifyEvent{Kind: notifyStop, Title: "X", Year: 1999, RatingKey: "1", Actor: "idle — everyone left", PositionSec: 9840}, "https://p").Embeds[0]
	if stop.Title != "⏹ Movie Ended" || stop.Color != colorGrey {
		t.Errorf("stop embed = %q/%d", stop.Title, stop.Color)
	}
	if stop.Description != "X (1999)" {
		t.Errorf("stop description = %q", stop.Description)
	}
	// Ended embed uses the small thumbnail, not a big image.
	if stop.Thumbnail == nil || stop.Image != nil {
		t.Errorf("stop thumb=%+v image=%+v", stop.Thumbnail, stop.Image)
	}
	if v, _ := findField(stop.Fields, "Ended by"); v != "idle — everyone left" {
		t.Errorf("stop actor = %q", v)
	}
	if v, ok := findField(stop.Fields, "Stopped at"); !ok || v != "2:44:00" {
		t.Errorf("stopped-at field = %q,%v", v, ok)
	}
}

func TestBuildPayloadNoYearTitle(t *testing.T) {
	e := buildPayload(notifyEvent{Kind: notifyStart, Title: "Heat", RatingKey: "1"}, "").Embeds[0]
	if e.Title != "Heat" {
		t.Errorf("no-year title = %q", e.Title)
	}
}

func TestBuildPayloadNoPosterWhenNoBaseURL(t *testing.T) {
	e := buildPayload(notifyEvent{Kind: notifyStart, Title: "X", RatingKey: "1"}, "").Embeds[0]
	if e.Image != nil || e.Thumbnail != nil {
		t.Errorf("poster set with empty base URL: image=%+v thumb=%+v", e.Image, e.Thumbnail)
	}
}

func TestNewNotifierNilWhenNoURL(t *testing.T) {
	n := NewNotifier("", "https://p")
	if n != nil {
		t.Fatal("expected nil notifier when webhook URL empty")
	}
	// All methods must be safe no-ops on nil.
	n.Enqueue(notifyEvent{Kind: notifyStart, Title: "x"})
	n.Close()
}

func TestNotifierDelivers(t *testing.T) {
	got := make(chan discordPayload, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		var p discordPayload
		_ = json.NewDecoder(r.Body).Decode(&p)
		got <- p
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	n := NewNotifier(srv.URL, "https://party.example")
	defer n.Close()
	n.Enqueue(notifyEvent{Kind: notifyStart, Title: "Heat", Year: 1995, RatingKey: "7"})

	select {
	case p := <-got:
		if len(p.Embeds) != 1 || p.Embeds[0].Title != "Heat (1995)" {
			t.Errorf("payload = %+v", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no webhook delivery within 2s")
	}
}
