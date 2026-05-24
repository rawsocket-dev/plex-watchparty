package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

func NewPlex(baseURL, token, cacheFile string) *Plex {
	p := &Plex{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Token:     token,
		http:      &http.Client{Timeout: 15 * time.Second},
		cacheFile: cacheFile,
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
				Container     string  `json:"container"`
				VideoCodec    string  `json:"videoCodec"`
				AudioCodec    string  `json:"audioCodec"`
				VideoProfile  string  `json:"videoProfile"`
				AudioProfile  string  `json:"audioProfile"`
				Width         int     `json:"width"`
				Height        int     `json:"height"`
				Bitrate       int     `json:"bitrate"`
				AudioChannels int     `json:"audioChannels"`
				VideoFrameRate string `json:"videoFrameRate"`
				Duration      int64   `json:"duration"`
				Part          []struct {
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
	media := metadata.Media[0]
	part := media.Part[0]

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
