package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Remuxer runs at most one ffmpeg session at a time. It pulls the Plex source
// ONCE and copy-remuxes it to fMP4 HLS — video is always stream-copied
// (h264 & hevc pass straight through, no transcode). Audio is copied when it
// is already AAC, otherwise re-encoded to AAC for broad browser support
// (cheap; video, the expensive part, is never touched).
type Remuxer struct {
	workDir string

	mu      sync.Mutex
	current string // ratingKey of the active session, "" if none
	dir     string // output dir of the active session
	cancel  context.CancelFunc
}

func NewRemuxer(workDir string) *Remuxer {
	return &Remuxer{workDir: workDir}
}

// SessionDir is the directory whose .m3u8 / .m4s files are served to clients.
func (rx *Remuxer) SessionDir() string {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	return rx.dir
}

func (rx *Remuxer) CurrentKey() string {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	return rx.current
}

// Start (re)starts the ffmpeg session for the given movie. It blocks only
// until the playlist file appears, so callers can flip the player over once
// this returns nil.
func (rx *Remuxer) Start(ratingKey string, si *StreamInfo) error {
	rx.mu.Lock()
	if rx.current == ratingKey && rx.dir != "" {
		rx.mu.Unlock()
		return nil // already streaming this movie
	}
	if rx.cancel != nil {
		rx.cancel()
	}
	dir := filepath.Join(rx.workDir, "session-"+ratingKey)
	_ = os.RemoveAll(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		rx.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rx.current = ratingKey
	rx.dir = dir
	rx.cancel = cancel
	rx.mu.Unlock()

	// On any error below we must clear the session state so the next click
	// of the same movie actually re-attempts ffmpeg, instead of hitting the
	// short-circuit above and returning a stale-but-dead session.
	clearOnError := func() {
		rx.mu.Lock()
		if rx.current == ratingKey {
			rx.current = ""
			rx.dir = ""
			rx.cancel = nil
		}
		rx.mu.Unlock()
	}

	playlist := filepath.Join(dir, "index.m3u8")
	args := []string{
		"-nostdin",
		// `error` rather than `warning`: Blu-ray remuxes with many PGS
		// subtitle tracks emit 20+ benign probe warnings at startup; this
		// keeps the compose log readable so real errors stand out.
		"-loglevel", "error",
		// Bigger probe window: Blu-ray remuxes with many subtitle / audio
		// tracks blow past the defaults and stall startup.
		"-analyzeduration", "100M", "-probesize", "100M",
		// Regenerate / forgive timestamps — Dolby Vision sources sometimes
		// emit DTS=0 frames at the start that otherwise stall the muxer.
		// `discardcorrupt` drops demuxer-flagged bad packets cleanly
		// instead of fighting them through decode (esp. DTS-HD MA XLL).
		"-fflags", "+genpts+igndts+discardcorrupt",
		// Burst the first 60 s of input so the playlist + first segment
		// land fast (low TTFF), then pace to 4× real-time. Without
		// pacing, ffmpeg races through the whole file as fast as Plex
		// can serve it (pinning the audio encode at 200%+ CPU); too
		// tight a cap and the audio encode can momentarily lag below
		// playback rate during heavy decode regions, audible as a skip.
		// 4× is the empirical sweet spot for HEVC + DTS-HD sources.
		"-readrate", "4.0",
		"-readrate_initial_burst", "60",
		"-i", si.URL,
		"-map", "0:v:0", "-map", "0:a:0",
		// Watchparty's design contract: never transcode video. The
		// remux is the whole point. If a source codec / profile isn't
		// playable in a viewer's browser, the answer is to use a Plex
		// source that delivers a browser-friendly variant, not to burn
		// CPU here.
		"-c:v", "copy",
		// Intentionally NOT `-strict unofficial`: without it, ffmpeg
		// drops the Dolby Vision dvcC/dvvC container boxes (the SEI
		// strip below cleans the bitstream-side DV RPU). With the box
		// present, Chrome's decoder pipeline sees "I am Dolby Vision"
		// in the sample description and refuses the stream with
		// kUnsupportedConfig even on machines that fully support the
		// underlying HEVC Main10 codec config we'd otherwise hand it.
	}
	if si.VideoCodec == "hevc" || si.VideoCodec == "h265" {
		args = append(args, "-tag:v", "hvc1") // Safari/Chrome need hvc1 in fMP4
		// Strip HEVC SEI NAL units (39 = prefix SEI, 40 = suffix SEI).
		// Dolby Vision RPU rides in unregistered SEI; some browser
		// decoders refuse the whole stream when they can't parse it.
		// Stripping is a copy-only bitstream filter — no transcode.
		// HDR10 metadata also lives here, so HDR sources play as SDR,
		// which is the right trade for a browser watch-party.
		args = append(args, "-bsf:v", "filter_units=remove_types=39|40")
	}
	if si.AudioCodec == "aac" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args,
			"-c:a", "aac", "-b:a", "192k", "-ac", "2",
			// Insert silence / resync for timing gaps caused by upstream
			// decode dropouts (esp. DTS-HD MA XLL frames the dca decoder
			// can't parse). Without this, dropped frames surface as
			// audible audio skips.
			"-af", "aresample=async=1",
		)
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", "6",
		// `event` rather than `vod`: ffmpeg's vod playlist type does
		// NOT write index.m3u8 incrementally — confirmed by inspecting
		// the workdir during runs. event does, and is what makes the
		// stream playable while ffmpeg is still racing ahead. We deal
		// with the live-seekable-range issue client-side in player.html
		// (hls.js config tweaks below).
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(dir, "seg_%05d.m4s"),
		playlist,
	)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr
	startedAt := time.Now()
	if err := cmd.Start(); err != nil {
		cancel()
		clearOnError()
		return fmt.Errorf("start ffmpeg: %w", err)
	}
	spawnMs := time.Since(startedAt).Milliseconds()
	go func() {
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			log.Printf("ffmpeg exited: %v", err)
		}
	}()

	// Wait for the playlist to materialize (or fail fast). Heavy-stream
	// sources (e.g. Blu-ray remuxes with DV metadata) can take a while
	// just to probe, so be generous here.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(playlist); err == nil {
			log.Printf("ffmpeg %s: spawn=%dms playlist=%dms",
				ratingKey, spawnMs, time.Since(startedAt).Milliseconds())
			return nil
		}
		if ctx.Err() != nil {
			clearOnError()
			return fmt.Errorf("ffmpeg aborted before playlist")
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
	clearOnError()
	return fmt.Errorf("timed out waiting for HLS playlist")
}

