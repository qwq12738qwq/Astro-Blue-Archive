// Image representation negotiation, transformation and caching.
//
// ARCHITECTURE.md §26–§32: WebP is a *delivery* representation, never a storage
// replacement. `MEDIA_ROOT` holds originals and nothing else, and every URL in
// Markdown keeps pointing at the original extension. When a browser accepts WebP
// the delivery layer serves a converted representation of the same file; when it
// does not, it serves the original bytes.
//
// ARCHITECTURE.md §26: this file is where a decoded pixel count becomes a memory
// bound. A 5 MB JPEG can decode to 100 megapixels, and an encoder allocates
// several bytes per pixel, so the byte cap alone does not keep the process alive.
package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	webp "github.com/mayahiro/go-webp"

	// Decoders. image/jpeg, image/png and image/gif come from the standard library;
	// WebP needs x/image. Registering them here is what makes image.Decode work on
	// an upload, which is how width/height were obtained for every other format and
	// how a WebP upload finally gets dimensions too.
	_ "golang.org/x/image/webp"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
)

// DefaultQuality is the WebP encoder quality used until an admin sets one.
const DefaultQuality = 82

// Quality bounds. Anything outside is a mistake worth surfacing rather than
// clamping silently, but the pipeline itself must never encode at a nonsense
// quality if it is handed one.
const (
	QualityMin = 1
	QualityMax = 100
)

// ErrTooManyPixels is returned when an image decodes to more pixels than the
// configured ceiling. It is a distinct error because the remedy is different: a
// too-large *file* is retried smaller, while a too-large *image* has to be cropped
// or resized before it can be served at all.
var ErrTooManyPixels = errors.New("image has too many pixels")

// ErrNotWebP reports that no WebP representation should be produced for this source,
// so the original must be served unchanged.
//
// Two sources land here. The obvious one is an image that already *is* WebP: there is
// nothing to convert, and a re-encode would only lose quality. The other is a GIF, and
// that one is a decision rather than a technicality. `image.Decode` on a GIF returns
// the *first frame*, so converting one to WebP would silently throw away every
// subsequent frame and hand the visitor a still image that used to move.
// ARCHITECTURE.md §3 lists GIF animation transcoding as a non-goal, and the cheapest
// way to honour a non-goal is to decline the conversion instead of shipping an encoder
// that quietly does the wrong thing. A browser that takes WebP gets the GIF.
//
// The distinction is a distinct sentinel rather than a bool on MediaRecord because the
// delivery layer has to be able to tell "there is no conversion to do" from "the
// conversion failed", and both mean the same thing to the caller: serve the original.
var ErrNotWebP = errors.New("image is already WebP or must be served as stored")

// ErrNoRepresentation is returned when no cached representation exists.
var ErrNoRepresentation = errors.New("no cached representation")

// ErrNoIndex reports that a file exists on disk with no media row behind it.
//
// ARCHITECTURE.md §5: the filesystem is the source of truth, so the file is still
// servable. Without a recorded checksum there is nothing to key a representation
// on, so it is served as the original.
var ErrNoIndex = errors.New("no media index entry")

// RepoFunc adapts a function to Repo, so a caller does not need a named type.
type RepoFunc func(ctx context.Context, rel string) (MediaRecord, error)

// MediaRecord implements Repo.
func (f RepoFunc) MediaRecord(ctx context.Context, rel string) (MediaRecord, error) {
	return f(ctx, rel)
}

// ImageConfig is the live image-pipeline configuration.
//
// ARCHITECTURE.md §84: these are admin settings, and they are applied by
// re-configuring the running pipeline rather than by requiring a restart. A value
// that only took effect on the next deploy would be a setting the admin changed and
// did not get.
type ImageConfig struct {
	// Quality is the WebP encoder quality, 1–100.
	Quality int
	// MemoryCacheBytes bounds the in-memory representation cache.
	MemoryCacheBytes int64
	// MaxPixels bounds a decoded image. Zero means "no ceiling".
	MaxPixels int64
}

// Root is the read side of the media store that the pipeline needs.
//
// An interface rather than a *Store so that the pipeline's dependency is "I can
// read a stored file" and not "I know how files are laid out".
type Root interface {
	Open(rel string) (*os.File, error)
	Stat(rel string) (os.FileInfo, error)
}

