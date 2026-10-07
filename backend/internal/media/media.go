// Package media stores uploaded files under MEDIA_ROOT.
//
// ARCHITECTURE.md §19: the database keeps metadata only. The client never
// supplies the final path; the server generates `YYYY/MM/<id>.<ext>` after
// validating the MIME type AND the magic bytes, because an extension is not
// trustworthy input.
//
// ARCHITECTURE.md §20: no user-supplied string ever becomes a path.
package media

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// ErrNotFound is returned when an id has no file.
var ErrNotFound = errors.New("media not found")

// ErrUnsupportedType is returned for a MIME type or magic-byte mismatch.
var ErrUnsupportedType = errors.New("unsupported media type")

// ErrTooLarge is returned when the upload exceeds the configured cap.
var ErrTooLarge = errors.New("file is too large")

// Allowed image types.
//
// SVG, HTML, HTM, JS, MJS, XML, XHTML and CSS are deliberately absent
// (ARCHITECTURE.md §19): an SVG is a script container, and the others could be
// served as active content from our own origin.
var allowed = map[string]struct {
	mime      string
	extension string
	magic     [][]byte
}{
	"image/jpeg": {
		mime:      "image/jpeg",
		extension: "jpg",
		// JPEG: FF D8 FF
		magic: [][]byte{{0xFF, 0xD8, 0xFF}},
	},
	"image/png": {
		mime:      "image/png",
		extension: "png",
		// PNG: 89 50 4E 47 0D 0A 1A 0A
		magic: [][]byte{{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}},
	},
	"image/gif": {
		mime:      "image/gif",
		extension: "gif",
		// GIF: "GIF87a" or "GIF89a"
		magic: [][]byte{
			[]byte("GIF87a"),
			[]byte("GIF89a"),
		},
	},
	"image/webp": {
		mime:      "image/webp",
		extension: "webp",
		// RIFF....WEBP
		magic: [][]byte{
			[]byte("RIFF????WEBP"),
		},
	},
}

// AllowedTypes returns the accepted MIME types, for error messages.
func AllowedTypes() []string {
	out := make([]string, 0, len(allowed))
	for m := range allowed {
		out = append(out, m)
	}
	return out
}

// sniffHeaderLen is how many leading bytes magic-byte detection needs.
const sniffHeaderLen = 32

// Result describes a stored file.
type Result struct {
	ID        string
	Filename  string
	Path      string
	MIME      string
	Size      int64
	Width     int
	Height    int
	SHA256    []byte
	CreatedAt time.Time
}

// Store owns the media root.
type Store struct {
	root string
}

// NewStore builds a Store over an existing media root.
func NewStore(root string) *Store { return &Store{root: root} }

// Root returns the media root.
func (s *Store) Root() string { return s.root }

func randomID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Save validates and stores an upload.
//
// declaredMIME is the type the client claimed. header is the leading bytes of
// the file, used only for magic-byte detection; if it is empty, Save reads it
// from r and puts it back. Either way r must yield the complete file, so a
// caller that has already consumed a prefix must re-join it before calling.
//
// The declared type and the magic bytes must agree with the allowlist, and the
// stored extension is derived from the detected type — never from the client's
// filename (ARCHITECTURE.md §19).
//
// maxPixels bounds the decoded image, not just the encoded bytes: a 5 MB JPEG can
// decode to 100 megapixels, and the delivery layer would allocate for all of them
// (ARCHITECTURE.md §26). Zero disables the check.
func (s *Store) Save(declaredMIME, originalName string, header []byte, r io.Reader, maxBytes int64, maxPixels int64) (*Result, error) {
	if len(header) < sniffHeaderLen {
		buf := make([]byte, sniffHeaderLen)
		n, err := io.ReadFull(r, buf)
		if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read upload: %w", err)
		}
		header = buf[:n]
		// Put the consumed bytes back so the file is stored intact.
		r = io.MultiReader(bytes.NewReader(header), r)
	}

	detected, ok := detect(header)
	if !ok {
		return nil, fmt.Errorf("%w: the file content is not an allowed image (allowed: %s)",
			ErrUnsupportedType, strings.Join(AllowedTypes(), ", "))
	}

	// ARCHITECTURE.md §19: the declared type must not contradict the bytes.
	// A mismatch means someone is lying about the content.
	declared := strings.ToLower(strings.TrimSpace(declaredMIME))
	if idx := strings.IndexByte(declared, ';'); idx >= 0 {
		declared = strings.TrimSpace(declared[:idx])
	}
	if declared != "" && declared != detected.mime && declared != "image/jpg" {
		return nil, fmt.Errorf("%w: declared %s but the content is %s",
			ErrUnsupportedType, declared, detected.mime)
	}

	// ARCHITECTURE.md §19: the extension is never trusted, so it is derived from
	// the detected type. The original name only survives as metadata.
	id, err := randomID()
	if err != nil {
		return nil, fmt.Errorf("generate id: %w", err)
	}

	now := time.Now().UTC()
	rel := fmt.Sprintf("%04d/%02d/%s.%s", now.Year(), int(now.Month()), id, detected.extension)
	full, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return nil, fmt.Errorf("create media directory: %w", err)
	}

	hasher := sha256.New()
	tmp, err := os.CreateTemp(filepath.Dir(full), ".tmp-upload-*")
	if err != nil {
		return nil, fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	// One extra byte over the cap, so exceeding it is detectable.
	written, err := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("write upload: %w", err)
	}
	if written > maxBytes {
		return nil, fmt.Errorf("%w: limit is %d bytes", ErrTooLarge, maxBytes)
	}
	if written == 0 {
		return nil, fmt.Errorf("%w: the file is empty", ErrUnsupportedType)
	}

	// Dimensions come from decoding, which also confirms the bytes really are a
	// valid image of that type.
	width, height := 0, 0
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("close temp file: %w", err)
	}
	if f, err := os.Open(tmpName); err == nil {
		// The pixel ceiling is checked from the header, before the body is
		// allocated, and a decode failure is fatal rather than ignored: a file
		// whose magic bytes say PNG but whose contents do not decode is not a PNG.
		if err := CheckPixels(f, maxPixels); err != nil {
			_ = f.Close()
			if errors.Is(err, ErrTooManyPixels) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: the file is not a decodable image: %v", ErrUnsupportedType, err)
		}
		if _, err := f.Seek(0, io.SeekStart); err == nil {
			if cfg, _, err := image.DecodeConfig(f); err == nil {
				width, height = cfg.Width, cfg.Height
			}
		}
		_ = f.Close()
	} else {
		return nil, fmt.Errorf("reopen upload: %w", err)
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		return nil, fmt.Errorf("chmod upload: %w", err)
	}
	if err := os.Rename(tmpName, full); err != nil {
		return nil, fmt.Errorf("store upload: %w", err)
	}

	return &Result{
		ID:        id,
		Filename:  safeDisplayName(originalName, detected.extension),
		Path:      rel,
		MIME:      detected.mime,
		Size:      written,
		Width:     width,
		Height:    height,
		SHA256:    hasher.Sum(nil),
		CreatedAt: now,
	}, nil
}

