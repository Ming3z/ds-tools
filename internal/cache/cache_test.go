package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSetPrunesExpiredEntries(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, time.Hour)

	c.Set("old", map[string]string{"v": "old"})
	c.Set("fresh", map[string]string{"v": "fresh"})

	oldPath := c.file("old")
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	// Any successful Set prunes entries whose file mtime is older than TTL.
	c.Set("new", map[string]string{"v": "new"})

	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("expired entry survived prune (err=%v)", err)
	}
	if _, err := os.Stat(c.file("fresh")); err != nil {
		t.Fatalf("fresh entry was pruned: %v", err)
	}
	var got map[string]string
	if !c.Get("fresh", &got) || got["v"] != "fresh" {
		t.Fatalf("fresh entry unreadable after prune: %v", got)
	}
}

func TestSetPruneLeavesOtherFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	c := New(dir, time.Hour)

	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{keep, sub} {
		if err := os.Chtimes(p, stale, stale); err != nil {
			t.Fatal(err)
		}
	}

	c.Set("k", map[string]string{"v": "v"})

	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unrelated file was removed: %v", err)
	}
	if info, err := os.Stat(sub); err != nil || !info.IsDir() {
		t.Fatalf("directory was removed: %v", err)
	}
}
