package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Plex talks to a Plex Media Server using a server-side token.
// The token NEVER leaves this process — clients only ever see remuxed HLS.
type Plex struct {
	BaseURL string // e.g. http://192.168.1.10:32400
	Token   string
	http    *http.Client

	// TranscodeKbps, when > 0, routes Resolve through Plex's Universal
	// Transcoder at 1080p h264 / target kbps instead of fetching the raw
	// original file. This is how we make 70+ Mbps 4K HEVC HDR Blu-ray
	// remuxes watch-party-friendly without forcing the user to wait
	// hours for Plex's offline Optimize feature. Plex transcodes on
	// demand (uses its own hardware acceleration if configured).
	// 0 / unset = direct-stream the original (legacy behaviour).
	TranscodeKbps int

	// ListMovies cache: walking every movie section costs several seconds
	// on a large library. The cache is held in memory for the TTL below,
	// and (when cacheFile is non-empty) also persisted to disk so a
	// container restart doesn't pay the cold-start cost.
	moviesMu  sync.Mutex
	moviesAt  time.Time
	moviesVal []Movie
	cacheFile string
}

// 30 minutes is long enough that a typical watch-party session (browsing
// the library, picking a movie, restarting the container if needed)
// never sees a Plex round-trip after the first call; short enough that
// newly-added content shows up within the same evening.
const moviesCacheTTL = 30 * time.Minute

func NewPlex(baseURL, token, cacheFile string, transcodeKbps int) *Plex {
	p := &Plex{
		BaseURL:       strings.TrimRight(baseURL, "/"),
		Token:         token,
		http:          &http.Client{Timeout: 15 * time.Second},
		TranscodeKbps: transcodeKbps,
		cacheFile:     cacheFile,
	}
	p.loadCacheFromDisk()
	return p
}

type Movie struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
}

// StreamInfo is everything the remuxer needs for one movie, plus enough
// human-readable detail to log what's about to play.
type StreamInfo struct {
	URL          string // direct progressive Part URL incl. token (server-side only)
	VideoCodec   string // "h264", "hevc", ...
	AudioCodec   string // "aac", "ac3", "eac3", "dca" (= DTS), ...
	Container    string // "mkv", "mp4", ...
	VideoProfile string // "Main 10", "High", ...
	AudioProfile string // "ma" (DTS-HD MA), "es" (DTS-ES), ...
	Width        int
	Height       int
	Bitrate      int    // kbps, total file
	AudioBitrate int    // kbps
	AudioChannels int
	FrameRate    string // "24p", "60p", ...
	Duration     int64  // ms
	Size         int64  // bytes
	// Transcoded is true when URL points to Plex's Universal Transcoder
	// (rather than a raw file). The remuxer uses this to drop the
	// -readrate cap: Plex's transcoder paces its own output, so there's
	// no need to throttle reads.
	Transcoded bool
}

// ServerIdentity is the subset of Plex's root response we care about
// for a startup health check.
type ServerIdentity struct {
	FriendlyName      string `json:"friendlyName"`
	MachineIdentifier string `json:"machineIdentifier"`
	Version           string `json:"version"`
	Platform          string `json:"platform"`
	PlatformVersion   string `json:"platformVersion"`
}

// Ping hits the Plex root endpoint with the configured token. Verifies
// (a) the server is reachable, (b) the token is valid, and (c) returns
// enough identity to log a "talking to <server>, version <X>" line at
// startup. A non-nil error means one of those checks failed.
func (p *Plex) Ping() (*ServerIdentity, error) {
	var resp struct {
		MediaContainer ServerIdentity `json:"MediaContainer"`
	}
	if err := p.get("/", &resp); err != nil {
		return nil, err
	}
	if resp.MediaContainer.MachineIdentifier == "" {
		return nil, fmt.Errorf("plex returned an empty identity (token may be invalid)")
	}
	return &resp.MediaContainer, nil
}