// Repo is the indexed metadata side.
//
// It is an interface rather than *store.DB so this package never learns about
// SQLite. The delivery layer needs three facts about a file — its checksum, its
// stored MIME type and whether it is still on disk — and a media row is metadata,
// never content (ARCHITECTURE.md §36).
type Repo interface {
	// MediaRecord looks a file up by its database-relative path. A file that exists
	// on disk without a row is still servable: the filesystem is the source of
	// truth (ARCHITECTURE.md §5).
	MediaRecord(ctx context.Context, rel string) (MediaRecord, error)
}

// MediaRecord is the indexed metadata of one stored file.
type MediaRecord struct {
	ID   string
	Rel  string
	MIME string
	Size int64
	// SHA256 of the stored file. Empty when there is no row, or when the file
	// cannot be read.
	SHA256 []byte
	// Missing is true when a row exists but the file is not on disk.
	Missing bool
}

// Checksum is the cache-key input: the file's content identity.
func (m MediaRecord) Checksum() string {
	if len(m.SHA256) == 0 {
		return ""
	}
	return hex.EncodeToString(m.SHA256)
}

// Pipeline owns conversion and both caches.
type Pipeline struct {
	root  Root
	repo  Repo
	disk  *DiskCache
	mem   *MemoryCache
	group singleGroup

	mu  sync.RWMutex
	cfg ImageConfig

	hits, misses, evictions, conversions, failures atomic.Int64
}

// NewPipeline builds a pipeline over a media root and its metadata index.
func NewPipeline(root Root, repo Repo, diskRoot string, cfg ImageConfig) *Pipeline {
	return &Pipeline{
		root: root,
		repo: repo,
		disk: NewDiskCache(diskRoot),
		mem:  NewMemoryCache(cfg.MemoryCacheBytes),
		cfg:  cfg,
	}
}

// Configure applies a new configuration, resizing the memory cache in place.
//
// A shrinking cache is cleared rather than trimmed: the representations it holds
// were produced under a configuration the admin has just moved away from, so
// serving them would be serving something nobody asked for. The disk cache is left
// alone — it is content-addressed, so its entries stay valid regardless.
func (p *Pipeline) Configure(cfg ImageConfig) {
	p.mu.Lock()
	resize := cfg.MemoryCacheBytes != p.cfg.MemoryCacheBytes
	p.cfg = cfg
	p.mu.Unlock()
	if resize {
		p.mem.Configure(cfg.MemoryCacheBytes)
	}
}

