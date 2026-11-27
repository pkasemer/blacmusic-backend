package admin

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blacmusic/backend/internal/artist"
	"github.com/blacmusic/backend/internal/auth"
	"github.com/blacmusic/backend/internal/profile"
	"github.com/blacmusic/backend/internal/release"
	"github.com/blacmusic/backend/internal/testutil"
)

func TestArtistProfilesWaitForApproval(t *testing.T) {
	s := newStudio(t)
	ctx := context.Background()
	artists := s.h.artists
	artists.Review = true
	listener := testutil.User(t, s.h.pool, "user", true)

	// a new profile is its owner's alone until approved
	owner := testutil.User(t, s.h.pool, "user", true)
	s.owner = owner
	name := testutil.Unique("Fresh Voice")
	a, err := artists.Create(ctx, owner, name, "Born in Gulu")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s.exec(`DELETE FROM albums WHERE artist_id = $1`, a.ID)
		s.exec(`DELETE FROM artists WHERE id = $1`, a.ID)
	})
	if _, err := artists.Get(ctx, a.ID, listener); !errors.Is(err, artist.ErrNotFound) {
		t.Fatalf("hidden profile = %v", err)
	}
	if err := artists.Follow(ctx, listener, a.ID); !errors.Is(err, artist.ErrNotFound) {
		t.Fatalf("follow a hidden profile = %v", err)
	}
	info, _, err := artists.ReviewOf(ctx, a.ID)
	if err != nil || info == nil || info.Kind != "new" || info.Status != "pending" || s.mail("We're verifying your artist profile") != "1" {
		t.Fatalf("new profile review = %+v %v", info, err)
	}
	if err := artists.WithdrawProfile(ctx, owner); !errors.Is(err, artist.ErrNotWithdrawable) {
		t.Fatalf("withdraw a new profile = %v", err)
	}

	// their release cannot be approved before the profile
	rel := testutil.Release(t, s.h.pool, a.ID, "Debut", "single", "draft")
	testutil.ReadyTrack(t, s.h.pool, owner, a.ID, rel, "Debut", 1, "draft")
	if _, err := s.rel.Publish(ctx, rel, owner, nil); err != nil {
		t.Fatal(err)
	}
	first := s.waiting(rel)
	waiting := first["artistProfile"].(map[string]any)
	if waiting["approved"] != false || waiting["reviewId"] == nil {
		t.Fatalf("profile with the first release = %v", waiting)
	}
	if code, b := s.decide(first, "approve", ""); code != 409 || errCode(b) != "profile_pending" {
		t.Fatalf("release before profile = %d %v", code, b)
	}
	profileID := waiting["reviewId"].(string)
	open := func() map[string]any {
		code, b := call(s.r, "GET", "/admin/reviews/"+profileID, s.adminT, nil)
		if code != 200 {
			t.Fatalf("profile detail = %d %v", code, b)
		}
		return b
	}
	d := open()
	_, list := call(s.r, "GET", "/admin/reviews?subject=profile", s.adminT, nil)
	if d["subject"] != "profile" || d["proposed"].(map[string]any)["bio"] != "Born in Gulu" || !strings.Contains(asText(list), profileID) {
		t.Fatalf("profile = %s", asText(d))
	}
	if code, b := s.decide(d, "approve", ""); code != 200 {
		t.Fatalf("approve profile = %d %v", code, b)
	}
	if _, err := artists.Get(ctx, a.ID, listener); err != nil || s.mail("Your artist profile is approved") != "1" {
		t.Fatalf("approved profile = %v", err)
	}
	if code, b := s.decide(s.waiting(rel), "approve", ""); code != 200 {
		t.Fatalf("approve release = %d %v", code, b)
	}
	if _, err := s.rel.Get(ctx, rel, listener); errors.Is(err, release.ErrNotFound) {
		t.Fatal("the release should be live")
	}

	// changes: name, bio and picture wait; the owner's account shows the new picture at once
	s.exec(`UPDATE artists SET verification = 'verified' WHERE id = $1`, a.ID)
	old, fresh := s.picture(owner, "avatar"), s.picture(owner, "avatar")
	s.exec(`UPDATE users SET avatar_image_id = $2 WHERE id = $1`, owner, old)
	setPhoto := func(id string) string {
		t.Helper()
		h := profile.NewHandler(s.h.pool, s.svc)
		h.Review = true
		req := httptest.NewRequest("PATCH", "/me", bytes.NewBufferString(`{"avatarImageId":"`+id+`"}`))
		rec := httptest.NewRecorder()
		auth.Require(auth.NewTokenService(testutil.Secret, 60e9))(httpHandler(h.Update)).ServeHTTP(rec, withToken(req, testutil.Token(t, owner)))
		if rec.Code != 200 {
			t.Fatalf("set photo = %d %s", rec.Code, rec.Body)
		}
		// what the account itself is shown (GET /v1/me reads the same)
		return s.text(`SELECT COALESCE(next_avatar_image_id, avatar_image_id, '') FROM users WHERE id = $1`, owner)
	}
	shown := func() string {
		got, err := artists.Get(ctx, a.ID, listener)
		if err != nil {
			t.Fatal(err)
		}
		return got.Name + "|" + got.Bio + "|" + got.ImageURL
	}
	if body := setPhoto(fresh); !strings.Contains(body, fresh) {
		t.Fatalf("the account shows the new photo: %s", body)
	}
	newName, newBio := name+" II", "Now in Kampala"
	if _, err := artists.Update(ctx, a.ID, owner, artist.Patch{Name: &newName, Bio: &newBio}); err != nil {
		t.Fatal(err)
	}
	if got := shown(); !strings.HasPrefix(got, name+"|Born in Gulu|") || !strings.Contains(got, old) {
		t.Fatalf("listeners keep the approved profile: %s", got)
	}
	info, pending, err := artists.ReviewOf(ctx, a.ID)
	if err != nil || info.Kind != "update" || strings.Join(info.Changes, ",") != "Name,Photo,Bio" || *pending.Name != newName || !strings.Contains(pending.ImageURL, fresh) {
		t.Fatalf("pending = %+v %+v %v", info, pending, err)
	}
	profileID = s.text(`SELECT id FROM artist_reviews WHERE artist_id = $1 AND status = 'pending'`, a.ID)
	d = open()
	if len(d["changeItems"].([]any)) != 3 || s.text(`SELECT verification FROM artists WHERE id = $1`, a.ID) != "verified" {
		t.Fatalf("detail = %s", asText(d))
	}
	if code, b := s.decide(d, "approve", ""); code != 200 {
		t.Fatalf("approve changes = %d %v", code, b)
	}
	if got := shown(); !strings.HasPrefix(got, newName+"|"+newBio+"|") || !strings.Contains(got, fresh) || s.text(`SELECT verification FROM artists WHERE id = $1`, a.ID) != "unverified" {
		t.Fatalf("approved = %s", got)
	}

	// refused: nothing shows, the account goes back; removing never waits; withdrawing drops what waits
	third := s.picture(owner, "avatar")
	setPhoto(third)
	profileID = s.text(`SELECT id FROM artist_reviews WHERE artist_id = $1 AND status = 'pending'`, a.ID)
	if code, b := s.decide(open(), "reject", "Use a photo of yourself"); code != 200 {
		t.Fatalf("reject = %d %v", code, b)
	}
	if got := shown(); !strings.Contains(got, fresh) || s.text(`SELECT COALESCE(next_avatar_image_id, 'none') FROM users WHERE id = $1`, owner) != "none" ||
		s.mail("Your artist profile needs changes") != "1" {
		t.Fatalf("after reject = %s", got)
	}
	empty := ""
	if _, err := artists.Update(ctx, a.ID, owner, artist.Patch{Bio: &empty}); err != nil || !strings.HasPrefix(shown(), newName+"||") {
		t.Fatalf("removing the bio is immediate: %v %s", err, shown())
	}
	setPhoto("")
	if got := shown(); strings.Contains(got, fresh) {
		t.Fatalf("removing the photo is immediate: %s", got)
	}
	again := "Another name"
	if _, err := artists.Update(ctx, a.ID, owner, artist.Patch{Name: &again}); err != nil {
		t.Fatal(err)
	}
	if err := artists.WithdrawProfile(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if info, _, _ := artists.ReviewOf(ctx, a.ID); info != nil {
		t.Fatalf("withdrawn = %+v", info)
	}
	if err := artists.WithdrawProfile(ctx, owner); !errors.Is(err, artist.ErrNoReview) {
		t.Fatalf("withdraw again = %v", err)
	}
}
