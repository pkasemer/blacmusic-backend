package admin

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/testutil"
)

func TestBulkMetadata(t *testing.T) {
	w := newWorld(t)
	art := w.catalogArtist("Bulk Artist")
	pool := w.h.pool
	rel := testutil.Release(t, pool, art, "Bulk", "album", "public")
	song := func() string {
		id, _ := testutil.ReadyTrack(t, pool, w.admin, art, rel, "S", 100, "public")
		return id
	}
	plain, has, unknown, bystander := song(), song(), song(), song()
	w.exec(`UPDATE tracks SET genre = 'Gospel' WHERE id = $1`, has)
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
	put := func(id string, body map[string]any) {
		t.Helper()
		body["version"] = get(id)["version"]
		if code, b := call(w.r, "PUT", "/admin/tracks/"+id+"/metadata", w.adminT, body); code != 200 {
			t.Fatalf("save = %d %v", code, b)
		}
	}
	put(has, map[string]any{"languages": []map[string]any{{"id": "english", "role": "primary"}}, "cultures": []string{"baganda"},
		"genres": []map[string]any{{"id": "gospel", "primary": true}}})
	put(unknown, map[string]any{"languageUnknown": true})
	w.exec(`UPDATE track_languages SET verification = 'verified' WHERE track_id = $1`, has)

	ids := []string{plain, has, unknown}
	bulk := func(path string, body map[string]any) (int, map[string]any) {
		return call(w.r, "POST", "/admin/metadata/"+path, w.adminT, body)
	}
	add := map[string]any{
		"languages": map[string]any{"mode": "add", "values": []map[string]any{{"id": "luganda", "role": "primary"}}},
		"regions":   map[string]any{"mode": "add", "ids": []string{"central_uganda"}},
	}
	code, preview := bulk("bulk", map[string]any{"trackIds": ids, "dryRun": true, "changes": add})
	if code != 200 || preview["changed"] != float64(2) || preview["skipped"] != float64(1) {
		t.Fatalf("preview = %d %s", code, text(preview))
	}
	if get(plain)["version"] != "0" || len(get(plain)["languages"].([]any)) != 0 || w.audit("track.metadata_update", plain) != 0 {
		t.Fatal("a preview must change nothing")
	}
	versions := map[string]string{}
	for _, x := range preview["results"].([]any) {
		m := x.(map[string]any)
		versions[m["trackId"].(string)] = m["version"].(string)
	}
	third := preview["results"].([]any)[2].(map[string]any)
	if third["outcome"] != "skipped" || third["reason"] != "language_unknown" || text(third["added"]) != `[]` {
		t.Fatalf("skipped = %s", text(third))
	}

	if code, b := bulk("bulk", map[string]any{"trackIds": ids, "dryRun": false, "changes": add}); code != 422 {
		t.Fatalf("no versions = %d %v", code, b)
	}
	put(plain, map[string]any{"cultures": []string{"alur"}}) // someone else changes one song meanwhile
	code, b := bulk("bulk", map[string]any{"trackIds": ids, "dryRun": false, "versions": versions, "changes": add})
	if code != 409 || errCode(b) != "stale_version" || !strings.Contains(text(b), plain) || len(get(has)["regions"].([]any)) != 0 {
		t.Fatalf("stale must apply nothing = %d %s", code, text(b))
	}
	versions[plain] = get(plain)["version"].(string)
	code, b = bulk("bulk", map[string]any{"trackIds": ids, "dryRun": false, "versions": versions, "changes": add})
	if code != 200 || b["changed"] != float64(2) {
		t.Fatalf("apply = %d %s", code, text(b))
	}
	// adding keeps the primary there was, its check, and every list not named
	got := w.text(`SELECT string_agg(language_id || ':' || role || ':' || verification, ',' ORDER BY language_id) FROM track_languages WHERE track_id = $1`, has)
	if got != "english:primary:verified,luganda:secondary:pending" || len(get(has)["cultures"].([]any)) != 1 || w.text(`SELECT genre FROM tracks WHERE id = $1`, has) != "Gospel" {
		t.Fatalf("after add = %s", got)
	}
	if w.text(`SELECT role FROM track_languages WHERE track_id = $1`, plain) != "primary" || len(get(plain)["cultures"].([]any)) != 1 ||
		get(unknown)["languageUnknown"] != true || len(get(unknown)["regions"].([]any)) != 0 || get(bystander)["version"] != "0" {
		t.Fatal("only the chosen songs and lists may change")
	}
	if got := w.text(`SELECT details->>'bulk' FROM admin_audit_log WHERE action = 'track.metadata_update' AND target_id = $1 ORDER BY id DESC LIMIT 1`, has); got != "true" {
		t.Fatalf("audit = %s", got)
	}

	// replacing a list clears "unknown" and moves tracks.genre with the main genre
	replace := map[string]any{
		"languages": map[string]any{"mode": "replace", "values": []map[string]any{{"id": "swahili", "role": "primary"}}},
		"genres":    map[string]any{"mode": "replace", "values": []map[string]any{{"id": "pop", "primary": true}}},
	}
	now := map[string]string{has: get(has)["version"].(string), unknown: get(unknown)["version"].(string)}
	code, b = bulk("bulk", map[string]any{"trackIds": []string{has, unknown}, "dryRun": false, "versions": now, "changes": replace})
	if code != 200 || b["changed"] != float64(2) || get(unknown)["languageUnknown"] != false || w.text(`SELECT genre FROM tracks WHERE id = $1`, has) != "Pop" ||
		len(get(has)["cultures"].([]any)) != 1 || len(get(has)["regions"].([]any)) != 1 {
		t.Fatalf("replace = %d %s", code, text(b))
	}

	for name, body := range map[string]map[string]any{
		"nothing named":  {"trackIds": ids, "dryRun": true, "changes": map[string]any{}},
		"clearing":       {"trackIds": ids, "dryRun": true, "changes": map[string]any{"regions": map[string]any{"mode": "replace", "ids": []string{}}}},
		"repeated track": {"trackIds": []string{has, has}, "dryRun": true, "changes": add},
	} {
		if code, b := bulk("bulk", body); code != 422 || errCode(b) != "validation_error" {
			t.Fatalf("%s = %d %v", name, code, b)
		}
	}
	wrong := map[string]any{"regions": map[string]any{"mode": "add", "ids": []string{"mars"}}}
	if code, b := bulk("bulk", map[string]any{"trackIds": ids, "dryRun": true, "changes": wrong}); code != 422 || errCode(b) != "invalid_taxonomy" {
		t.Fatalf("unknown term = %d %v", code, b)
	}
	if code, b := bulk("bulk", map[string]any{"trackIds": []string{has, "trk_nothing"}, "dryRun": true, "changes": add}); code != 404 || !strings.Contains(text(b), "trk_nothing") {
		t.Fatalf("unknown track = %d %v", code, b)
	}

	// bulk verify: only what is pending in the named lists
	w.exec(`UPDATE track_regions SET verification = 'rejected' WHERE track_id = $1`, plain)
	verify := map[string]any{"trackIds": []string{plain, bystander}, "categories": []string{"languages", "regions"}, "dryRun": true}
	code, b = bulk("bulk-verify", verify)
	if code != 200 || b["changed"] != float64(1) || b["unchanged"] != float64(1) || text(b["results"].([]any)[0].(map[string]any)["verified"]) != `[{"category":"languages","id":"luganda"}]` {
		t.Fatalf("verify preview = %d %s", code, text(b))
	}
	verify["dryRun"], verify["versions"] = false, map[string]string{plain: get(plain)["version"].(string), bystander: "0"}
	if code, b = bulk("bulk-verify", verify); code != 200 || get(plain)["status"] != "verified" || w.audit("track.metadata_verify", plain) != 1 {
		t.Fatalf("verify = %d %s", code, text(b))
	}
	if got := w.text(`SELECT verification FROM track_regions WHERE track_id = $1`, plain) + " " + w.text(`SELECT verification FROM track_cultures WHERE track_id = $1`, plain); got != "rejected pending" {
		t.Fatalf("rejected and unnamed stay = %s", got)
	}
}