func (p *Pipeline) config() ImageConfig {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

// Stats is the diagnostic snapshot.
//
// It deliberately contains no filesystem path (ARCHITECTURE.md §85): the admin
// screen shows sizes, counts and hit rates, not where the cache lives.
type Stats struct {
	MemoryLimitBytes int64 `json:"memoryLimitBytes"`
	MemoryUsedBytes  int64 `json:"memoryUsedBytes"`
	MemoryEntries    int   `json:"memoryEntries"`
	DiskEntries      int   `json:"diskEntries"`
	DiskBytes        int64 `json:"diskBytes"`
	Hits             int64 `json:"hits"`
	Misses           int64 `json:"misses"`
	Evictions        int64 `json:"evictions"`
	Conversions      int64 `json:"conversions"`
	Failures         int64 `json:"failures"`
	Quality          int   `json:"quality"`
	MaxPixels        int64 `json:"maxPixels"`
}

// Stats returns the counters.
//
// The disk walk is bounded by the number of cached files, which is the number of
// distinct (source checksum, quality) pairs. It runs only when an admin opens the
// diagnostics screen.
func (p *Pipeline) Stats() Stats {
	entries, bytes := p.disk.Usage()
	return Stats{
		MemoryLimitBytes: p.mem.Limit(),
		MemoryUsedBytes:  p.mem.Used(),
		MemoryEntries:    p.mem.Len(),
		DiskEntries:      entries,
		DiskBytes:        bytes,
		Hits:             p.hits.Load(),
		Misses:           p.misses.Load(),
		Evictions:        p.evictions.Load(),
		Conversions:      p.conversions.Load(),
		Failures:         p.failures.Load(),
		Quality:          p.config().Quality,
		MaxPixels:        p.config().MaxPixels,
	}
}

// ClearCache empties both caches.
//
// ARCHITECTURE.md §21/§83: the cache is derived. Clearing it costs one conversion
// per image and must never touch MEDIA_ROOT, because the originals are the source
// of truth.
func (p *Pipeline) ClearCache() error {
	p.mem.Clear()
	return p.disk.Clear()
}

// CacheKey identifies one representation.
//
// ARCHITECTURE.md §20: the key is the *content* of the original plus the parameters
// that produced the representation. Keying on the filename alone is the bug this
// avoids — replacing `photo.jpg` in place would keep serving the previous file's
// WebP under a URL that now claims to be the new one.
func CacheKey(checksum string, quality int) string {
	h := sha256.New()
	h.Write([]byte(checksum))
	h.Write([]byte{0})
	h.Write([]byte("webp"))
	h.Write([]byte{0})
	h.Write([]byte(strconv.Itoa(quality)))
	return hex.EncodeToString(h.Sum(nil))
}

// WebP returns the WebP representation of the original at rel.
//
// The order is memory → disk → convert, and the conversion is single-flighted per
// cache key, because ARCHITECTURE.md §59 requires that a burst of requests for a
// cold image produce one conversion and not one per request.
//
// Every failure mode returns an error rather than a zero-length body: the caller
// falls back to the original (ARCHITECTURE.md §60), and a truncated WebP would
// render as a broken image instead.
func (p *Pipeline) WebP(ctx context.Context, rel string, rec MediaRecord) ([]byte, error) {
	// §3: a GIF is served as stored. See ErrNotWebP — converting one would keep only
	// its first frame.
	if rec.MIME == "image/webp" || rec.MIME == "image/gif" {
		return nil, ErrNotWebP
	}

	cfg := p.config()
	checksum := rec.Checksum()
	if checksum == "" {
		return nil, errors.New("image: no checksum, refusing to cache")
	}
	key := CacheKey(checksum, cfg.Quality)

	if body, ok := p.mem.Get(key); ok {
		p.hits.Add(1)
		return body, nil
	}
	p.misses.Add(1)

	if body, err := p.disk.Get(key); err == nil {
		p.hits.Add(1)
		p.store(key, body)
		return body, nil
	} else if !errors.Is(err, ErrNoRepresentation) {
		// A cache read error must not become a 500; fall through to converting.
		slog.Warn("image_cache_read_failed", "error", err)
	}

	body, err := p.group.Do(key, func() ([]byte, error) {
		// Re-check inside the flight. The winner of a race writes the cache, so
		// the losers must read it back rather than encode a second time.
		if cached, err := p.disk.Get(key); err == nil {
			return cached, nil
		}
		encoded, err := p.convert(ctx, rel, cfg)
		if err != nil {
			return nil, err
		}
		p.conversions.Add(1)
		if err := p.disk.Put(key, encoded); err != nil {
			// A full or read-only cache costs CPU on the next request, not
			// correctness, so it is logged rather than fatal.
			slog.Warn("image_cache_write_failed", "error", err)
		}
		return encoded, nil
	})
	if err != nil {
		p.failures.Add(1)
		// The log names the file and the reason. It never carries image bytes
		// (ARCHITECTURE.md §86).
		slog.Warn("image_convert_failed", "path", rel, "error", err)
		return nil, err
	}

	p.store(key, body)
	return body, nil
}

// WebPKey is the cache key for a representation, exposed so an ETag can name the
// exact bytes it covers (ARCHITECTURE.md §89).
func (p *Pipeline) WebPKey(rec MediaRecord) string {
	return CacheKey(rec.Checksum(), p.config().Quality)
}

// Quality is the WebP quality currently in force.
func (p *Pipeline) Quality() int { return p.config().Quality }

func (p *Pipeline) store(key string, body []byte) {
	p.mem.Put(key, body)
	p.evictions.Store(p.mem.Evictions())
}

// convert decodes the original and re-encodes it as lossy WebP.
func (p *Pipeline) convert(ctx context.Context, rel string, cfg ImageConfig) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	f, err := p.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	// The header alone is enough to reject an oversized image before allocating
	// for it: image.DecodeConfig reads a few dozen bytes, where a full decode of a
	// decompression bomb is gigabytes.
	if cfg.MaxPixels > 0 {
		if err := p.checkPixels(f, cfg.MaxPixels); err != nil {
			return nil, err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return nil, fmt.Errorf("rewind original: %w", err)
		}
	}

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, fmt.Errorf("decode original: %w", err)
	}

	quality := cfg.Quality
	if quality < QualityMin || quality > QualityMax {
		quality = DefaultQuality
	}

	var out bytes.Buffer
	// ModeLowMemory keeps the encoder from holding a second, larger candidate
	// buffer beside the decoded image. Conversion runs on its own request
	// goroutine and must not be the reason the process runs out of memory.
	if err := webp.Encode(&out, img, &webp.Options{
		Compression: webp.CompressionLossy,
		Quality:     quality,
		Mode:        webp.ModeLowMemory,
	}); err != nil {
		return nil, fmt.Errorf("encode webp: %w", err)
	}
	if out.Len() == 0 {
		return nil, errors.New("encode webp: produced no output")
	}
	return out.Bytes(), nil
}

