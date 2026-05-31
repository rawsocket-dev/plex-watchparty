package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAliasStoreSetGetRemove(t *testing.T) {
	s := NewAliasStore(filepath.Join(t.TempDir(), "aliases.json"))
	if got := s.Get("a@x.com"); got != "" {
		t.Errorf("Get on empty = %q, want \"\"", got)
	}
	s.Set("A@X.com", "Ada") // mixed-case email normalizes to lower
	if got := s.Get("a@x.com"); got != "Ada" {
		t.Errorf("Get after Set = %q, want Ada", got)
	}
	s.Remove("a@x.com")
	if got := s.Get("a@x.com"); got != "" {
		t.Errorf("Get after Remove = %q, want \"\"", got)
	}
}

func TestAliasStoreListSortedCopy(t *testing.T) {
	s := NewAliasStore(filepath.Join(t.TempDir(), "aliases.json"))
	s.Set("c@x.com", "Cy")
	s.Set("a@x.com", "Ada")
	s.Set("b@x.com", "Bee")
	list := s.List()
	want := []string{"a@x.com", "b@x.com", "c@x.com"}
	if len(list) != 3 {
		t.Fatalf("List len = %d, want 3", len(list))
	}
	for i, e := range list {
		if e.Email != want[i] {
			t.Errorf("List[%d].Email = %q, want %q", i, e.Email, want[i])
		}
	}
	// Mutating the returned slice must not change internal state.
	list[0].Alias = "MUTATED"
	if got := s.Get("a@x.com"); got != "Ada" {
		t.Errorf("internal state changed after mutating List result: %q", got)
	}
}

func TestAliasStorePersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aliases.json")
	s := NewAliasStore(path)
	s.Set("a@x.com", "Ada")
	s.Set("b@x.com", "Bee")
	s2 := NewAliasStore(path) // fresh store, same path, must load mappings
	if got := s2.Get("a@x.com"); got != "Ada" {
		t.Errorf("reloaded Get(a) = %q, want Ada", got)
	}
	if got := s2.Get("b@x.com"); got != "Bee" {
		t.Errorf("reloaded Get(b) = %q, want Bee", got)
	}
}

func TestAliasStoreCorruptFileTolerated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aliases.json")
	if err := os.WriteFile(path, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewAliasStore(path) // must not panic; starts empty
	if got := s.Get("a@x.com"); got != "" {
		t.Errorf("corrupt-file store Get = %q, want \"\"", got)
	}
	s.Set("a@x.com", "Ada") // and is still usable
	if got := s.Get("a@x.com"); got != "Ada" {
		t.Errorf("Get after Set on recovered store = %q, want Ada", got)
	}
}

func TestParseAliasArgs(t *testing.T) {
	cases := []struct {
		name                 string
		email, alias         string
		wantEmail, wantAlias string
		wantOK               bool
	}{
		{"valid", "Alice@X.com", "Ally", "alice@x.com", "Ally", true},
		{"trims and lowercases email", "  Bob@X.COM ", "Bob", "bob@x.com", "Bob", true},
		{"empty email rejected", "", "Ally", "", "", false},
		{"missing at-sign rejected", "notanemail", "Ally", "", "", false},
		{"empty alias rejected", "a@x.com", "", "", "", false},
		{"whitespace-only alias rejected", "a@x.com", "   ", "", "", false},
		{"all-emoji alias rejected", "a@x.com", "😀🎬", "", "", false},
		{"emoji stripped, ascii kept", "a@x.com", "Al😀ly", "a@x.com", "Ally", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e, a, ok := parseAliasArgs(c.email, c.alias)
			if ok != c.wantOK || e != c.wantEmail || a != c.wantAlias {
				t.Errorf("parseAliasArgs(%q,%q) = (%q,%q,%v), want (%q,%q,%v)",
					c.email, c.alias, e, a, ok, c.wantEmail, c.wantAlias, c.wantOK)
			}
		})
	}
}
