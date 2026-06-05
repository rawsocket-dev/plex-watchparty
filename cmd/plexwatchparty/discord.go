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

// notifyKind selects which embed buildPayload produces.
type notifyKind int

const (
	notifyStart notifyKind = iota
	notifyPause
	notifyResume
	notifyStop
)

// Embed accent colors (Discord expects a decimal int; hex literals are fine).
const (
	colorGreen = 0x57F287
	colorAmber = 0xFEE75C
	colorGrey  = 0x95A5A6
)

// notifyEvent is the structured, transport-agnostic description of a
// playback change. The Hub builds one and hands it to Notifier.Enqueue.
type notifyEvent struct {
	Kind        notifyKind
	Title       string
	Year        int
	RatingKey   string
	Actor       string  // display name, or a synthetic label ("idle — everyone left", "admin", "host stepped away")
	PositionSec float64 // pause/resume/stop
	RuntimeSec  float64 // start only
	ResumeSec   float64 // start only; 0 = not a resume
	Quality     string  // start only; "" = omit
}

type discordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

type discordThumbnail struct {
	URL string `json:"url"`
}

type discordEmbed struct {
	Title       string            `json:"title"`
	Description string            `json:"description,omitempty"`
	Color       int               `json:"color"`
	Fields      []discordField    `json:"fields,omitempty"`
	Thumbnail   *discordThumbnail `json:"thumbnail,omitempty"`
}

type discordPayload struct {
	Embeds []discordEmbed `json:"embeds"`
}

// posterURL builds the public poster link for an embed, or "" if no base
// URL is configured. The Plex token is never part of this URL.
func posterURL(baseURL, ratingKey string) string {
	if baseURL == "" || ratingKey == "" {
		return ""
	}
	return baseURL + "/poster/" + ratingKey + ".jpg"
}

// buildPayload turns a notifyEvent into the Discord webhook JSON. Pure and
// deterministic — all delivery concerns live in the worker.
func buildPayload(ev notifyEvent, baseURL string) discordPayload {
	movie := ev.Title
	if ev.Year > 0 {
		movie = fmt.Sprintf("%s (%d)", ev.Title, ev.Year)
	}
	e := discordEmbed{Description: movie}
	if u := posterURL(baseURL, ev.RatingKey); u != "" {
		e.Thumbnail = &discordThumbnail{URL: u}
	}
	addField := func(name, value string, inline bool) {
		if value != "" {
			e.Fields = append(e.Fields, discordField{Name: name, Value: value, Inline: inline})
		}
	}
	switch ev.Kind {
	case notifyStart:
		e.Title = "▶ Now Playing"
		e.Color = colorGreen
		addField("Started by", ev.Actor, true)
		if ev.RuntimeSec > 0 {
			addField("Runtime", fmtClock(ev.RuntimeSec), true)
		}
		addField("Quality", ev.Quality, true)
		if ev.ResumeSec > 0 {
			addField("Resuming at", fmtClock(ev.ResumeSec), true)
		}
	case notifyPause:
		e.Title = "⏸ Paused"
		e.Color = colorAmber
		addField("Paused by", ev.Actor, true)
		addField("Position", fmtClock(ev.PositionSec)+" in", true)
	case notifyResume:
		e.Title = "▶ Resumed"
		e.Color = colorGreen
		addField("Resumed by", ev.Actor, true)
		addField("Position", fmtClock(ev.PositionSec)+" in", true)
	case notifyStop:
		e.Title = "⏹ Movie Ended"
		e.Color = colorGrey
		addField("Ended by", ev.Actor, true)
		if ev.PositionSec > 0 {
			addField("Stopped at", fmtClock(ev.PositionSec), true)
		}
	}
	return discordPayload{Embeds: []discordEmbed{e}}
}
