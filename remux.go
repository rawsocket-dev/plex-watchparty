package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
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
		"-fflags", "+genpts+igndts",
		"-i", si.URL,
		"-map", "0:v:0", "-map", "0:a:0",
		"-c:v", "copy",
		// Allow non-standard MP4 boxes (dvcC/dvvC for Dolby Vision).
		// Browsers ignore them; the HEVC base layer plays normally.
		"-strict", "unofficial",
	}
	if si.VideoCodec == "hevc" || si.VideoCodec == "h265" {
		args = append(args, "-tag:v", "hvc1") // Safari/Chrome need hvc1 in fMP4
	}
	if si.AudioCodec == "aac" {
		args = append(args, "-c:a", "copy")
	} else {
		args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", "6",
		"-hls_playlist_type", "vod",
		"-hls_segment_type", "fmp4",
		"-hls_flags", "independent_segments",
		"-hls_segment_filename", filepath.Join(dir, "seg_%05d.m4s"),
		playlist,
	)

	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("start ffmpeg: %w", err)
	}
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
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("ffmpeg aborted before playlist")
		}
		time.Sleep(200 * time.Millisecond)
	}
	cancel()
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
