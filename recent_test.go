package main

import (
	"path/filepath"
	"testing"
)

func TestRecentMoviesTouchPromotes(t *testing.T) {
	r := NewRecentMovies(filepath.Join(t.TempDir(), "recent.json"))
	r.Touch("a", "Movie A", 2020)
	r.Touch("b", "Movie B", 2021)
	r.Touch("c", "Movie C", 2022)
	r.Touch("a", "Movie A", 2020) // promote
	got := r.List()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3 (touch should dedup, not append)", len(got))
	}
	if got[0].RatingKey != "a" {
		t.Errorf("front = %q, want 'a' after re-touch", got[0].RatingKey)
	}
}

func TestRecentMoviesCapsAtFive(t *testing.T) {
	r := NewRecentMovies(filepath.Join(t.TempDir(), "recent.json"))
	for i, k := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		r.Touch(k, "Movie "+k, 2000+i)
	}
	got := r.List()
	if len(got) != recentCap {
		t.Fatalf("len = %d, want cap %d", len(got), recentCap)
	}
	if got[0].RatingKey != "g" {
		t.Errorf("front = %q, want 'g' (newest)", got[0].RatingKey)
	}
	if got[len(got)-1].RatingKey == "a" || got[len(got)-1].RatingKey == "b" {
		t.Errorf("oldest = %q, want one of c/d/e (a and b should be evicted)", got[len(got)-1].RatingKey)
	}
}

func TestRecentMoviesPersistsAndLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recent.json")
	r1 := NewRecentMovies(path)
	r1.Touch("k", "Knives Out", 2019)
	r1.Touch("j", "Jaws", 1975)

	r2 := NewRecentMovies(path)
	r2.Load()
	got := r2.List()
	if len(got) != 2 {
		t.Fatalf("len after Load = %d, want 2", len(got))
	}
	if got[0].Title != "Jaws" || got[1].Title != "Knives Out" {
		t.Errorf("Load order = [%q, %q], want [Jaws, Knives Out]", got[0].Title, got[1].Title)
	}
}

func TestRecentMoviesIgnoresEmptyRatingKey(t *testing.T) {
	r := NewRecentMovies(filepath.Join(t.TempDir(), "recent.json"))
	r.Touch("", "no key", 2020)
	if got := r.List(); len(got) != 0 {
		t.Errorf("empty-key Touch added an entry: %+v", got)
	}
}
