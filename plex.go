package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Plex talks to a Plex Media Server using a server-side token.
// The token NEVER leaves this process — clients only ever see remuxed HLS.
type Plex struct {
	BaseURL string // e.g. http://192.168.1.10:32400
	Token   string
	http    *http.Client
}

func NewPlex(baseURL, token string) *Plex {
	return &Plex{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		http:    &http.Client{Timeout: 15 * time.Second},
	}
}

type Movie struct {
	RatingKey string `json:"ratingKey"`
	Title     string `json:"title"`
	Year      int    `json:"year"`
}

// StreamInfo is everything the remuxer needs for one movie.
type StreamInfo struct {
	URL        string // direct progressive Part URL incl. token (server-side only)
	VideoCodec string // "h264", "hevc", ...
	AudioCodec string // "aac", "ac3", "eac3", ...
	Container  string
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

// ListMovies returns every item across all movie-type library sections.
func (p *Plex) ListMovies() ([]Movie, error) {
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
	return out, nil
}

type metadataResp struct {
	MediaContainer struct {
		Metadata []struct {
			Media []struct {
				Container string `json:"container"`
				Part      []struct {
					Key    string `json:"key"`
					Stream []struct {
						StreamType int    `json:"streamType"` // 1=video, 2=audio
						Codec      string `json:"codec"`
					} `json:"Stream"`
				} `json:"Part"`
			} `json:"Media"`
		} `json:"Metadata"`
	} `json:"MediaContainer"`
}

// Resolve turns a ratingKey into a direct, range-capable progressive URL plus
// codec info so the remuxer can decide on container tags / audio handling.
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
	media := mr.MediaContainer.Metadata[0].Media[0]
	part := media.Part[0]

	si := &StreamInfo{
		URL: p.BaseURL + part.Key + "?X-Plex-Token=" + url.QueryEscape(p.Token) +
			"&download=1",
		Container: media.Container,
	}
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
		}
	}
	return si, nil
}
