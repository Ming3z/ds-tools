// Package cache is a tiny on-disk TTL cache for API responses.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

type entry struct {
	At    time.Time       `json:"at"`
	Value json.RawMessage `json:"value"`
}

// Cache stores one JSON file per key under Dir.
type Cache struct {
	Dir string
	TTL time.Duration
}

// New returns a cache rooted at dir. A non-positive TTL disables reads and writes.
func New(dir string, ttl time.Duration) *Cache {
	return &Cache{Dir: dir, TTL: ttl}
}

// Enabled reports whether the cache is usable.
func (c *Cache) Enabled() bool { return c != nil && c.TTL > 0 && c.Dir != "" }

func (c *Cache) file(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Get decodes a cached value into out. It returns false on miss or expiry.
func (c *Cache) Get(key string, out any) bool {
	if !c.Enabled() {
		return false
	}
	b, err := os.ReadFile(c.file(key))
	if err != nil {
		return false
	}
	var e entry
	if err := json.Unmarshal(b, &e); err != nil {
		return false
	}
	if time.Since(e.At) > c.TTL {
		return false
	}
	return json.Unmarshal(e.Value, out) == nil
}

// Set stores a value. Failures are silent: the cache is best-effort.
func (c *Cache) Set(key string, v any) {
	if !c.Enabled() {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return
	}
	b, err := json.Marshal(entry{At: time.Now(), Value: raw})
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.Dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(c.Dir, ".tmp-*.json")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return
	}
	_ = os.Rename(name, c.file(key))
}

// Clear removes every cached entry and reports how many files and bytes went away.
func (c *Cache) Clear() (files int, bytes int64, err error) {
	if c == nil || c.Dir == "" {
		return 0, 0, nil
	}
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, err
	}
	for _, e := range ents {
		p := filepath.Join(c.Dir, e.Name())
		if info, statErr := os.Stat(p); statErr == nil && !info.IsDir() {
			bytes += info.Size()
		}
		if rmErr := os.RemoveAll(p); rmErr != nil {
			err = rmErr
			continue
		}
		files++
	}
	return files, bytes, err
}

// Stats reports the number of entries and their total size in bytes.
func (c *Cache) Stats() (files int, bytes int64) {
	if c == nil || c.Dir == "" {
		return 0, 0
	}
	ents, err := os.ReadDir(c.Dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if info, statErr := e.Info(); statErr == nil {
			files++
			bytes += info.Size()
		}
	}
	return files, bytes
}
