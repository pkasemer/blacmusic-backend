package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestMetadataReview(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Review Artist")
	pool := w.h.pool
	rel := testutil.Release(t, pool, art, "Reviewed", "album", "public")
	song, _ := testutil.ReadyTrack(t, pool, w.admin, art, rel, "One", 100, "public")
	other, _ := testutil.ReadyTrack(t, pool, w.admin, art, rel, "Two", 100, "public")
	w.exec(`UPDATE tracks SET genre = 'Gospel' WHERE id = $1`, song)

	text := func(v any) string {
		b, _ := json.Marshal(v)
		return string(b)
	}
	get := func(id string) map[string]any {
		t.Helper()
		code, b := call(w.r, "GET", "/admin/tracks/"+id+"/metadata", w.adminT, nil)
		if code != 200 {
			t.Fatalf("get = %d %v", code, b)
		}
		return b
	}
	post := func(id, what string, body map[string]any) (int, map[string]any) {
		if body["version"] == nil {
			body["version"] = get(id)["version"]
		}
		return call(w.r, "POST", "/admin/tracks/"+id+"/metadata/"+what, w.adminT, body)
	}
	save := func(id string, body map[string]any) map[string]any {
		t.Helper()
		body["version"] = get(id)["version"]
		code, b := call(w.r, "PUT", "/admin/tracks/"+id+"/metadata", w.adminT, body)
		if code != 200 {
			t.Fatalf("save = %d %v", code, b)
		}
		return b
	}
	status := func(m map[string]any, want string) {
		t.Helper()
		if m["status"] != want {
			t.Fatalf("status = %v, want %s: %s", m["status"], want, text(m))
		}
	}
	row := func(filter string) map[string]any {
		t.Helper()
		code, b := call(w.r, "GET", "/admin/releases?artistId="+art+filter, w.adminT, nil)
		if code != 200 {
			t.Fatalf("releases = %d %v", code, b)
		}
		if list := b["releases"].([]any); len(list) == 1 {
			return list[0].(map[string]any)
		}
		return nil
	}

	_, tax := call(w.r, "GET", "/admin/taxonomy", w.adminT, nil)
	if text(tax["requiredFields"]) != `["language"]` {
		t.Fatalf("required = %v", tax["requiredFields"])
	}
	m := get(song)
	status(m, "not_started")
	if text(m["missingRequired"]) != `["language"]` || m["submittedAt"] != nil || m["languageUnknownReview"] != nil || m["reviewNote"] != "" {
		t.Fatalf("before = %s", text(m))
	}
	if r := row("&metadata=not_started"); r == nil || text(r["metadata"]) != `{"enriched":0,"pendingReview":0,"status":"not_started","total":2,"verified":0}` {
		t.Fatalf("row = %s", text(r))
	}

	code, got := post(song, "submit", map[string]any{})
	if code != 422 || errCode(got) != "missing_required" || !strings.Contains(text(got), `"missing":["language"]`) {
		t.Fatalf("submit with nothing = %d %s", code, text(got))
	}
	languages := []map[string]any{{"id": "luganda", "role": "primary"}}
	status(save(song, map[string]any{"languages": languages, "genres": []map[string]any{{"id": "gospel", "primary": true}}, "cultures": []string{"baganda"}}), "pending_review") // it waits for review as soon as it is saved
	if r := row("&pendingReview=true"); r == nil || r["metadata"].(map[string]any)["pendingReview"] != float64(1) {
		t.Fatalf("waiting without being submitted = %s", text(r))
	}
	if _, b := call(w.r, "GET", "/admin/metadata/tracks?artistId="+art+"&queue=pending_review", w.adminT, nil); b["total"] != float64(1) {
		t.Fatalf("queue without being submitted = %s", text(b))
	}
	code, got = post(song, "submit", map[string]any{})
	if code != 200 || got["submittedBy"] == nil {
		t.Fatalf("submit = %d %s", code, text(got))
	}
	status(got, "pending_review")
	if code, again := post(song, "submit", map[string]any{}); code != 200 || again["version"] != got["version"] || w.audit("track.metadata_submit", song) != 1 {
		t.Fatalf("submitting again = %d %v", code, again["version"])
	}
	if code, b := post(song, "submit", map[string]any{"version": "0"}); code != 409 || errCode(b) != "stale_version" {
		t.Fatalf("stale = %d %v", code, b)
	}
	if r := row("&pendingReview=true&metadata=in_progress"); r == nil || r["metadata"].(map[string]any)["pendingReview"] != float64(1) {
		t.Fatalf("pending row = %s", text(r))
	}

	item := func(category, id string) []map[string]any { return []map[string]any{{"category": category, "id": id}} }
	for name, body := range map[string]map[string]any{
		"no items":       {"decision": "verify", "items": []any{}},
		"not on song":    {"decision": "verify", "items": item("languages", "english")},
		"half wrong":     {"decision": "verify", "items": append(item("languages", "luganda"), item("regions", "west_nile")...)},
		"reject no note": {"decision": "reject", "items": item("languages", "luganda")},
		"bad decision":   {"decision": "maybe", "items": item("languages", "luganda")},
	} {
		if code, b := post(song, "review", body); code != 422 || errCode(b) != "validation_error" {
			t.Fatalf("%s = %d %v", name, code, b)
		}
	}
	status(get(song), "pending_review") // nothing of "half wrong" was kept

	code, got = post(song, "review", map[string]any{"decision": "reject", "items": item("languages", "luganda"), "note": "wrong language"})
	if code != 200 || got["reviewNote"] != "wrong language" || got["submittedAt"] != nil || !strings.Contains(text(got["languages"]), `"verification":"rejected"`) {
		t.Fatalf("reject = %d %s", code, text(got))
	}
	status(got, "in_progress")
	if text(got["missingRequired"]) != `["language"]` || w.audit("track.metadata_reject", song) != 1 {
		t.Fatalf("after reject = %s", text(got))
	}
	// a rejected genre stays the song's genre until someone edits it
	if code, b := post(song, "review", map[string]any{"decision": "reject", "items": item("genres", "gospel"), "note": "not gospel"}); code != 200 || w.text(`SELECT genre FROM tracks WHERE id = $1`, song) != "Gospel" {
		t.Fatalf("reject genre = %d %v", code, b)
	}

	// "unknown" is checked like any value, and only what is required has to be verified
	got = save(song, map[string]any{"languageUnknown": true, "genres": []map[string]any{{"id": "gospel", "primary": true}}, "cultures": []string{"baganda"}})
	if got["languageUnknownReview"].(map[string]any)["verification"] != "pending" {
		t.Fatalf("unknown = %s", text(got))
	}
	code, got = post(song, "review", map[string]any{"decision": "verify", "items": item("languageUnknown", "")})
	if code != 200 || got["reviewNote"] != "" || got["languageUnknownReview"].(map[string]any)["verifiedBy"] == nil {
		t.Fatalf("verify unknown = %d %s", code, text(got))
	}
	status(got, "verified")
	// editing a verified required value takes it out of verified, back to waiting
	status(save(song, map[string]any{"languages": languages, "cultures": []string{"baganda"}}), "pending_review")
	code, got = post(song, "review", map[string]any{"decision": "verify", "items": item("languages", "luganda")})
	if code != 200 {
		t.Fatalf("verify = %d %v", code, got)
	}
	status(got, "verified")

	// queues, with counts under the same filters
	queue := func(name string) (map[string]any, string) {
		code, b := call(w.r, "GET", "/admin/metadata/tracks?artistId="+art+"&queue="+name, w.adminT, nil)
		if code != 200 {
			t.Fatalf("queue %s = %d %v", name, code, b)
		}
		ids := []string{}
		for _, x := range b["tracks"].([]any) {
			ids = append(ids, x.(map[string]any)["trackId"].(string))
		}
		return b, strings.Join(ids, ",")
	}
	b, ids := queue("incomplete")
	if ids != other || b["total"] != float64(1) || text(b["counts"]) != `{"culture_region_review":1,"incomplete":1,"no_genre":2,"no_language":1,"pending_review":0,"unverified_language":0}` {
		t.Fatalf("incomplete = %s %s", ids, text(b))
	}
	if _, ids := queue("culture_region_review"); ids != song {
		t.Fatalf("culture queue = %s", ids)
	}
	if code, _ := call(w.r, "GET", "/admin/metadata/tracks?queue=x", w.adminT, nil); code != 400 {
		t.Fatalf("bad queue = %d", code)
	}

	// finishing the other song completes the release
	save(other, map[string]any{"languageUnknown": true})
	if code, b := post(other, "review", map[string]any{"decision": "verify", "items": item("languageUnknown", "")}); code != 200 {
		t.Fatalf("verify other = %d %v", code, b)
	}
	if r := row("&metadata=completed"); r == nil || r["metadata"].(map[string]any)["verified"] != float64(2) {
		t.Fatalf("completed row = %s", text(r))
	}
}