func (rx *Remuxer) Stop() {
	rx.mu.Lock()
	defer rx.mu.Unlock()
	if rx.cancel != nil {
		rx.cancel()
		rx.cancel = nil
	}
	rx.current = ""
	rx.dir = ""
}

// PruneOlderThan deletes session-* subdirectories of WORK_DIR whose
// mtime is older than `age`. Each removal is logged with the dir's
// name, age, and size so it's visible in the compose log. Run this
// at startup so a long-uptime deploy doesn't accumulate junk.
func (rx *Remuxer) PruneOlderThan(age time.Duration) {
	entries, err := os.ReadDir(rx.workDir)
	if err != nil {
		log.Printf("prune: cannot read %s: %v", rx.workDir, err)
		return
	}
	cutoff := time.Now().Add(-age)
	var removed int
	var freedBytes int64
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "session-") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		full := filepath.Join(rx.workDir, e.Name())
		var dirBytes int64
		_ = filepath.Walk(full, func(_ string, fi os.FileInfo, _ error) error {
			if fi != nil && !fi.IsDir() {
				dirBytes += fi.Size()
			}
			return nil
		})
		if err := os.RemoveAll(full); err != nil {
			log.Printf("prune: failed to remove %s: %v", e.Name(), err)
			continue
		}
		log.Printf("prune: removed %s (age %s, %s)",
			e.Name(),
			time.Since(info.ModTime()).Round(time.Minute),
			humanBytes(dirBytes))
		removed++
		freedBytes += dirBytes
	}
	if removed == 0 {
		log.Printf("prune: nothing older than %s in %s", age, rx.workDir)
	} else {
		log.Printf("prune: removed %d session(s), freed %s", removed, humanBytes(freedBytes))
	}
}

// humanBytes formats b as a binary-prefixed size (KiB / MiB / GiB).
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
