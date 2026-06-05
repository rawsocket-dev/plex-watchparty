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