// checkPixels rejects an image whose decoded size exceeds the ceiling.
func (p *Pipeline) checkPixels(f *os.File, maxPixels int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind original: %w", err)
	}
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("read image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return fmt.Errorf("image has no usable dimensions (%dx%d)", cfg.Width, cfg.Height)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return fmt.Errorf("%w: %dx%d exceeds the %d pixel limit",
			ErrTooManyPixels, cfg.Width, cfg.Height, maxPixels)
	}
	return nil
}

// CheckPixels is the upload-path form of the same guard, so an image that cannot
// be converted is never accepted in the first place.
func CheckPixels(f *os.File, maxPixels int64) error {
	return (&Pipeline{}).checkPixels(f, maxPixels)
}

// AcceptsWebP reports whether an Accept header permits image/webp.
//
// ARCHITECTURE.md §17: the decision is made from Accept, never from User-Agent.
// Sniffing a browser string is how a delivery layer ends up sending a format the
// client cannot decode, and Accept is the mechanism HTTP provides for exactly this
// question. `image/webp;q=0` is an explicit refusal and is honoured.
func AcceptsWebP(header string) bool {
	if strings.TrimSpace(header) == "" {
		// No Accept at all is not the same as `Accept: */*`: a request that
		// expressed no preference gets the original, which every client can read.
		return false
	}
	best := -1.0
	seen := false
	for _, part := range strings.Split(header, ",") {
		fields := strings.Split(part, ";")
		switch mt := strings.ToLower(strings.TrimSpace(fields[0])); mt {
		case "image/webp", "*/*", "image/*":
			seen = true
		default:
			continue
		}
		q := 1.0
		for _, p := range fields[1:] {
			p = strings.ToLower(strings.TrimSpace(p))
			if !strings.HasPrefix(p, "q=") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(p[2:]), 64)
			if err != nil {
				continue
			}
			q = parsed
		}
		if q > best {
			best = q
		}
	}
	// A q of 0 is a refusal; `seen` guards the header-only-junk case.
	return seen && best > 0
}

// ---------------------------------------------------------------------------
// single-flight
// ---------------------------------------------------------------------------

// singleGroup collapses concurrent work on the same key into one call.
//
// ARCHITECTURE.md §59. Hand-rolled rather than golang.org/x/sync/singleflight: the
// failure being avoided is N concurrent encodes of one file, and that is thirty
// lines rather than a dependency.
type singleGroup struct {
	mu    sync.Mutex
	calls map[string]*singleCall
}

type singleCall struct {
	wg  sync.WaitGroup
	val []byte
	err error
}

// Do runs fn once per key. Every caller gets the same result, including the error,
// so a conversion that fails once fails once rather than N times.
func (g *singleGroup) Do(key string, fn func() ([]byte, error)) ([]byte, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*singleCall)
	}
	if existing, ok := g.calls[key]; ok {
		g.mu.Unlock()
		existing.wg.Wait()
		return existing.val, existing.err
	}
	call := new(singleCall)
	call.wg.Add(1)
	g.calls[key] = call
	g.mu.Unlock()

	call.val, call.err = fn()
	call.wg.Done()

	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return call.val, call.err
}
