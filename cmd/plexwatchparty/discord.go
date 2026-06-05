package main

import (
	"fmt"
	"net/url"
	"strings"
)

// resBucket maps a source width/height to a friendly resolution label.
// "" means unknown (omit the quality line entirely).
func resBucket(w, h int) string {
	switch {
	case w >= 3000 || h >= 1700:
		return "4K"
	case w >= 1800 || h >= 1000:
		return "1080p"
	case h >= 700:
		return "720p"
	case h > 0:
		return fmt.Sprintf("%dp", h)
	default:
		return ""
	}
}

// qualityLine renders a source→target summary like "4K HEVC → 1080p".
// We always transcode to 1080p. Returns "" if the source dims are unknown.
func qualityLine(si StreamInfo) string {
	bucket := resBucket(si.Width, si.Height)
	if bucket == "" {
		return ""
	}
	if c := strings.ToUpper(si.VideoCodec); c != "" {
		return bucket + " " + c + " → 1080p"
	}
	return bucket + " → 1080p"
}

// publicBaseURL resolves the public origin used to build poster image URLs
// that Discord must fetch. An explicit value wins; otherwise we derive the
// scheme+host from the OAuth redirect URL (always configured and public).
// Returns "" when no usable absolute origin is available — callers then omit
// posters and post text-only embeds.
func publicBaseURL(explicit, googleRedirect string) string {
	if explicit != "" {
		return strings.TrimRight(explicit, "/")
	}
	u, err := url.Parse(googleRedirect)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
