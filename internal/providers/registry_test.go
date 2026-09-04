package providers

import (
	"strings"
	"testing"
)

func TestResolveOrderPreserved(t *testing.T) {
	got, err := Resolve([]string{"googlebooks", "openlibrary"}, "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("Resolve: got %d providers, want 2", len(got))
	}
	if got[0].Name() != "googlebooks" {
		t.Errorf("Resolve[0].Name() = %q, want %q", got[0].Name(), "googlebooks")
	}
	if got[1].Name() != "openlibrary" {
		t.Errorf("Resolve[1].Name() = %q, want %q", got[1].Name(), "openlibrary")
	}
}

func TestResolveUnknownNameNamesIt(t *testing.T) {
	_, err := Resolve([]string{"openlibrary", "bogus"}, "")
	if err == nil {
		t.Fatal("Resolve with an unknown name: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("Resolve error %q does not name the unknown provider", err.Error())
	}
	if !strings.Contains(err.Error(), "googlebooks") || !strings.Contains(err.Error(), "openlibrary") {
		t.Errorf("Resolve error %q does not list the valid providers", err.Error())
	}
}

func TestResolveEmptyIsNoProviders(t *testing.T) {
	got, err := Resolve(nil, "")
	if err != nil {
		t.Fatalf("Resolve(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Resolve(nil) = %d providers, want 0", len(got))
	}
}

func TestParseNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"default pair", "openlibrary,googlebooks", []string{"openlibrary", "googlebooks"}},
		{"empty disables", "", nil},
		{"blank disables", "   ", nil},
		{"whitespace and empty entries trimmed", " openlibrary ,, googlebooks ", []string{"openlibrary", "googlebooks"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseNames(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("ParseNames(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("ParseNames(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}
