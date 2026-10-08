package api

// ARCHITECTURE.md §16/§22: the audit log.
//
// The HTTP suites prove what a browser sees, and a browser
// never reads the audit table, so the two promises the
// architecture makes about it are proved here and nowhere
// else: every decision appends an event that names a verb
// and a reference, and no event ever carries content.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"blogcms/internal/content"
)

func assetRequest(t *testing.T, method, target string, body any) *http.Request {
	t.Helper()
	var reader *strings.Reader
	if body == nil {
		reader = strings.NewReader("")
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = strings.NewReader(string(raw))
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req
}

func callAsset(t *testing.T, h http.HandlerFunc, req *http.Request) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec.Code, rec.Body.String()
}

func decodeAssetOne(t *testing.T, body string) customAssetContentResponse {
	t.Helper()
	var out customAssetContentResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode asset %q: %v", body, err)
	}
	return out
}

// auditRows returns the audit log, oldest first.
func auditRows(t *testing.T, d Deps) [][2]string {
	t.Helper()
	rows, err := d.DB.Query(
		`SELECT kind, ref FROM content_event ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	out := [][2]string{}
	for rows.Next() {
		var kind, ref string
		if err := rows.Scan(&kind, &ref); err != nil {
			t.Fatal(err)
		}
		out = append(out, [2]string{kind, ref})
	}
	return out
}

func TestCustomAssetCreateWritesAFileAndAudits(t *testing.T) {
	d := testDeps(t)
	req := assetRequest(t, "POST", "/api/v1/admin/custom/css", map[string]any{
		"filename": "010-layout.css",
		"content":  ".a{color:red}\n",
	})
	status, body := callAsset(t, d.createCustomAsset(content.AssetCSS), req)
	if status != http.StatusCreated {
		t.Fatalf("create = %d (%s)", status, body)
	}

	onDisk, err := os.ReadFile(filepath.Join(d.Cfg.ContentRoot, "system", "css", "010-layout.css"))
	if err != nil {
		t.Fatalf("the file was not written: %v", err)
	}
	if string(onDisk) != ".a{color:red}\n" {
		t.Errorf("on-disk body = %q", onDisk)
	}

	got := decodeAssetOne(t, body)
	if got.ID != "010-layout.css" || !got.Enabled || got.Status != content.AssetStatusOK {
		t.Errorf("created row = %+v", got)
	}

	rows := auditRows(t, d)
	if len(rows) != 1 || rows[0][0] != "custom_css.created" {
		t.Fatalf("audit = %v, want one custom_css.created", rows)
	}
	if rows[0][1] != "system/css/010-layout.css" {
		t.Errorf("audit ref = %q", rows[0][1])
	}
}

// §22: the audit log records what happened and to what. Never the text.
func TestCustomAssetAuditNeverCarriesTheBody(t *testing.T) {
	d := testDeps(t)
	const secret = "SECRET-MARKER-9f2a"
	_, body := callAsset(t, d.createCustomAsset(content.AssetCSS), assetRequest(t, "POST", "/api/v1/admin/custom/css",
		map[string]any{"filename": "010-layout.css", "content": ".a{content:'" + secret + "'}\n"}))
	if !strings.Contains(body, secret) {
		t.Fatal("the create response did not echo the body, so the check below proves nothing")
	}
	for _, row := range auditRows(t, d) {
		if strings.Contains(row[0], secret) || strings.Contains(row[1], secret) {
			t.Errorf("the audit log leaked the body: %v", row)
		}
	}

	// Walk the whole table as JSON too: a `content` column on content_event would not
	// show up in a kind/ref pair.
	var payload string
	if err := d.DB.QueryRow(`SELECT group_concat(kind || '|' || ref) FROM content_event`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(payload, secret) {
		t.Errorf("content_event holds the body: %s", payload)
	}
}
