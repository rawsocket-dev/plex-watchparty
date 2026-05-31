package main

import "testing"

func TestValidRatingKey(t *testing.T) {
	good := []string{"1", "12345", "00", "rk1", "abcDEF123"} // alphanumeric keys
	for _, s := range good {
		if !validRatingKey(s) {
			t.Errorf("validRatingKey(%q) = false, want true", s)
		}
	}

	bad := []string{
		"",                      // empty
		"123/../../etc",         // path traversal
		"123?X-Plex-Token=evil", // query smuggling
		"12 34",                 // space
		"12.3",                  // dot
		"../1",                  // leading traversal
		"\n12",                  // control char
		"%2e%2e",                // encoded dots
		"a#b",                   // fragment
	}
	for _, s := range bad {
		if validRatingKey(s) {
			t.Errorf("validRatingKey(%q) = true, want false", s)
		}
	}
}
