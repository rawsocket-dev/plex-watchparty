package main

import (
	"io"
	"log"
	"net/http"
	"strings"
)

// posterHandler serves Plex poster art at /poster/<ratingKey>.jpg. It is
// mounted UNAUTHENTICATED so Discord's servers (which fetch embed images
// from the public internet) can render the thumbnail. Safe to expose: the
// rating key is validated as a bounded [A-Za-z0-9] token before any Plex
// call, the response is image bytes only, and the Plex token never appears
// in it.
func posterHandler(p *Plex) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/poster/"), ".jpg")
		if !validRatingKey(key) {
			http.Error(w, "invalid ratingKey", http.StatusBadRequest)
			return
		}
		body, ct, err := p.PosterStream(key)
		if err != nil {
			if err != errNoPoster {
				log.Printf("poster: %s: %v", key, err)
			}
			http.NotFound(w, r)
			return
		}
		defer body.Close()
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		if _, err := io.Copy(w, body); err != nil {
			log.Printf("poster: copy %s: %v", key, err)
		}
	}
}