type detectedType struct {
	mime      string
	extension string
}

// detect identifies the file from its magic bytes.
func detect(header []byte) (detectedType, bool) {
	for _, t := range allowed {
		for _, sig := range t.magic {
			if matchesSig(header, sig) {
				return detectedType{mime: t.mime, extension: t.extension}, true
			}
		}
	}
	return detectedType{}, false
}

// matchesSig compares a signature, where '?' matches any single byte. The
// WebP signature needs this because the RIFF length field varies.
func matchesSig(header, sig []byte) bool {
	if len(header) < len(sig) {
		return false
	}
	for i, b := range sig {
		if b == '?' {
			continue
		}
		if header[i] != b {
			return false
		}
	}
	return true
}

// safeDisplayName keeps the original name only as a label, stripped of anything
// that could be mistaken for a path. It is never used to build a path.
//
// Any existing extension is dropped and replaced with the detected one, so a
// mislabelled upload cannot produce a confusing "photo.png.jpg".
func safeDisplayName(name, ext string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))

	// Drop the original extension before filtering, so the filter cannot turn
	// the separator into a dot.
	if existing := filepath.Ext(name); existing != "" {
		name = strings.TrimSuffix(name, existing)
	}

	name = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-' || r == '_' || r == '.':
			return r
		default:
			return -1
		}
	}, name)
	name = strings.Trim(name, ".-")
	if name == "" {
		name = "upload"
	}
	if len(name) > 120 {
		name = name[:120]
	}
	return name + "." + ext
}

// resolve turns a server-generated relative path into an absolute one and
// confirms it stays under the media root (ARCHITECTURE.md §20).
func (s *Store) resolve(rel string) (string, error) {
	root, err := filepath.Abs(s.root)
	if err != nil {
		return "", fmt.Errorf("resolve media root: %w", err)
	}
	root = filepath.Clean(root)

	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	inside, err := filepath.Rel(root, full)
	if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("media: refusing to use %q", full)
	}
	return full, nil
}

// Delete removes a stored file given its database-relative path.
//
// The path still comes from the database, not the request, and the containment
// check is repeated regardless.
func (s *Store) Delete(rel string) error {
	if rel == "" || strings.Contains(rel, "..") || strings.HasPrefix(rel, "/") {
		return fmt.Errorf("media: refusing to delete %q", rel)
	}
	full, err := s.resolve(rel)
	if err != nil {
		return err
	}
	if err := os.Remove(full); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ErrNotFound
		}
		return fmt.Errorf("delete media: %w", err)
	}
	return nil
}

// Exists reports whether a stored file is still present.
func (s *Store) Exists(rel string) bool {
	full, err := s.resolve(rel)
	if err != nil {
		return false
	}
	st, err := os.Stat(full)
	return err == nil && st.Mode().IsRegular()
}

// Open opens a stored file for reading, after re-checking containment.
//
// It is the read side the image pipeline and the delivery handler use. The path
// still comes from the database (or from a URL that was validated against the
// stored layout), never from a raw request parameter.
func (s *Store) Open(rel string) (*os.File, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open media: %w", err)
	}
	return f, nil
}

// Stat returns metadata for a stored file.
func (s *Store) Stat(rel string) (os.FileInfo, error) {
	full, err := s.resolve(rel)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("stat media: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, ErrNotFound
	}
	return st, nil
}

// Root returns the media root. Exposed so a store can be built over an existing
// directory in a test or a maintenance command; the delivery layer never needs it.
func (s *Store) RootDir() string { return s.root }

// IDPattern is the shape of a media id: lowercase hex, generated by randomID.
var IDPattern = regexp.MustCompile(`^[0-9a-f]{24}$`)

// ValidID reports whether s is a well-formed media id.
//
// Used before a media id becomes a query parameter, so a hostile value cannot even
// reach SQLite — the value would be parameterised either way, but a bounded string
// is a bounded index scan and an obviously-bad one is worth refusing early.
func ValidID(s string) bool { return len(s) == 24 && IDPattern.MatchString(s) }
