package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.gutocarvalho.net/open/bolha/agende/internal/relnotes"
)

// Every VERSION needs its CHANGELOG.md entry: the release notes page is built from it.
func TestChangelogHasVersion(t *testing.T) {
	if err := relnotes.Check(changelog, versionFile); err != nil {
		t.Fatal(err)
	}
}

// The release notes page is public and needs no database.
func TestReleaseNotesPage(t *testing.T) {
	tmpl, err := parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	s := newServer(config{domain: "example.social", lang: "en"}, tmpl, nil, nil, nil, nil)
	for path, want := range map[string]string{"/release-notes": `id="v` + releases[0].Version + `"`, "/release-notes.json": `"app": "agende"`} {
		rec := httptest.NewRecorder()
		s.routes().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: %d, missing %q", path, rec.Code, want)
		}
	}
}
