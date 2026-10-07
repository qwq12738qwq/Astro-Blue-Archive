package media

import (
	"container/list"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// ---------------------------------------------------------------------------
// On-disk representation cache
// ---------------------------------------------------------------------------

// DiskCache stores converted representations under a root that is separate from
// MEDIA_ROOT.
//
// ARCHITECTURE.md §82: the two must not share a directory. A `.webp` written beside
// `photo.jpg` is indistinguishable from an upload, so a restore from backup would
// quietly promote a cache entry into the source of truth, and a directory listing
// would show derived bytes as if they were assets.
//
// Entries are named by content key, so replacing an original produces a different
// key and the stale entry is simply never read again (ARCHITECTURE.md §20). Nothing
// has to notice that the file changed.
type DiskCache struct {
	root string
}

// NewDiskCache returns a cache rooted at dir. The directory is created lazily, so a
// deployment that never requests a conversion never writes anything.
func NewDiskCache(dir string) *DiskCache { return &DiskCache{root: dir} }

// path shards by the first two hex characters so no directory accumulates one
// entry per image.
func (d *DiskCache) path(key string) (string, error) {
	// The key is a hex digest this package produced, and that is enforced rather
	// than assumed. A key containing a separator would still stay under the root
	// (filepath.Join neutralises it), but it would create subdirectories named after
	// a caller-supplied string, which is a filesystem layout nobody chose.
	if !isCacheKey(key) {
		return "", errors.New("media: refusing to use a cache key that is not a digest")
	}
	return filepath.Join(d.root, key[:2], key[2:4], key+".webp"), nil
}

// isCacheKey reports whether s is a lowercase hex digest of CacheKey's shape.
func isCacheKey(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return false
	}
	return true
}

// Get reads a cached representation.
func (d *DiskCache) Get(key string) ([]byte, error) {
	if d.root == "" {
		return nil, ErrNoRepresentation
	}
	if !isCacheKey(key) {
		// A miss, not an error: an unusable key simply has nothing stored.
		return nil, ErrNoRepresentation
	}
	// The containment check is repeated even though the key is a hex digest this
	// package produced. It is two comparisons, and it is the last line between a
	// bug here and an arbitrary filesystem read (ARCHITECTURE.md §20).
	target, err := d.path(key)
	if err != nil {
		return nil, err
	}
	full, err := contained(d.root, target)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(full)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNoRepresentation
		}
		return nil, err
	}
	return body, nil
}

// Put writes a representation atomically, so a reader never sees a partial file.
func (d *DiskCache) Put(key string, body []byte) error {
	if d.root == "" {
		return errors.New("image: no cache root configured")
	}
	target, err := d.path(key)
	if err != nil {
		return err
	}
	full, err := contained(d.root, target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return err
	}
	// Digest-named and write-once, so a temp name in the same directory is enough:
	// rename(2) within a directory is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(full), ".webp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(name)
	}()
	if _, err := tmp.Write(body); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o644); err != nil {
		return err
	}
	return os.Rename(name, full)
}

// Clear removes every cached representation.
//
// It never touches MEDIA_ROOT: the originals are the source of truth and this
// directory holds only derived bytes (ARCHITECTURE.md §83).
func (d *DiskCache) Clear() error {
	if d.root == "" {
		return nil
	}
	entries, err := os.ReadDir(d.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(d.root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Usage counts cached files and their total size, for the diagnostics screen.
func (d *DiskCache) Usage() (entries int, bytes int64) {
	if d.root == "" {
		return 0, 0
	}
	_ = filepath.WalkDir(d.root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A cache directory that vanished mid-walk is not an error worth
			// failing a diagnostics read over.
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		entries++
		bytes += info.Size()
		return nil
	})
	return entries, bytes
}

// contained resolves candidate under root and refuses anything that escapes it.
func contained(root, candidate string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absRoot = filepath.Clean(absRoot)
	full := filepath.Clean(candidate)
	rel, err := filepath.Rel(absRoot, full)
	if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", errors.New("media: refusing to use a path outside the cache root")
	}
	return full, nil
}

// ---------------------------------------------------------------------------
// In-memory representation cache
// ---------------------------------------------------------------------------

// MemoryCache is a bounded LRU of converted representations.
//
// ARCHITECTURE.md §23: the bound is the whole point. An unbounded map of decoded
// images is a memory leak with a long fuse — every distinct request that missed the
// disk cache would add an entry and nothing would ever remove one, so the process
// would be OOM-killed by ordinary traffic rather than by an attack.
type MemoryCache struct {
	mu      sync.Mutex
	limit   int64
	used    int64
	order   *list.List               // front = most recently used
	entries map[string]*list.Element // key -> element holding *memEntry
	evicted int64
}

type memEntry struct {
	key  string
	body []byte
}

// NewMemoryCache builds a cache with a byte ceiling. A non-positive ceiling
// disables caching rather than unbounded caching.
func NewMemoryCache(limitBytes int64) *MemoryCache {
	return &MemoryCache{
		limit:   max64(limitBytes, 0),
		order:   list.New(),
		entries: make(map[string]*list.Element),
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Configure changes the ceiling in place.
//
// Shrinking evicts from the least-recently-used end until the cache fits. It runs
// while holding the lock so a concurrent Put cannot observe the new limit with the
// old contents.
func (c *MemoryCache) Configure(limitBytes int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.limit = max64(limitBytes, 0)
	if c.limit == 0 {
		c.evicted += int64(c.entriesLenLocked())
		c.resetLocked()
		return
	}
	c.evictLocked()
}

// Get returns a cached representation and marks it most-recently used.
func (c *MemoryCache) Get(key string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(element)
	return element.Value.(*memEntry).body, true
}

// Put stores a representation, evicting until it fits.
//
// An entry larger than the whole budget is not cached at all. Storing it would
// evict everything else and then be evicted itself, which turns a large image into
// a cache-wide flush on every request.
func (c *MemoryCache) Put(key string, body []byte) {
	size := int64(len(body))
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.limit == 0 || size > c.limit {
		return
	}
	if existing, ok := c.entries[key]; ok {
		entry := existing.Value.(*memEntry)
		c.used += size - int64(len(entry.body))
		entry.body = body
		c.order.MoveToFront(existing)
		c.evictLocked()
		return
	}
	c.entries[key] = c.order.PushFront(&memEntry{key: key, body: body})
	c.used += size
	c.evictLocked()
}

func (c *MemoryCache) evictLocked() {
	for c.used > c.limit {
		oldest := c.order.Back()
		if oldest == nil {
			return
		}
		c.order.Remove(oldest)
		entry := oldest.Value.(*memEntry)
		delete(c.entries, entry.key)
		c.used -= int64(len(entry.body))
		c.evicted++
	}
}

func (c *MemoryCache) entriesLenLocked() int { return len(c.entries) }

func (c *MemoryCache) resetLocked() {
	c.order.Init()
	c.entries = make(map[string]*list.Element)
	c.used = 0
}

// Clear empties the cache.
func (c *MemoryCache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resetLocked()
}

// Limit is the configured ceiling in bytes.
func (c *MemoryCache) Limit() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

// Used is the number of bytes currently held.
func (c *MemoryCache) Used() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// Len is the number of entries.
func (c *MemoryCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// Evictions is a running total, for the diagnostics screen.
func (c *MemoryCache) Evictions() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evicted
}
