package media

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// -----------------------------------------------------------------------------
// §17 Accept negotiation
// -----------------------------------------------------------------------------

func TestAcceptsWebP(t *testing.T) {
	cases := []struct {
		header string
		want   bool
		why    string
	}{
		{"image/webp", true, "the exact type"},
		{"image/webp,image/apng,*/*;q=0.8", true, "a realistic browser header"},
		{"*/*", true, "anything goes"},
		{"image/*", true, "any image"},
		{"text/html,application/xhtml+xml,image/webp,*/*;q=0.8", true, "after HTML types"},
		{"image/jpeg,image/png", false, "no WebP anywhere"},
		{"image/webp;q=0,image/jpeg", false, "q=0 is an explicit refusal"},
		{"image/webp;q=0.0, */*;q=0.1", true, "*/* still accepts it"},
		{"*/*;q=0", false, "a global refusal"},
		{"", false, "no preference expressed"},
		{"   ", false, "whitespace is not a preference"},
		{"text/html", false, "an unrelated type"},
		{"image/webp;q=0.9,image/jpeg;q=0.95", true, "the WebP is acceptable at any weight"},
	}

	for _, c := range cases {
		t.Run(c.why, func(t *testing.T) {
			if got := AcceptsWebP(c.header); got != c.want {
				t.Errorf("AcceptsWebP(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// §23 / §25 the memory cache is bounded
// -----------------------------------------------------------------------------

func TestMemoryCacheStaysInsideItsCeiling(t *testing.T) {
	// A 4 KiB ceiling with 1 KiB entries, so eviction has to happen on every put
	// past the fourth entry. An unbounded implementation would hold all of them.
	const entry = 1024
	c := NewMemoryCache(4 * entry)

	for i := 0; i < 50; i++ {
		c.Put(fmt.Sprintf("key-%d", i), bytes.Repeat([]byte{byte(i)}, entry))
		if used := c.Used(); used > c.Limit() {
			t.Fatalf("after %d puts: used %d exceeds the %d ceiling", i+1, used, c.Limit())
		}
	}

	if got := c.Len(); got > 4 {
		t.Errorf("held %d entries, want at most 4", got)
	}
	if c.Evictions() == 0 {
		t.Error("nothing was evicted, so the bound was never exercised")
	}
	if c.Used() > c.Limit() {
		t.Errorf("used %d bytes, ceiling %d", c.Used(), c.Limit())
	}
}

func TestMemoryCacheEvictsLeastRecentlyUsed(t *testing.T) {
	c := NewMemoryCache(3 * 10)
	c.Put("a", bytes.Repeat([]byte{1}, 10))
	c.Put("b", bytes.Repeat([]byte{2}, 10))
	c.Put("c", bytes.Repeat([]byte{3}, 10))

	// Touch a and c so b becomes the least recently used.
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should be cached")
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c should be cached")
	}
	c.Put("d", bytes.Repeat([]byte{4}, 10))

	if _, ok := c.Get("b"); ok {
		t.Error("b was least recently used and should have been evicted")
	}
	for _, key := range []string{"a", "c", "d"} {
		if _, ok := c.Get(key); !ok {
			t.Errorf("%s should still be cached", key)
		}
	}
}

func TestMemoryCacheRefusesAnEntryLargerThanItself(t *testing.T) {
	c := NewMemoryCache(100)
	c.Put("small", bytes.Repeat([]byte{1}, 10))

	c.Put("huge", bytes.Repeat([]byte{2}, 101))

	if _, ok := c.Get("huge"); ok {
		t.Error("an entry bigger than the whole budget must not be cached")
	}
	if _, ok := c.Get("small"); ok != true {
		t.Error("and it must not have flushed everything else on its way")
	}
	if c.Used() > c.Limit() {
		t.Errorf("used %d bytes with a %d ceiling", c.Used(), c.Limit())
	}
}

func TestMemoryCacheConfigureShrinksLive(t *testing.T) {
	c := NewMemoryCache(10 * 1024)
	for i := 0; i < 10; i++ {
		c.Put(fmt.Sprintf("key-%d", i), bytes.Repeat([]byte{byte(i)}, 1024))
	}
	if c.Used() == 0 {
		t.Fatal("nothing cached")
	}

	c.Configure(2 * 1024)

	if c.Limit() != 2*1024 {
		t.Errorf("limit is %d, want %d", c.Limit(), 2*1024)
	}
	if c.Used() > c.Limit() {
		t.Errorf("used %d bytes after shrinking to %d", c.Used(), c.Limit())
	}
}

func TestMemoryCacheZeroCeilingDisablesIt(t *testing.T) {
	c := NewMemoryCache(0)
	c.Put("a", bytes.Repeat([]byte{1}, 1024))
	if c.Len() != 0 {
		t.Error("a zero ceiling must cache nothing rather than cache everything")
	}
}

// -----------------------------------------------------------------------------
// §20 the cache key follows content and parameters
// -----------------------------------------------------------------------------

func TestCacheKeyVariesWithQualityAndChecksum(t *testing.T) {
	base := CacheKey("abc123", 82)
	if base != CacheKey("abc123", 82) {
		t.Error("the same inputs must produce the same key")
	}
	if base == CacheKey("abc124", 82) {
		t.Error("a different checksum must produce a different key")
	}
	if base == CacheKey("abc123", 81) {
		t.Error("a different quality must produce a different key")
	}
	for _, key := range []string{base, CacheKey("abc123", 81), CacheKey("abc124", 82)} {
		if strings.ContainsAny(key, "/.\\") {
			t.Errorf("key %q must be a bare digest usable as a filename", key)
		}
	}
}

// -----------------------------------------------------------------------------
// §19 / §21 / §82 the disk cache
// -----------------------------------------------------------------------------

func TestDiskCacheRoundTripAndClear(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media-cache")
	d := NewDiskCache(root)

	if _, err := d.Get("missing"); err == nil {
		t.Error("a cold read must report a miss, not an empty body")
	}

	key := CacheKey("0123456789abcdef", 82)
	body := []byte("RIFF____WEBPnotreally")
	if err := d.Put(key, body); err != nil {
		t.Fatalf("put: %v", err)
	}

	got, err := d.Get(key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round trip changed the bytes")
	}

	entries, size := d.Usage()
	if entries != 1 || size != int64(len(body)) {
		t.Errorf("usage reported %d entries / %d bytes", entries, size)
	}

	// The cache must not have written anything into the media root: it has its own
	// directory, so a restore from backup cannot promote a cache entry into the
	// source of truth (§82).
	entriesOnDisk, _ := os.ReadDir(root)
	if len(entriesOnDisk) == 0 {
		t.Error("expected a sharded cache directory")
	}

	if err := d.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := d.Get(key); err == nil {
		t.Error("a cleared entry must be a miss, not a stale hit")
	}
}

func TestDiskCacheRefusesAnEscapingKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	d := NewDiskCache(root)

	// The key is always produced by CacheKey, but the containment check is repeated
	// rather than assumed: it is two comparisons and the last line against an
	// arbitrary filesystem read (§20).
	for _, key := range []string{"../escape", "../../etc/passwd", "/etc/passwd", "aa11", "AA11BB"} {
		if _, err := d.Get(key); err == nil {
			t.Errorf("key %q should not have been served", key)
		}
		if err := d.Put(key, []byte("x")); err == nil {
			t.Errorf("key %q should not have been written", key)
		}
	}

	// A real digest still works, and it still lands under the cache root.
	if err := d.Put(CacheKey("abc", 82), []byte("x")); err != nil {
		t.Errorf("a real digest should be accepted: %v", err)
	}
}

// -----------------------------------------------------------------------------
// §59 single-flight
// -----------------------------------------------------------------------------

func TestSingleGroupCollapsesConcurrentWork(t *testing.T) {
	var g singleGroup
	var (
		mu    sync.Mutex
		calls int
	)
	release := make(chan struct{})

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, _ = g.Do("same-key", func() ([]byte, error) {
				mu.Lock()
				calls++
				mu.Unlock()
				<-release
				return []byte("result"), nil
			})
		}()
	}

	// Give the goroutines time to pile up on the same key, then let the one winner
	// finish.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("the work ran %d times, want exactly 1", calls)
	}
}

func TestSingleGroupSharesTheError(t *testing.T) {
	var g singleGroup
	want := fmt.Errorf("encode failed")

	release := make(chan struct{})
	const workers = 10
	var wg sync.WaitGroup
	wg.Add(workers)
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = g.Do("k", func() ([]byte, error) {
				<-release
				return nil, want
			})
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != want {
			t.Errorf("worker %d got %v, want the shared error", i, err)
		}
	}
}

