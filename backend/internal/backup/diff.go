package backup

import (
	"bytes"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	fdiff "github.com/go-git/go-git/v5/plumbing/format/diff"
	gogitdiff "github.com/go-git/go-git/v5/utils/diff"
	"github.com/sergi/go-diff/diffmatchpatch"
)

// buildPatch renders the pending change of one file as a
// unified diff. The committed side comes from HEAD, the
// working side from the snapshot; either side may be
// absent, which is how a new or a deleted file diffs.
//
// The patch is plain text: the admin screen escapes it
// like any other untrusted value, because the files it
// is computed from are admin-authored Markdown, CSS and
// JavaScript.
func buildPatch(path string, oldContent *string, newContent []byte) string {
	var oldText string
	if oldContent != nil {
		oldText = *oldContent
	}
	var newText string
	if newContent != nil {
		newText = string(newContent)
	}

	patch := singlePatch{filePatch: &textFilePatch{
		from:   fileRef{path: path, exists: oldContent != nil},
		to:     fileRef{path: path, exists: newContent != nil},
		chunks: chunks(oldText, newText),
	}}

	var buf bytes.Buffer
	if err := fdiff.NewUnifiedEncoder(&buf, fdiff.DefaultContextLines).Encode(patch); err != nil {
		// A rendering failure is reported as no diff
		// rather than failing the whole request.
		return ""
	}
	return buf.String()
}

func chunks(oldText, newText string) []fdiff.Chunk {
	var out []fdiff.Chunk
	for _, d := range gogitdiff.Do(oldText, newText) {
		var op fdiff.Operation
		switch d.Type {
		case diffmatchpatch.DiffEqual:
			op = fdiff.Equal
		case diffmatchpatch.DiffDelete:
			op = fdiff.Delete
		case diffmatchpatch.DiffInsert:
			op = fdiff.Add
		default:
			continue
		}
		if d.Text == "" {
			continue
		}
		out = append(out, textChunk{content: d.Text, op: op})
	}
	return out
}

type singlePatch struct {
	filePatch *textFilePatch
}

func (p singlePatch) FilePatches() []fdiff.FilePatch {
	return []fdiff.FilePatch{p.filePatch}
}

func (p singlePatch) Message() string { return "" }

type textFilePatch struct {
	from, to fileRef
	chunks   []fdiff.Chunk
}

func (f *textFilePatch) IsBinary() bool { return false }

// Files returns nil for a side the file does not exist
// on: that is what the unified encoder reads to print a
// "new file" or a "deleted file" header.
func (f *textFilePatch) Files() (fdiff.File, fdiff.File) {
	var from, to fdiff.File
	if f.from.exists {
		from = f.from
	}
	if f.to.exists {
		to = f.to
	}
	return from, to
}

func (f *textFilePatch) Chunks() []fdiff.Chunk { return f.chunks }

type fileRef struct {
	path   string
	exists bool
}

func (f fileRef) Hash() plumbing.Hash     { return plumbing.ZeroHash }
func (f fileRef) Mode() filemode.FileMode { return filemode.Regular }
func (f fileRef) Path() string            { return f.path }

type textChunk struct {
	content string
	op      fdiff.Operation
}

func (c textChunk) Content() string       { return c.content }
func (c textChunk) Type() fdiff.Operation { return c.op }
