package admin

import (
	"context"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/storage"
	"github.com/blacmusic/backend/internal/sysmetrics"
	"github.com/blacmusic/backend/internal/testutil"
)

type countedObjects struct{ calls int }

func (c *countedObjects) UsageByPrefix(context.Context) (map[string]storage.Usage, error) {
	c.calls++
	return map[string]storage.Usage{"originals": {Objects: 2, Bytes: 100}, "imports": {Objects: 1, Bytes: 50}, "tracks": {Objects: 9, Bytes: 300},
		"images": {Objects: 3, Bytes: 7}, "stray": {Objects: 1, Bytes: 1}}, nil
}

func TestServerMetrics(t *testing.T) {
	f := setupFull(t)
	pool, ctx := f.h.pool, context.Background()
	if _, err := pool.Exec(ctx, `DELETE FROM server_samples`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM server_samples`) }) //nolint:errcheck
	machine := sysmetrics.Host{CPUCount: 1, Load1: 0.4, Load5: 0.5, Load15: 0.6, MemTotal: 2000, MemAvail: 1200, SwapTotal: 500, SwapFree: 400,
		UptimeSeconds: 60, DiskTotal: 50 * gib, DiskUsed: 39 * gib, DiskFree: 7 * gib}
	f.h.ReadHost = func() (sysmetrics.Host, bool) { return machine, true }
	codes := func(b map[string]any) map[string]string {
		out := map[string]string{}
		for _, a := range b["alerts"].([]any) {
			out[a.(map[string]any)["code"].(string)] = a.(map[string]any)["level"].(string)
		}
		return out
	}

	// before the worker has reported
	code, b := call(f.r, "GET", "/admin/system", f.adminT, nil)
	if code != 200 || b["lastSampleAt"] != nil || b["storage"].(map[string]any)["objectBytes"] != nil || codes(b)["worker_silent"] != "critical" || codes(b)["disk_low"] != "warning" {
		t.Fatalf("before any sample = %d %v", code, b)
	}

	// an artist who takes space
	art := testutil.Artist(t, pool, testutil.User(t, pool, "user", true), testutil.Unique("Heavy"), testutil.GiB)
	rel := testutil.Release(t, pool, art, "R", "single", "draft")
	testutil.ReadyTrack(t, pool, f.admin, art, rel, "T", 900<<30, "draft") // far more than anything another test left

	counter := &countedObjects{}
	sampler := sysmetrics.NewSampler(pool, counter)
	sampler.Read = f.h.ReadHost
	pool.Exec(ctx, `INSERT INTO server_samples (at, load1, load5, mem_total, mem_available, disk_total, disk_used, disk_free, uploads_queued, uploads_processing)
		VALUES (now() - interval '40 days', 9, 9, 1, 0, 1, 1, 0, 0, 0), (now() - interval '3 hours', 1, 2.5, 2000, 500, 100, 70, 30, 3, 1)`) //nolint:errcheck
	for range 2 {
		if err := sampler.Sample(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if counter.calls != 1 {
		t.Fatalf("storage was counted %d times, want once an hour", counter.calls)
	}
	var old int
	pool.QueryRow(ctx, `SELECT count(*) FROM server_samples WHERE at < now() - interval '30 days'`).Scan(&old) //nolint:errcheck
	if old != 0 {
		t.Fatal("readings older than 30 days must go")
	}

	code, b = call(f.r, "GET", "/admin/system", f.adminT, nil)
	if code != 200 {
		t.Fatalf("system = %d %v", code, b)
	}
	server, store := b["server"].(map[string]any), b["storage"].(map[string]any)
	disk := server["disk"].(map[string]any)
	kinds := store["byKind"].(map[string]any)
	if disk["usedPercent"] != 84.8 || server["memory"].(map[string]any)["usedPercent"] != float64(40) || server["swap"].(map[string]any)["usedPercent"] != float64(20) || server["load"].(map[string]any)["15m"] != 0.6 {
		t.Fatalf("server = %v", server)
	}
	if store["objectBytes"] != float64(458) || store["objects"] != float64(16) || kinds["audioOriginals"].(map[string]any)["bytes"] != float64(150) || kinds["audioPlayback"].(map[string]any)["bytes"] != float64(300) ||
		kinds["other"].(map[string]any)["bytes"] != float64(1) || store["databaseBytes"].(float64) <= 0 || store["otherBytes"].(float64) <= 0 {
		t.Fatalf("storage = %v", store)
	}
	top := b["topArtists"].([]any)[0].(map[string]any)
	if top["artistId"] != art || top["tracks"] != float64(1) || top["bytes"] != float64(900<<30+3000) {
		t.Fatalf("top artist = %v", top)
	}
	if got := codes(b); got["worker_silent"] != "" || got["memory_low"] != "" || got["load_high"] != "" {
		t.Fatalf("alerts = %v", got)
	}

	// trouble
	machine.DiskUsed, machine.DiskFree, machine.MemAvail, machine.Load15 = 45*gib, gib, 100, 3
	upload := ""
	pool.QueryRow(ctx, `SELECT id FROM uploads WHERE release_id = $1`, rel).Scan(&upload)                                    //nolint:errcheck
	pool.Exec(ctx, `UPDATE uploads SET status = 'queued', updated_at = now() - interval '45 minutes' WHERE id = $1`, upload) //nolint:errcheck
	_, b = call(f.r, "GET", "/admin/system", f.adminT, nil)
	if got := codes(b); got["disk_low"] != "critical" || got["memory_low"] != "critical" || got["load_high"] != "warning" || got["queue_slow"] != "warning" {
		t.Fatalf("alerts = %v", got)
	}

	// history
	code, b = call(f.r, "GET", "/admin/system/history?hours=6", f.adminT, nil)
	points, _ := b["points"].([]any)
	if code != 200 || b["stepSeconds"] != float64(300) || len(points) < 2 {
		t.Fatalf("history = %d %v", code, b)
	}
	first := points[0].(map[string]any)
	if first["load"] != 2.5 || first["memoryUsedPercent"] != float64(75) || first["diskUsedBytes"] != float64(70) || first["uploadsWaiting"] != float64(4) {
		t.Fatalf("first point = %v", first)
	}
	if code, b = call(f.r, "GET", "/admin/system/history?hours=720", f.adminT, nil); code != 200 || b["stepSeconds"] != float64(8640) {
		t.Fatalf("a month = %d %v", code, b)
	}
	for _, bad := range []string{"0", "721", "x"} {
		if code, _ = call(f.r, "GET", "/admin/system/history?hours="+bad, f.adminT, nil); code != 422 {
			t.Errorf("hours=%s = %d", bad, code)
		}
	}
	// administrators only
	user := testutil.Token(t, testutil.User(t, pool, "user", true))
	for _, path := range []string{"/admin/system", "/admin/system/history"} {
		if code, _ = call(f.r, "GET", path, user, nil); code != 403 {
			t.Errorf("%s for a user = %d", path, code)
		}
	}
}
