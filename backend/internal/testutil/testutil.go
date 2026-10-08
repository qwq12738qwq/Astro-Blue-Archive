// Package testutil holds the cross-package test scaffolding: the
// temporary trees the content and config layers are tested
// against. Both the custom-asset and the Markdown-template layers
// are exercised over the same directory shape, and both need the
// same minimum environment for a successful config.Load, so the
// two builders live here once instead of being re-stubbed per
// package.
//
// A package's own helpers stay with its tests. Go's white-box
// tests reach unexported symbols — a handler method, a pipeline
// constructor, a response type — and moving those helpers out
// would mean exporting production code only for the sake of a
// test, which is a worse trade than a helper living beside the
// tests that use it.
package testutil

import (
	"os"
	"path/filepath"
	"testing"

	"blogcms/internal/content"
)

// ContentStore returns a content Store over a fresh temporary
// tree with the directories the content layer expects: posts,
// pages and system. The tree's root is returned alongside it,
// for the tests that hand-place files the listing has to
// report.
func ContentStore(t testing.TB) (*content.Store, string) {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"posts", "pages", "system"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return content.NewStore(root), root
}

// Env builds a getenv over a map, seeded with the minimum a
// successful config.Load needs. `t` is taken as a parameter rather
// than closed over so each test owns its temp root.
func Env(t testing.TB, over map[string]string) func(string) string {
	t.Helper()
	dir := t.TempDir()
	base := map[string]string{
		"CONTENT_ROOT":  dir,
		"MEDIA_ROOT":    dir,
		"DATA_ROOT":     dir,
		"PUBLIC_ORIGIN": "http://127.0.0.1:9900",
	}
	for k, v := range over {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}