func (p *Plex) get(path string, v any) error {
	u := p.BaseURL + path
	if strings.Contains(path, "?") {
		u += "&"
	} else {
		u += "?"
	}
	u += "X-Plex-Token=" + url.QueryEscape(p.Token)

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("plex %s: status %d", path, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

type sectionsResp struct {
	MediaContainer struct {
		Directory []struct {
			Key  string `json:"key"`
			Type string `json:"type"`
		} `json:"Directory"`
	} `json:"MediaContainer"`
}

type libraryResp struct {
	MediaContainer struct {
		Metadata []struct {
			RatingKey string `json:"ratingKey"`
			Title     string `json:"title"`
			Year      int    `json:"year"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// cachedLibrary is the on-disk shape of the library cache.
type cachedLibrary struct {
	At     time.Time `json:"at"`
	Movies []Movie   `json:"movies"`
}

func (p *Plex) loadCacheFromDisk() {
	if p.cacheFile == "" {
		return
	}
	data, err := os.ReadFile(p.cacheFile)
	if err != nil {
		return // missing/unreadable is fine — first ListMovies will populate
	}
	var entry cachedLibrary
	if err := json.Unmarshal(data, &entry); err != nil {
		log.Printf("library cache: parse %s: %v", p.cacheFile, err)
		return
	}
	p.moviesMu.Lock()
	p.moviesVal = entry.Movies
	p.moviesAt = entry.At
	p.moviesMu.Unlock()
	log.Printf("library cache: loaded %d titles from %s (saved %s)",
		len(entry.Movies), p.cacheFile,
		time.Since(entry.At).Round(time.Second))
}

func (p *Plex) saveCacheToDisk() {
	if p.cacheFile == "" {
		return
	}
	p.moviesMu.Lock()
	entry := cachedLibrary{At: p.moviesAt, Movies: p.moviesVal}
	p.moviesMu.Unlock()
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p.cacheFile), 0o755); err != nil {
		log.Printf("library cache: mkdir: %v", err)
		return
	}
	tmp := p.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("library cache: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, p.cacheFile); err != nil {
		log.Printf("library cache: rename: %v", err)
	}
}

// ListMovies returns every item across all movie-type library sections.
// Cached in-memory + on disk for `moviesCacheTTL`.
func (p *Plex) ListMovies() ([]Movie, error) {
	p.moviesMu.Lock()
	if p.moviesVal != nil && time.Since(p.moviesAt) < moviesCacheTTL {
		out := p.moviesVal
		p.moviesMu.Unlock()
		return out, nil
	}
	p.moviesMu.Unlock()

	var sr sectionsResp
	if err := p.get("/library/sections", &sr); err != nil {
		return nil, err
	}
	var out []Movie
	for _, d := range sr.MediaContainer.Directory {
		if d.Type != "movie" {
			continue
		}
		var lr libraryResp
		if err := p.get("/library/sections/"+d.Key+"/all", &lr); err != nil {
			return nil, err
		}
		for _, m := range lr.MediaContainer.Metadata {
			out = append(out, Movie{RatingKey: m.RatingKey, Title: m.Title, Year: m.Year})
		}
	}

	p.moviesMu.Lock()
	p.moviesVal = out
	p.moviesAt = time.Now()
	p.moviesMu.Unlock()
	p.saveCacheToDisk()
	return out, nil
}

type metadataResp struct {
	MediaContainer struct {
		Metadata []struct {
			Title    string `json:"title"`
			Duration int64  `json:"duration"`
			Media    []struct {
				Container             string `json:"container"`
				VideoCodec            string `json:"videoCodec"`
				AudioCodec            string `json:"audioCodec"`
				VideoProfile          string `json:"videoProfile"`
				AudioProfile          string `json:"audioProfile"`
				Width                 int    `json:"width"`
				Height                int    `json:"height"`
				Bitrate               int    `json:"bitrate"`
				AudioChannels         int    `json:"audioChannels"`
				VideoFrameRate        string `json:"videoFrameRate"`
				Duration              int64  `json:"duration"`
				// Plex sets this to 1 on Optimized (pre-transcoded)
				// versions and on Direct Stream-friendly originals.
				// We treat 1 as "browser-friendly variant" and prefer
				// it over the raw original (which can be 70+ Mbps HEVC
				// HDR that no browser wants to deal with).
				OptimizedForStreaming int `json:"optimizedForStreaming"`
				Part                  []struct {
					Key    string `json:"key"`
					Size   int64  `json:"size"`
					Stream []struct {
						StreamType int    `json:"streamType"` // 1=video, 2=audio
						Codec      string `json:"codec"`
						Bitrate    int    `json:"bitrate"`
					} `json:"Stream"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// Resolve turns a ratingKey into a direct, range-capable progressive URL plus
// the codec/profile/size/etc. info we want to log and act on.
func (p *Plex) Resolve(ratingKey string) (*StreamInfo, error) {
	var mr metadataResp
	if err := p.get("/library/metadata/"+ratingKey, &mr); err != nil {
		return nil, err
	}
	if len(mr.MediaContainer.Metadata) == 0 ||
		len(mr.MediaContainer.Metadata[0].Media) == 0 ||
		len(mr.MediaContainer.Metadata[0].Media[0].Part) == 0 {
		return nil, fmt.Errorf("no playable part for ratingKey %s", ratingKey)
	}
	metadata := mr.MediaContainer.Metadata[0]

	// Pick the best Media variant for browser playback. Plex movies
	// often have multiple Media entries: the original Blu-ray remux
	// (HEVC HDR @ 70+ Mbps) plus one or more Optimized versions
	// (h264 @ 8-12 Mbps, what Plex generates via "Optimize"). The
	// optimized version is dramatically friendlier to MSE / hls.js /
	// VideoToolbox — fewer decoder errors, smaller buffers, broader
	// browser compat. Always prefer it when present.
	mediaIdx := 0
	chosenReason := "default (only variant)"
	if len(metadata.Media) > 1 {
		chosenReason = "default (no optimized variant)"
		for i, m := range metadata.Media {
			if m.OptimizedForStreaming == 1 && len(m.Part) > 0 {
				mediaIdx = i
				chosenReason = "optimizedForStreaming=1"
				break
			}
		}
	}
	media := metadata.Media[mediaIdx]
	if len(media.Part) == 0 {
		return nil, fmt.Errorf("chosen media variant has no Part for ratingKey %s", ratingKey)
	}
	part := media.Part[0]
	if len(metadata.Media) > 1 {
		log.Printf("plex: ratingKey %s has %d Media variants; picked #%d (%s, %dx%d %s @ %d kbps)",
			ratingKey, len(metadata.Media), mediaIdx, chosenReason,
			media.Width, media.Height, media.VideoCodec, media.Bitrate)
	}

	// Older Plex responses only populate Duration at the Metadata level,
	// not inside the Media block. Prefer the inner value (it's per-version
	// and more accurate when a library item has multiple Media entries)
	// but fall back to the outer one so the scrub bar always has a real
	// movie length and not 0 / v.duration.
	duration := media.Duration
	if duration == 0 {
		duration = metadata.Duration
		if duration > 0 {
			log.Printf("plex: ratingKey %s — Media.Duration was 0, falling back to Metadata.Duration=%dms",
				ratingKey, duration)
		} else {
			log.Printf("plex: ratingKey %s — no duration at either level; scrub bar will fall back to v.duration",
				ratingKey)
		}
	}

	si := &StreamInfo{
		URL: p.BaseURL + part.Key + "?X-Plex-Token=" + url.QueryEscape(p.Token) +
			"&download=1",
		Container:     media.Container,
		VideoCodec:    strings.ToLower(media.VideoCodec),
		AudioCodec:    strings.ToLower(media.AudioCodec),
		VideoProfile:  media.VideoProfile,
		AudioProfile:  media.AudioProfile,
		Width:         media.Width,
		Height:        media.Height,
		Bitrate:       media.Bitrate,
		AudioChannels: media.AudioChannels,
		FrameRate:     media.VideoFrameRate,
		Duration:      duration,
		Size:          part.Size,
	}
	// If on-the-fly transcode is enabled, replace the direct-download URL
	// with Plex's Universal Transcoder endpoint. Plex will do the heavy
	// HEVC→h264 + HDR→SDR work on its own hardware; our ffmpeg just
	// remuxes the resulting MKV stream to HLS. StreamInfo fields are
	// updated to reflect the post-transcode characteristics so logs and
	// the scrub bar show the right thing.
	if p.TranscodeKbps > 0 {
		si.URL = p.transcodeURL(ratingKey, mediaIdx, 0)
		si.Transcoded = true
		si.VideoCodec = "h264"
		si.AudioCodec = "aac"
		si.VideoProfile = "high"
		si.AudioProfile = ""
		si.Container = "mkv"
		si.Width = 1920
		si.Height = 1080
		si.Bitrate = p.TranscodeKbps
		si.AudioChannels = 2
		log.Printf("plex: routing ratingKey %s through Universal Transcoder → 1920x1080 h264 @ %d kbps",
			ratingKey, p.TranscodeKbps)
		log.Printf("plex: transcode URL = %s", redactedURL(si.URL))
		// Pre-flight the transcode URL so we surface Plex's actual error
		// message (which usually identifies the missing parameter or
		// reason) instead of letting ffmpeg report a bare 400.
		if err := p.preflightTranscode(si.URL); err != nil {
			return nil, fmt.Errorf("plex transcoder rejected request: %w", err)
		}
	}
	// Fall back to Stream entries if the top-level Media fields are empty
	// (older Plex responses sometimes only populate one level).
	for _, s := range part.Stream {
		switch s.StreamType {
		case 1:
			if si.VideoCodec == "" {
				si.VideoCodec = strings.ToLower(s.Codec)
			}
		case 2:
			if si.AudioCodec == "" {
				si.AudioCodec = strings.ToLower(s.Codec)
			}
			if si.AudioBitrate == 0 {
				si.AudioBitrate = s.Bitrate
			}
		}
	}
	return si, nil
}

// preflightTranscode validates our transcode params against Plex's
// /decision endpoint, which is the dry-run sibling of /start.mkv. It
// accepts the same parameters but returns structured JSON describing
// what the transcoder *would* do — or, on failure, why it can't. This
// is far more useful for debugging than /start.mkv's generic HTML 400.
func (p *Plex) preflightTranscode(transcodeURL string) error {
	decisionURL := strings.Replace(transcodeURL,
		"/transcode/universal/start?",
		"/transcode/universal/decision?", 1)
	req, err := http.NewRequest(http.MethodGet, decisionURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := p.http.Do(req)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer resp.Body.Close()
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	bodyStr := strings.TrimSpace(string(body[:n]))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		log.Printf("plex: decision OK (status %d, body: %s)", resp.StatusCode, truncate(bodyStr, 400))
		return nil
	}
	if bodyStr == "" {
		bodyStr = "<empty body>"
	}
	return fmt.Errorf("decision status %d: %s", resp.StatusCode, bodyStr)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// transcodeURL builds a Plex Universal Transcoder URL that targets
// 1920x1080 h264 at p.TranscodeKbps. Plex transcodes on demand —
// using its own hardware acceleration if configured — and serves
// the result as a chunked MKV over HTTP. Our ffmpeg consumes that
// directly, treating it just like the original input.
//
// The X-Plex-* params are required: Plex's transcoder refuses requests
// that don't look like they're coming from a real Plex client.
func (p *Plex) transcodeURL(ratingKey string, mediaIdx, partIdx int) string {
	sessionID := "watchparty-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// X-Plex-Client-Profile-Extra defines the transcode target Plex
	// should pick. Required for our use case — Plex's "Generic" base
	// platform has NO preset transcode targets, so without these
	// add-transcode-target clauses Plex returns decision code 2000
	// ("neither direct play nor conversion is available") which surfaces
	// as a blank HTML 400 from /start. Pattern + comma encoding
	// (%2C inside the DSL value) mirrors plezy's reference client.
	profileExtra := strings.Join([]string{
		"add-settings(DirectPlayStreamSelection=true)",
		fmt.Sprintf("add-limitation(scope=videoCodec&scopeName=*&type=upperBound&name=video.bitrate&value=%d&replace=true)",
			p.TranscodeKbps),
		"add-transcode-target(type=videoProfile&context=streaming&protocol=http&container=mkv" +
			"&videoCodec=h264%2Chevc%2C*&audioCodec=opus%2Cvorbis%2Cflac%2C*" +
			"&subtitleCodec=ass%2Cpgs%2Cvobsub%2C*)",
		"add-transcode-target-settings(type=videoProfile&context=streaming" +
			"&protocol=http&CopyMatroskaAttachments=true)",
	}, "+")

	q := url.Values{}
	q.Set("hasMDE", "1")
	q.Set("path", "/library/metadata/"+ratingKey)
	q.Set("mediaIndex", strconv.Itoa(mediaIdx))
	q.Set("partIndex", strconv.Itoa(partIdx))
	q.Set("protocol", "http") // chunked MKV from the universal start endpoint
	q.Set("fastSeek", "1")
	q.Set("directPlay", "0")
	q.Set("directStream", "0")
	q.Set("subtitleSize", "100")
	q.Set("audioBoost", "100")
	q.Set("location", "lan")
	q.Set("maxVideoBitrate", strconv.Itoa(p.TranscodeKbps))
	q.Set("addDebugOverlay", "0")
	q.Set("autoAdjustQuality", "0")
	q.Set("directStreamAudio", "0")
	q.Set("mediaBufferSize", "102400")
	q.Set("session", sessionID)
	q.Set("subtitles", "none")
	q.Set("copyts", "1")
	q.Set("Accept-Language", "en")
	q.Set("X-Plex-Session-Identifier", sessionID)
	q.Set("X-Plex-Client-Profile-Extra", profileExtra)
	q.Set("X-Plex-Chunked", "1")
	q.Set("X-Plex-Features", "external-media,indirect-media")
	q.Set("X-Plex-Model", "standalone")
	q.Set("X-Plex-Language", "en")
	q.Set("X-Plex-Product", "plexwatchparty")
	q.Set("X-Plex-Version", "1.0")
	q.Set("X-Plex-Client-Identifier", "plexwatchparty")
	// X-Plex-Platform=Linux is REJECTED by Plex's transcoder with a
	// blank HTML 400. Plex maps known platform names ("Chrome", "Mac",
	// etc.) to base transcode profiles; "Linux" / "MacOSX" / "Flutter"
	// are NOT recognized. "Generic" is accepted and starts with no
	// preset targets — which is fine because we supplied our own via
	// X-Plex-Client-Profile-Extra above.
	q.Set("X-Plex-Platform", "Generic")
	q.Set("X-Plex-Device", "plexwatchparty")
	q.Set("X-Plex-Token", p.Token)
	return p.BaseURL + "/video/:/transcode/universal/start?" + q.Encode()
}

// redactedURL returns a transcode URL with the token replaced by
// "<redacted>" so we can safely log it.
func redactedURL(u string) string {
	idx := strings.Index(u, "X-Plex-Token=")
	if idx == -1 {
		return u
	}
	end := strings.Index(u[idx:], "&")
	if end == -1 {
		return u[:idx] + "X-Plex-Token=<redacted>"
	}
	return u[:idx] + "X-Plex-Token=<redacted>" + u[idx+end:]
}
