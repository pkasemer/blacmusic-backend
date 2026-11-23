package upload

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

// patterned returns n bytes where every byte depends on its position, so a
// wrongly assembled file is detected.
func patterned(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// startMultipart creates a 12000-byte upload (3 parts of 5000, 5000, 2000).
func startMultipart(t *testing.T, e *env, tok string) (id, mpID string, parts []any) {
	t.Helper()
	code, b := e.do("POST", "/uploads", tok, body(12000))
	if code != 201 {
		t.Fatalf("create = %d %v", code, b)
	}
	target := b["upload"].(map[string]any)
	id = b["id"].(string)
	if err := e.pool.QueryRow(context.Background(), `SELECT multipart_id FROM uploads WHERE id=$1`, id).Scan(&mpID); err != nil {
		t.Fatal(err)
	}
	return id, mpID, target["parts"].([]any)
}

func TestLargeFilesGetAResumableUpload(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)

	// Exactly twice the part size still goes in one PUT; one byte more is multipart.
	if _, b := e.do("POST", "/uploads", tok, body(2*testPart)); b["upload"].(map[string]any)["type"] != "single" {
		t.Fatalf("2x part size should be a single PUT: %v", b["upload"])
	}
	code, b := e.do("POST", "/uploads", tok, body(2*testPart+1))
	if code != 201 {
		t.Fatalf("create = %d %v", code, b)
	}
	target := b["upload"].(map[string]any)
	if target["type"] != "multipart" || target["method"] != "PUT" || target["url"] != nil ||
		target["partSize"].(float64) != testPart || target["partCount"].(float64) != 3 {
		t.Fatalf("target = %v", target)
	}
	parts := target["parts"].([]any)
	if len(parts) != 3 {
		t.Fatalf("parts = %v", parts)
	}
	for i, p := range parts {
		pm := p.(map[string]any)
		if int(pm["number"].(float64)) != i+1 || !strings.Contains(pm["url"].(string), "partNumber=") {
			t.Fatalf("part %d = %v", i, pm)
		}
	}
	if b["partSize"].(float64) != testPart || b["partCount"].(float64) != 3 {
		t.Fatalf("view should describe the parts: %v", b)
	}
	if e.blobs.OpenUploads() != 1 {
		t.Fatalf("open multipart uploads = %d, want 1", e.blobs.OpenUploads())
	}
}

func TestResumeAfterAnInterruption(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)
	id, mp, _ := startMultipart(t, e, tok)
	data := patterned(12000)

	// The connection drops after parts 1 and 3 went through.
	e.blobs.PutPart(mp, 1, data[:5000])
	e.blobs.PutPart(mp, 3, data[10000:])

	code, b := e.do("GET", "/uploads/"+id+"/parts", tok, nil)
	if code != 200 {
		t.Fatalf("parts = %d %v", code, b)
	}
	if up := b["uploaded"].([]any); len(up) != 2 || up[0].(map[string]any)["number"].(float64) != 1 || up[1].(map[string]any)["number"].(float64) != 3 {
		t.Fatalf("uploaded = %v", up)
	}
	todo := b["parts"].([]any)
	if len(todo) != 1 || todo[0].(map[string]any)["number"].(float64) != 2 {
		t.Fatalf("to send = %v, want only part 2 (with a fresh URL)", todo)
	}

	// Completing now is refused, and says exactly what is missing.
	code, b = e.do("POST", "/uploads/"+id+"/complete", tok, nil)
	d := b["error"].(map[string]any)["details"].(map[string]any)
	if code != 409 || errCode(b) != "parts_missing" || len(d["missing"].([]any)) != 1 || d["missing"].([]any)[0].(float64) != 2 {
		t.Fatalf("complete with a gap = %d %v", code, b)
	}
	if _, g := e.do("GET", "/uploads/"+id, tok, nil); g["status"] != "pending" {
		t.Fatal("a refused completion must leave the upload pending so it can be finished")
	}

	// A part of the wrong size is reported and offered again.
	e.blobs.PutPart(mp, 2, data[5000:5100])
	code, b = e.do("POST", "/uploads/"+id+"/complete", tok, nil)
	d = b["error"].(map[string]any)["details"].(map[string]any)
	if code != 409 || len(d["wrongSize"].([]any)) != 1 || d["wrongSize"].([]any)[0].(float64) != 2 || len(d["missing"].([]any)) != 0 {
		t.Fatalf("complete with a short part = %d %v", code, b)
	}
	if _, b = e.do("GET", "/uploads/"+id+"/parts", tok, nil); len(b["parts"].([]any)) != 1 {
		t.Fatalf("the short part must be offered again: %v", b["parts"])
	}

	// The client resends part 2 correctly; now it assembles and queues.
	e.blobs.PutPart(mp, 2, data[5000:10000])
	code, b = e.do("POST", "/uploads/"+id+"/complete", tok, nil)
	if code != 202 || b["status"] != "queued" {
		t.Fatalf("complete = %d %v, want 202 queued", code, b)
	}
	got, ok := e.blobs.Get("originals/" + id)
	if !ok || !bytes.Equal(got, data) {
		t.Fatalf("the assembled file differs from what was uploaded (%d bytes, ok=%v)", len(got), ok)
	}
	if e.blobs.OpenUploads() != 0 {
		t.Fatal("the multipart upload must be closed after assembling")
	}
	var open string
	e.pool.QueryRow(context.Background(), `SELECT multipart_id FROM uploads WHERE id=$1`, id).Scan(&open) //nolint:errcheck
	if open != "" {
		t.Fatalf("multipart_id = %q, want cleared once queued", open)
	}
	// Repeating the call, or asking for parts, is harmless / refused cleanly.
	if code, _ = e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 200 {
		t.Fatalf("second complete = %d, want 200", code)
	}
	if code, b = e.do("GET", "/uploads/"+id+"/parts", tok, nil); code != 409 || errCode(b) != "not_pending" {
		t.Fatalf("parts after queueing = %d %v, want 409 not_pending", code, b)
	}
}

