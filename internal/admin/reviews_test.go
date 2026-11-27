package admin

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/credits"
	"github.com/blacmusic/backend/internal/lyrics"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/review"
	"github.com/blacmusic/backend/internal/testutil"
	"github.com/blacmusic/backend/internal/upload"
)

// studio is an artist publishing with review switched on, and the team deciding.
type studio struct {
	*world
	owner, artist string
	words         *lyrics.Store
	names         *credits.Store
	uploads       *upload.Store
}

func newStudio(t *testing.T) *studio {
	w := newWorld(t)
	s := &studio{world: w, words: lyrics.NewStore(w.h.pool), names: credits.NewStore(w.h.pool), uploads: upload.NewStore(w.h.pool)}
	w.rel.Review, s.words.Review, s.names.Review, s.uploads.Review = true, true, true, true
	s.owner = testutil.User(t, w.h.pool, "user", true)
	s.artist = testutil.Artist(t, w.h.pool, s.owner, testutil.Unique("Reviewed"), testutil.GiB)
	// their phone, so notifications have somewhere to go
	w.exec(`INSERT INTO push_tokens (token, user_id, platform) VALUES ($1, $2, 'android')`, testutil.Unique("ExponentPushToken"), s.owner)
	return s
}

func (s *studio) draft(typ string, songs int) (string, []string) {
	s.t.Helper()
	rel := testutil.Release(s.t, s.h.pool, s.artist, "First title", typ, "draft")
	var ids []string
	for range songs {
		id, _ := testutil.ReadyTrack(s.t, s.h.pool, s.owner, s.artist, rel, "Song", 100, "draft")
		s.exec(`UPDATE tracks SET duration_ms = 60000 WHERE id = $1`, id)
		ids = append(ids, id)
	}
	return rel, ids
}

// seen is the release as a listener gets it; nil when they cannot.
func (s *studio) seen(rel string) *release.Release {
	r, err := s.rel.Get(context.Background(), rel, testutil.User(s.t, s.h.pool, "user", true))
	if errors.Is(err, release.ErrNotFound) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return r
}

func (s *studio) mine(rel string) *release.Release {
	s.t.Helper()
	r, err := s.rel.Get(context.Background(), rel, s.owner)
	if err != nil {
		s.t.Fatal(err)
	}
	return r
}

// waiting is the admin's view of what waits for a release.
func (s *studio) waiting(rel string) map[string]any {
	s.t.Helper()
	id := s.text(`SELECT id FROM release_reviews WHERE release_id = $1 ORDER BY seq DESC LIMIT 1`, rel)
	code, b := call(s.r, "GET", "/admin/reviews/"+id, s.adminT, nil)
	if code != 200 {
		s.t.Fatalf("detail = %d %v", code, b)
	}
	return b
}

func (s *studio) decide(d map[string]any, what, reason string) (int, map[string]any) {
	return call(s.r, "POST", "/admin/reviews/"+d["id"].(string)+"/"+what, s.adminT, map[string]any{"revision": d["revision"], "reason": reason})
}

// mail counts the artist's emails whose subject starts with prefix.
func (s *studio) mail(prefix string) string {
	return s.text(`SELECT count(*)::text FROM email_messages WHERE user_id = $1 AND subject LIKE $2 || '%'`, s.owner, prefix)
}

func (s *studio) pushes() string {
	return s.text(`SELECT count(*)::text FROM push_messages WHERE user_id = $1`, s.owner)
}

