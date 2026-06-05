package main

import "testing"

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
	}
	p := buildPayload(ev, "https://party.example")
	if len(p.Embeds) != 1 {
		t.Fatalf("embeds = %d, want 1", len(p.Embeds))
	}
	e := p.Embeds[0]
	if e.Title != "▶ Now Playing" {
		t.Errorf("title = %q", e.Title)
	}
	if e.Color != colorGreen {
		t.Errorf("color = %d, want green", e.Color)
	}
	if e.Description != "Blade Runner 2049 (2017)" {
		t.Errorf("description = %q", e.Description)
	}
	if e.Thumbnail == nil || e.Thumbnail.URL != "https://party.example/poster/42.jpg" {
		t.Errorf("thumbnail = %+v", e.Thumbnail)
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

func TestBuildPayloadPauseAndStop(t *testing.T) {
	pause := buildPayload(notifyEvent{Kind: notifyPause, Title: "X", RatingKey: "1", Actor: "Dana", PositionSec: 4320}, "https://p").Embeds[0]
	if pause.Title != "⏸ Paused" || pause.Color != colorAmber {
		t.Errorf("pause embed = %q/%d", pause.Title, pause.Color)
	}
	if v, _ := findField(pause.Fields, "Position"); v != "1:12:00 in" {
		t.Errorf("pause position = %q", v)
	}
	if v, ok := findField(pause.Fields, "Paused by"); !ok || v != "Dana" {
		t.Errorf("paused-by field = %q,%v", v, ok)
	}
	stop := buildPayload(notifyEvent{Kind: notifyStop, Title: "X", RatingKey: "1", Actor: "idle — everyone left", PositionSec: 9840}, "https://p").Embeds[0]
	if stop.Title != "⏹ Movie Ended" || stop.Color != colorGrey {
		t.Errorf("stop embed = %q/%d", stop.Title, stop.Color)
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
	if e.Description != "Heat" {
		t.Errorf("no-year description = %q", e.Description)
	}
}

func TestBuildPayloadNoPosterWhenNoBaseURL(t *testing.T) {
	e := buildPayload(notifyEvent{Kind: notifyStart, Title: "X", RatingKey: "1"}, "").Embeds[0]
	if e.Thumbnail != nil {
		t.Errorf("thumbnail set with empty base URL: %+v", e.Thumbnail)
	}
}