// -----------------------------------------------------------------------------
// §26 pixel ceilings
// -----------------------------------------------------------------------------

// widePNG builds a valid PNG of the given size without allocating the pixels twice.
func widePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	m := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := range m.Pix {
		m.Pix[i] = 120
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, m); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func TestCheckPixelsRefusesAnOversizedImage(t *testing.T) {
	dir := t.TempDir()
	// 200x200 = 40,000 pixels.
	small := filepath.Join(dir, "small.png")
	if err := os.WriteFile(small, widePNG(t, 200, 200), 0o644); err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(small)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := CheckPixels(f, 50_000); err != nil {
		t.Errorf("a 40,000 pixel image should pass a 50,000 ceiling: %v", err)
	}
}

func TestCheckPixelsNamesTheCeiling(t *testing.T) {
	dir := t.TempDir()
	// A flat 400x400 PNG is a few kilobytes and 160,000 pixels: the shape of a
	// decompression bomb, and cheap to build in a test.
	path := filepath.Join(dir, "bomb.png")
	if err := os.WriteFile(path, widePNG(t, 400, 400), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	err = CheckPixels(f, 10_000)
	if err == nil {
		t.Fatal("a 160,000 pixel image should fail a 10,000 ceiling")
	}
	if !strings.Contains(err.Error(), "pixels") {
		t.Errorf("the error should name the ceiling, got %q", err)
	}
	t.Logf("the bomb is %d bytes on disk and was refused: %v", info.Size(), err)
}

func TestCheckPixelsRejectsSomethingThatDoesNotDecode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fake.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\nnot really a png"), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := CheckPixels(f, 1_000_000); err == nil {
		t.Error("a file that claims to be a PNG but does not decode must be refused")
	}
}