func asText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestANewReleaseWaitsForApproval(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	rel, songs := s.draft("album", 2)

	got, err := s.rel.Publish(ctx, rel, s.owner, nil)
	if err != nil || got.Visibility != "draft" || got.Review == nil || got.Review.Status != "pending" || got.Review.Kind != "new" || !got.ReviewRequired {
		t.Fatalf("publish = %+v %v", got, err)
	}
	if s.seen(rel) != nil || s.text(`SELECT count(*)::text FROM tracks WHERE album_id = $1 AND visibility = 'public'`, rel) != "0" {
		t.Fatal("nothing may be public before approval")
	}
	if s.mail("We're verifying First title") != "1" || s.pushes() != "0" {
		t.Fatal("sending it gives one email and no notification")
	}

	// locked while it is checked
	title, text := "Changed", "la"
	_, e1 := s.rel.Update(ctx, rel, s.owner, release.Patch{Title: &title})
	_, e2 := s.rel.UpdateTrack(ctx, songs[0], s.owner, release.TrackPatch{Title: &title})
	_, e3 := s.words.Put(ctx, songs[0], s.owner, lyrics.Input{Text: &text})
	_, e4 := s.names.Replace(ctx, rel, s.owner, []credits.Input{{Name: "P", Role: "producer"}})
	_, e5 := s.uploads.Create(ctx, upload.NewUpload{UserID: s.owner, ReleaseID: rel, Title: "More", Filename: "a.mp3", SizeBytes: 10, TermsVersion: "t"})
	for i, e := range []error{e1, e2, e3, e4, e5} {
		if !errors.Is(e, review.ErrPending) {
			t.Fatalf("change %d while waiting = %v", i+1, e)
		}
	}

	first := s.waiting(rel)
	_, list := call(s.r, "GET", "/admin/reviews", s.adminT, nil)
	_, overview := call(s.r, "GET", "/admin/stats/overview", s.adminT, nil)
	if first["kind"] != "new" || len(first["tracks"].([]any)) != 2 || !strings.Contains(asText(list["reviews"]), rel) || list["pending"].(float64) < 1 ||
		overview["reviews"].(map[string]any)["pending"].(float64) < 1 {
		t.Fatalf("queue = %s %s", asText(first), asText(overview["reviews"]))
	}

	// withdrawn: a decision on it is refused
	if err := s.rel.Withdraw(ctx, rel, s.owner); err != nil || s.mine(rel).Review != nil {
		t.Fatalf("withdraw = %v", err)
	}
	if code, b := s.decide(first, "approve", ""); code != 409 || errCode(b) != "not_pending" {
		t.Fatalf("approving what was withdrawn = %d %v", code, b)
	}
	if err := s.rel.Withdraw(ctx, rel, s.owner); !errors.Is(err, release.ErrNotPending) {
		t.Fatalf("withdraw again = %v", err)
	}

	// changed after it was opened: refused too
	if _, err := s.rel.Publish(ctx, rel, s.owner, nil); err != nil {
		t.Fatal(err)
	}
	opened := s.waiting(rel)
	soon := time.Now().Add(48 * time.Hour)
	if _, err := s.rel.Publish(ctx, rel, s.owner, &soon); err != nil {
		t.Fatal(err)
	}
	if code, b := s.decide(opened, "approve", ""); code != 409 || errCode(b) != "review_changed" {
		t.Fatalf("approving an older revision = %d %v", code, b)
	}

	// refused, with why
	now := s.waiting(rel)
	if code, b := s.decide(now, "reject", "x"); code != 422 {
		t.Fatalf("a reason is needed = %d %v", code, b)
	}
	if code, b := s.decide(now, "reject", "The cover shows another artist"); code != 200 || b["status"] != "rejected" {
		t.Fatalf("reject = %d %v", code, b)
	}
	mine := s.mine(rel)
	if mine.Visibility != "draft" || mine.Review == nil || mine.Review.Status != "rejected" || mine.Review.Reason != "The cover shows another artist" ||
		s.mail("First title needs changes") != "1" || s.pushes() != "1" {
		t.Fatalf("after reject = %+v", mine.Review)
	}

	// sent again with a time ahead: approved, it is scheduled, and goes live when due
	if _, err := s.rel.Publish(ctx, rel, s.owner, &soon); err != nil || s.mine(rel).Review.Status != "pending" {
		t.Fatalf("again = %v", err)
	}
	if code, b := s.decide(s.waiting(rel), "approve", ""); code != 200 {
		t.Fatalf("approve = %d %v", code, b)
	}
	if r := s.mine(rel); r.Visibility != "scheduled" || r.Review != nil || s.seen(rel) != nil || s.mail("First title is approved") != "1" {
		t.Fatalf("scheduled = %+v", r)
	}
	s.exec(`UPDATE albums SET publish_at = now() - interval '1 minute' WHERE id = $1`, rel)
	if _, err := s.rel.PublishDue(ctx); err != nil || s.seen(rel) == nil || len(s.seen(rel).Tracks) != 2 {
		t.Fatalf("due = %v", err)
	}
	if s.mail("Your release is live") != "0" {
		t.Fatal("approval is the one message until it goes live (the test store sends no live hook)")
	}
	if s.audit("review.approve", rel) != 1 || s.audit("review.reject", rel) != 1 {
		t.Fatal("decisions must be recorded")
	}
}

