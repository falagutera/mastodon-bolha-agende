package main

import (
	"io/fs"
	"regexp"
	"testing"
)

// The CSP allows no inline script or style: markup only loads /assets files.
func TestNoInlineScriptOrStyle(t *testing.T) {
	inline := regexp.MustCompile(`(?i)<style|\sstyle=|<script|\son[a-z]+=|javascript:`)
	script := regexp.MustCompile(`(?i)<script\s+src="/assets/[^"]+"(?:\s+defer)?></script>`)
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		if m := inline.FindString(script.ReplaceAllString(string(raw), "")); m != "" {
			t.Errorf("%s: inline script or style %q", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every text exists in both languages, and every key a template reads exists.
func TestTextsInBothLanguages(t *testing.T) {
	en, pt := translations["en"], translations["pt-BR"]
	if len(translations) != 2 || en == nil || pt == nil {
		t.Fatalf("languages: want en and pt-BR, got %d", len(translations))
	}
	for k := range en {
		if pt[k] == "" {
			t.Errorf("%q missing in pt-BR", k)
		}
	}
	for k := range pt {
		if en[k] == "" {
			t.Errorf("%q missing in en", k)
		}
	}
	used := regexp.MustCompile(`\.T\.([a-z0-9_]+)`)
	err := fs.WalkDir(templateFS, "templates", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := fs.ReadFile(templateFS, path)
		if err != nil {
			return err
		}
		for _, m := range used.FindAllStringSubmatch(string(raw), -1) {
			if en[m[1]] == "" {
				t.Errorf("%s uses %q, which has no text", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