// -----------------------------------------------------------------------------
// §12 / §80 the media URL contract
// -----------------------------------------------------------------------------

func TestPublicURLAndPathRoundTrip(t *testing.T) {
	const rel = "2026/10/0123456789abcdef01234567.jpg"
	url := PublicURL(rel)
	if url != "/media/"+rel {
		t.Errorf("PublicURL = %q", url)
	}
	if got := PublicPath(url); got != rel {
		t.Errorf("PublicPath(%q) = %q, want %q", url, got, rel)
	}
	if got := PublicPath("https://example.com" + url); got != rel {
		t.Errorf("an absolute URL should resolve too, got %q", got)
	}
	if got := PublicPath("https://example.com/elsewhere/x.jpg"); got != "" {
		t.Errorf("a foreign path must not resolve, got %q", got)
	}
}

func TestNormaliseMediaPathRejectsAnythingNotOurs(t *testing.T) {
	for _, bad := range []string{
		"../../etc/passwd",
		"/etc/passwd",
		"2026/10/short.jpg",
		"2026/10/0123456789ABCDEF01234567.jpg",
		"2026/10/0123456789abcdef01234567.svg",
		"not-a-path.jpg",
		"",
	} {
		if got := NormaliseMediaPath(bad); got != "" {
			t.Errorf("NormaliseMediaPath(%q) = %q, want \"\"", bad, got)
		}
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("0123456789abcdef01234567") {
		t.Error("a 24-character lowercase hex id is valid")
	}
	for _, bad := range []string{
		"short",
		"0123456789ABCDEF01234567",
		"../../etc/passwd0123456",
		"0123456789abcdef012345678",
	} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) should be false", bad)
		}
	}
}

// -----------------------------------------------------------------------------
// §50 / §52 the usage scanner
// -----------------------------------------------------------------------------

func TestExtractMediaPaths(t *testing.T) {
	const rel = "2026/10/0123456789abcdef01234567.jpg"
	const other = "2026/10/fedcba987654321001234567.png"

	doc := strings.Join([]string{
		"---",
		"title: With images",
		"cover: /media/" + rel,
		"---",
		"",
		"![inline](/media/" + rel + ")",
		"",
		`<img src="/media/` + rel + `" alt="raw html">`,
		"",
		"Absolute too: https://example.com/media/" + other,
		"",
		"Not ours: /media/../etc/passwd and /media/nope.jpg",
	}, "\n")

	got := ExtractMediaPaths(doc)

	// The image appears in the frontmatter, in Markdown image syntax and in a raw
	// <img>, so it is counted three times; the other appears once.
	counts := map[string]int{}
	for _, p := range got {
		counts[p]++
	}
	if counts[rel] != 3 {
		t.Errorf("%q was counted %d times, want 3", rel, counts[rel])
	}
	if counts[other] != 1 {
		t.Errorf("%q was counted %d times, want 1", other, counts[other])
	}
	if len(counts) != 2 {
		t.Errorf("extracted %v, want only the two stored paths", got)
	}
	for _, p := range got {
		if !strings.HasPrefix(p, "2026/10/") {
			t.Errorf("extracted a path outside the storage layout: %q", p)
		}
	}
}