func TestChangesToALiveReleaseWaitForApproval(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	rel, songs := s.draft("album", 2)
	if _, err := s.rel.Publish(ctx, rel, s.owner, nil); err != nil {
		t.Fatal(err)
	}
	if code, b := s.decide(s.waiting(rel), "approve", ""); code != 200 || s.seen(rel) == nil {
		t.Fatalf("approve = %d %v", code, b)
	}
	listener := testutil.User(t, s.h.pool, "user", true)
	heard := func() (string, string, string) {
		t.Helper()
		r := s.seen(rel)
		l, err := s.words.Get(ctx, songs[0], listener)
		words := ""
		if err == nil {
			words = l.Text
		}
		names, err := s.names.ForRelease(ctx, rel, listener)
		if err != nil {
			t.Fatal(err)
		}
		var who []string
		for _, c := range names {
			who = append(who, c.Name)
		}
		return r.Title + "/" + r.Tracks[0].Title, words, strings.Join(who, ",")
	}

	// changing something and changing it back leaves nothing waiting
	other, first := "Second thoughts", "First title"
	for _, v := range []*string{&other, &first} {
		if _, err := s.rel.Update(ctx, rel, s.owner, release.Patch{Title: v}); err != nil {
			t.Fatal(err)
		}
	}
	if r := s.mine(rel); r.Review != nil {
		t.Fatalf("nothing to check = %+v", r.Review)
	}

	newTitle, songTitle, text := "Better title", "Better song", "Words to check"
	changed, err := s.rel.Update(ctx, rel, s.owner, release.Patch{Title: &newTitle})
	if err != nil || changed.Title != newTitle || changed.Review == nil || changed.Review.Kind != "update" {
		t.Fatalf("the owner sees the edit waiting = %+v %v", changed, err)
	}
	if v, err := s.rel.UpdateTrack(ctx, songs[0], s.owner, release.TrackPatch{Title: &songTitle}); err != nil || v.Title != songTitle {
		t.Fatalf("track = %+v %v", v, err)
	}
	if _, err := s.words.Put(ctx, songs[0], s.owner, lyrics.Input{Text: &text}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.names.Replace(ctx, rel, s.owner, []credits.Input{{Name: "New Producer", Role: "producer"}}); err != nil {
		t.Fatal(err)
	}
	if title, words, who := heard(); title != "First title/Song" || words != "" || who != "" {
		t.Fatalf("listeners keep the approved version: %q %q %q", title, words, who)
	}
	l, info, err := s.words.ForOwner(ctx, songs[0], s.owner)
	list, creditInfo, cerr := s.names.OwnerList(ctx, rel, s.owner)
	if err != nil || l.Text != text || info == nil || info.Status != "pending" || cerr != nil || creditInfo == nil || len(list) != 1 || list[0].Name != "New Producer" {
		t.Fatalf("the owner continues from what they sent: %v %+v %v", l, list, cerr)
	}
	wantLabels := []string{"Title", "Track: Song", "Lyrics: Song", "Credits"}
	if got := s.mine(rel).Review.Changes; !slices.Equal(got, wantLabels) {
		t.Fatalf("labels = %v", got)
	}
	// the new release, the edit that was taken back, and these changes (one email, though there are four)
	if s.mail("We're verifying") != "3" {
		t.Fatalf("an email each time something starts waiting, not for each change: %s", s.mail("We're verifying"))
	}
	d := s.waiting(rel)
	all := asText(d["changeItems"])
	if d["kind"] != "update" || len(d["changeItems"].([]any)) != 4 || !strings.Contains(all, `"approved":"First title"`) || !strings.Contains(all, `"proposed":"Better title"`) {
		t.Fatalf("detail = %s", all)
	}
	if code, b := s.decide(d, "approve", ""); code != 200 {
		t.Fatalf("approve = %d %v", code, b)
	}
	if title, words, who := heard(); title != "Better title/Better song" || words != text || who != "New Producer" || s.mail("Your changes to Better title are approved") != "1" {
		t.Fatalf("after approval: %q %q %q", title, words, who)
	}

	// refused changes never show, and the owner is told why
	worse := "Worse title"
	if _, err := s.rel.Update(ctx, rel, s.owner, release.Patch{Title: &worse}); err != nil {
		t.Fatal(err)
	}
	newText := "Rude words"
	if _, err := s.words.Put(ctx, songs[0], s.owner, lyrics.Input{Text: &newText}); err != nil {
		t.Fatal(err)
	}
	if code, b := s.decide(s.waiting(rel), "reject", "Please keep it clean"); code != 200 {
		t.Fatalf("reject = %d %v", code, b)
	}
	if title, words, _ := heard(); title != "Better title/Better song" || words != text {
		t.Fatalf("after rejection: %q %q", title, words)
	}
	l, info, err = s.words.ForOwner(ctx, songs[0], s.owner)
	if mine := s.mine(rel); err != nil || l.Text != text || info == nil || info.Status != "rejected" || mine.Title != "Better title" || mine.Review.Reason != "Please keep it clean" {
		t.Fatalf("owner after rejection = %v %+v", l, info)
	}

	// taking something down never waits
	if _, err := s.names.Replace(ctx, rel, s.owner, []credits.Input{}); err != nil {
		t.Fatal(err)
	}
	if err := s.words.Delete(ctx, songs[0], s.owner); err != nil {
		t.Fatal(err)
	}
	if _, words, who := heard(); words != "" || who != "" {
		t.Fatalf("removal is immediate: %q %q", words, who)
	}

	// a song added now stays hidden until approved
	u, err := s.uploads.Create(ctx, upload.NewUpload{UserID: s.owner, ReleaseID: rel, Title: "Late song", Filename: "a.mp3", SizeBytes: 10, TermsVersion: "t"})
	if err != nil {
		t.Fatal(err)
	}
	s.exec(`UPDATE uploads SET status = 'processing' WHERE id = $1`, u.ID)
	if err := s.uploads.Finish(ctx, u, upload.Result{DurationMs: 1000, Renditions: []upload.Rendition{{Quality: "medium", MediaKey: "tracks/late/" + u.ID, BitrateKbps: 128, SizeBytes: 1}}}); err != nil {
		t.Fatal(err)
	}
	if n := len(s.seen(rel).Tracks); n != 2 {
		t.Fatalf("listeners see %d songs before approval", n)
	}
	d = s.waiting(rel)
	if !strings.Contains(asText(d["changeItems"]), "New track: Late song") || !strings.Contains(asText(d["tracks"]), `"isNew":true`) {
		t.Fatalf("new track = %s", asText(d))
	}
	if code, b := s.decide(d, "approve", ""); code != 200 || len(s.seen(rel).Tracks) != 3 {
		t.Fatalf("approve the new song = %d %v", code, b)
	}

	// back to draft: what waited goes onto the draft, and publishing again is a new submission
	last := "Last title"
	if _, err := s.rel.Update(ctx, rel, s.owner, release.Patch{Title: &last}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.rel.Unpublish(ctx, rel, s.owner); err != nil || got.Title != last || got.Review != nil {
		t.Fatalf("unpublish = %+v %v", got, err)
	}
	if got, err := s.rel.Publish(ctx, rel, s.owner, nil); err != nil || got.Visibility != "draft" || got.Review.Kind != "new" {
		t.Fatalf("publish again = %+v %v", got, err)
	}
	// the team publishing it settles the matter
	if err := s.rel.AdminPublish(ctx, rel, nil, nil); err != nil || s.seen(rel) == nil || s.mine(rel).Review != nil {
		t.Fatalf("admin publish = %v", err)
	}
}