func TestCompleteSurvivesALostResponse(t *testing.T) {
	// The server assembled the file but crashed before recording it; the retry must still work.
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)
	id, mp, _ := startMultipart(t, e, tok)
	data := patterned(12000)
	e.blobs.PutPart(mp, 1, data[:5000])
	e.blobs.PutPart(mp, 2, data[5000:10000])
	e.blobs.PutPart(mp, 3, data[10000:])
	parts, _ := e.blobs.ListParts(context.Background(), "originals/"+id, mp)
	if err := e.blobs.CompleteMultipart(context.Background(), "originals/"+id, mp, parts); err != nil {
		t.Fatal(err)
	}
	if code, b := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 202 || b["status"] != "queued" {
		t.Fatalf("retry = %d %v, want 202 queued", code, b)
	}
}

func TestMultipartErrors(t *testing.T) {
	e := setup(t)
	tok, _, _ := e.artistUser(testutil.GiB)

	// Single-PUT uploads have no parts.
	_, small := e.do("POST", "/uploads", tok, body(100))
	if code, b := e.do("GET", "/uploads/"+small["id"].(string)+"/parts", tok, nil); code != 400 || errCode(b) != "not_multipart" {
		t.Fatalf("parts of a single upload = %d %v", code, b)
	}

	// Someone else cannot see or complete it.
	id, _, _ := startMultipart(t, e, tok)
	other, _, _ := e.artistUser(testutil.GiB)
	for _, c := range []struct{ m, p string }{{"GET", "/parts"}, {"POST", "/complete"}} {
		if code, _ := e.do(c.m, "/uploads/"+id+c.p, other, nil); code != 404 {
			t.Errorf("%s %s as another user = %d, want 404", c.m, c.p, code)
		}
	}

	// If the object store forgot the upload (e.g. it was aborted), say so.
	gone, _, _ := startMultipart(t, e, tok)
	var mp string
	e.pool.QueryRow(context.Background(), `SELECT multipart_id FROM uploads WHERE id=$1`, gone).Scan(&mp) //nolint:errcheck
	e.blobs.AbortMultipart(context.Background(), "originals/"+gone, mp)                                   //nolint:errcheck
	if code, b := e.do("POST", "/uploads/"+gone+"/complete", tok, nil); code != 409 || errCode(b) != "expired" {
		t.Fatalf("complete after the upload was discarded = %d %v, want 409 expired", code, b)
	}
}

func TestCancellingAMultipartUploadAbortsItAndFreesQuota(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(testutil.GiB)
	id, mp, _ := startMultipart(t, e, tok)
	e.blobs.PutPart(mp, 1, patterned(5000))
	if got := e.quota(artistID).UsedStorageBytes; got != 12000 {
		t.Fatalf("reserved = %d, want 12000", got)
	}
	if code, _ := e.do("DELETE", "/uploads/"+id, tok, nil); code != 204 {
		t.Fatalf("cancel = %d", code)
	}
	if e.blobs.OpenUploads() != 0 || len(e.blobs.Aborted()) != 1 || e.blobs.Aborted()[0] != mp {
		t.Fatalf("the multipart upload must be aborted so its parts stop using storage (open=%d aborted=%v)",
			e.blobs.OpenUploads(), e.blobs.Aborted())
	}
	if got := e.quota(artistID).UsedStorageBytes; got != 0 {
		t.Fatalf("used = %d, want 0 after cancelling", got)
	}
}

func TestFilesCannotBeBiggerThanDeclared(t *testing.T) {
	e := setup(t)
	tok, _, artistID := e.artistUser(10_000)

	// Declaring 100 bytes and uploading 4000 must not slip past the quota.
	_, c := e.do("POST", "/uploads", tok, body(100))
	id := c["id"].(string)
	e.blobs.Put("originals/"+id, make([]byte, 4000))
	if code, b := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 422 {
		t.Fatalf("bigger than declared = %d %v, want 422", code, b)
	}
	if _, ok := e.blobs.Get("originals/" + id); ok {
		t.Fatal("the oversized object must be deleted")
	}
	if got := e.quota(artistID).UsedStorageBytes; got != 0 {
		t.Fatalf("used = %d after rejection, want 0", got)
	}

	// A smaller file is fine, and the reservation shrinks to what was really used.
	_, c = e.do("POST", "/uploads", tok, body(1000))
	id = c["id"].(string)
	e.blobs.Put("originals/"+id, make([]byte, 600))
	if code, b := e.do("POST", "/uploads/"+id+"/complete", tok, nil); code != 202 || b["sizeBytes"].(float64) != 600 {
		t.Fatalf("smaller file = %d %v, want 202 with sizeBytes 600", code, b)
	}
	if got := e.quota(artistID).UsedStorageBytes; got != 600 {
		t.Fatalf("used = %d, want 600", got)
	}
}