func TestScanUsageReadsBothCollections(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"posts", "pages"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	write := func(rel, name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, rel, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	const a = "2026/10/0123456789abcdef01234567.jpg"
	const b = "2026/10/111111111111111111111111.png"
	const c = "2026/10/222222222222222222222222.webp"

	write("posts", "one.md", "---\ntitle: One\n---\n\n![x](/media/"+a+") and again ![y](/media/"+a+")\n")
	write("posts", "two.md", "---\ntitle: Two\ncover: /media/"+b+"\n---\n")
	write("pages", "about.md", "---\ntitle: About\n---\n\n![z](/media/"+c+")\n")
	// Not Markdown, and therefore not content.
	write("posts", "notes.txt", "/media/"+a)

	refs, err := ScanUsage(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	index := map[string]UsageRef{}
	for _, r := range refs {
		index[r.MediaPath+"|"+r.Kind+"|"+r.Slug] = r
	}

	if got := index[a+"|post|one"]; got.ReferenceCount != 2 {
		t.Errorf("two references in one post counted as %d", got.ReferenceCount)
	}
	if _, ok := index[b+"|post|two"]; !ok {
		t.Error("a frontmatter cover is a reference")
	}
	if _, ok := index[c+"|page|about"]; !ok {
		t.Error("a page reference was not recorded")
	}
	if _, ok := index[a+"|post|notes"]; ok {
		t.Error("a non-Markdown file must not be scanned")
	}
}

func TestScanUsageSurvivesAnUnreadableFile(t *testing.T) {
	root := t.TempDir()
	posts := filepath.Join(root, "posts")
	if err := os.MkdirAll(posts, 0o755); err != nil {
		t.Fatal(err)
	}
	const rel = "2026/10/0123456789abcdef01234567.jpg"
	good := filepath.Join(posts, "good.md")
	if err := os.WriteFile(good, []byte("![a](/media/"+rel+")"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory named like a file: os.ReadFile fails on it, and the scan must skip
	// it rather than abandon the whole directory. Losing one file must not make every
	// image look unused, which is how a media library loses data.
	if err := os.MkdirAll(filepath.Join(posts, "broken.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	refs, err := ScanUsage(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(refs) != 1 || refs[0].MediaPath != rel {
		t.Errorf("scan returned %v, want the one readable reference", refs)
	}
}

func TestScanUsageToleratesAMissingDirectory(t *testing.T) {
	refs, err := ScanUsage(filepath.Join(t.TempDir(), "nothing-here"))
	if err != nil {
		t.Fatalf("a missing content root must not be an error: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("expected no references, got %v", refs)
	}
}

// -----------------------------------------------------------------------------
// §19 upload validation still holds after the pixel check
// -----------------------------------------------------------------------------

func TestSaveRejectsASVG(t *testing.T) {
	store := NewStore(t.TempDir())
	svg := `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`

	_, err := store.Save("image/svg+xml", "x.svg", nil, strings.NewReader(svg), 1<<20, 0)
	if err == nil {
		t.Fatal("an SVG upload must be refused")
	}
}

func TestSaveRefusesAnOversizedPixelCount(t *testing.T) {
	store := NewStore(t.TempDir())
	png := widePNG(t, 400, 400)

	_, err := store.Save("image/png", "big.png", nil, bytes.NewReader(png), 5<<20, 10_000)
	if err == nil {
		t.Fatal("a 160,000 pixel image must fail a 10,000 ceiling")
	}
	if !strings.Contains(err.Error(), "pixels") {
		t.Errorf("the error should name the pixel ceiling, got %v", err)
	}
}

func TestSaveKeepsTheOriginalNameOnlyAsALabel(t *testing.T) {
	store := NewStore(t.TempDir())
	png := widePNG(t, 8, 8)

	res, err := store.Save("image/png", "../../etc/passwd.png", nil, bytes.NewReader(png), 1<<20, 1_000_000)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if strings.Contains(res.Path, "..") || strings.Contains(res.Filename, "..") {
		t.Errorf("the client's name leaked into the path or the label: %+v", res)
	}
	if !strings.HasPrefix(res.Path, time.Now().Format("2006")+"/") {
		t.Errorf("path %q is not YYYY/MM/<id>.png", res.Path)
	}
	if res.Width != 8 || res.Height != 8 {
		t.Errorf("dimensions are %dx%d, want 8x8", res.Width, res.Height)
	}
	if len(res.SHA256) != 32 {
		t.Errorf("expected a 32-byte SHA-256, got %d bytes", len(res.SHA256))
	}
}

func TestSaveDerivesTheExtensionFromTheBytes(t *testing.T) {
	store := NewStore(t.TempDir())

	// A real JPEG with a misleading filename and a misleading declared type.
	m := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range m.Pix {
		m.Pix[i] = 200
	}
	var jpg bytes.Buffer
	if err := jpeg.Encode(&jpg, m, nil); err != nil {
		t.Fatal(err)
	}

	res, err := store.Save("image/png", "photo.png", nil, bytes.NewReader(jpg.Bytes()), 1<<20, 1_000_000)
	if err == nil {
		t.Fatal("a declared type that contradicts the bytes must be refused")
	}
	if res != nil {
		t.Errorf("nothing should have been stored: %+v", res)
	}
}

// -----------------------------------------------------------------------------
// §3 GIF animation transcoding is a non-goal
// -----------------------------------------------------------------------------

// A GIF is served as stored.
//
// `image.Decode` on a GIF returns its first frame, so a pipeline that converted one
// would hand every WebP-capable visitor a still image that used to move: a silent
// regression with no error, no failed request and no visible warning — the kind that
// only surfaces if somebody uploads an animated icon. ARCHITECTURE.md §3 lists GIF
// animation transcoding as a non-goal, and the cheapest way to honour a non-goal is to
// decline the conversion rather than to ship an encoder that quietly does the wrong
// thing.
func TestWebPRefusesToFlattenAnAnimatedGIF(t *testing.T) {
	p := NewPipeline(nil, nil, filepath.Join(t.TempDir(), "media-cache"), ImageConfig{})

	var buf bytes.Buffer
	anim := &gif.GIF{}
	for _, phase := range []uint8{0, 1} {
		m := image.NewPaletted(image.Rect(0, 0, 4, 4), color.Palette{color.Black, color.White})
		for i := range m.Pix {
			m.Pix[i] = phase
		}
		anim.Image = append(anim.Image, m)
		anim.Delay = append(anim.Delay, 10)
	}
	if err := gif.EncodeAll(&buf, anim); err != nil {
		t.Fatal(err)
	}
	if len(buf.Bytes()) == 0 {
		t.Fatal("the fixture GIF is empty, so this test would pass for the wrong reason")
	}

	rec := MediaRecord{MIME: "image/gif", SHA256: []byte(strings.Repeat("ab", 32))}
	if _, err := p.WebP(context.Background(), "2026/10/anim.gif", rec); !errors.Is(err, ErrNotWebP) {
		t.Fatalf("a GIF must be served as stored rather than flattened, got %v", err)
	}

	// Nothing may have been written for it: the refusal happens before the cache key
	// is even computed, so a rejected conversion leaves no trace to clean up later.
	if _, err := p.disk.Get(CacheKey(rec.Checksum(), 82)); !errors.Is(err, ErrNoRepresentation) {
		t.Errorf("a refused GIF must not populate the cache, got %v", err)
	}
}

// A WebP is likewise not re-encoded. §81 keeps one URL per asset, and re-encoding a
// lossy format to itself only loses quality while doubling the cache.
func TestWebPRefusesToReencodeWebP(t *testing.T) {
	p := NewPipeline(nil, nil, filepath.Join(t.TempDir(), "media-cache"), ImageConfig{})

	rec := MediaRecord{MIME: "image/webp", SHA256: []byte(strings.Repeat("cd", 32))}
	if _, err := p.WebP(context.Background(), "2026/10/a.webp", rec); !errors.Is(err, ErrNotWebP) {
		t.Fatalf("a WebP must be served as stored, got %v", err)
	}
}

// The two refusals are distinguished from a failure, because the caller treats them
// differently: ErrNotWebP means "serve the original", anything else is logged and
// counted. If a future change collapsed the two, a GIF would be reported as a
// conversion error on every request.
func TestGIFRefusalIsNotAConversionFailure(t *testing.T) {
	p := NewPipeline(nil, nil, filepath.Join(t.TempDir(), "media-cache"), ImageConfig{})

	_, err := p.WebP(context.Background(), "2026/10/anim.gif", MediaRecord{MIME: "image/gif"})
	if err == nil {
		t.Fatal("a GIF has no stored bytes here, so the call must still be refused up front")
	}
	if !errors.Is(err, ErrNotWebP) {
		t.Fatalf("got %v, want ErrNotWebP", err)
	}
}
