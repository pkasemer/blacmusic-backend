package image

import (
	"bytes"
	"context"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/blacmusic/backend/internal/blobtest"
)

func TestMosaic(t *testing.T) {
	const base = "http://api.test"
	blobs := blobtest.New()
	h := NewHandler(nil, blobs, base)
	colours := []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {255, 255, 255, 255}}
	var covers []string
	for i, c := range colours {
		id := "img_" + strings.Repeat(string(rune('a'+i)), 32)
		img := stdimage.NewRGBA(stdimage.Rect(0, 0, 640, 480)) // not square: its middle is used
		for p := 0; p < len(img.Pix); p += 4 {
			img.Pix[p], img.Pix[p+1], img.Pix[p+2], img.Pix[p+3] = c.R, c.G, c.B, 255
		}
		path := filepath.Join(t.TempDir(), "c.jpg")
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, nil); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := blobs.UploadFile(context.Background(), variantKey(id, SizeMedium), path, "image/jpeg"); err != nil {
			t.Fatal(err)
		}
		covers = append(covers, URL(base, id, SizeMedium))
	}

	medium, large, ok := MosaicURLs(base, covers)
	if !ok || !strings.HasSuffix(medium, "/medium.jpg") || !strings.HasSuffix(large, "/large.jpg") {
		t.Fatalf("urls = %q %q %v", medium, large, ok)
	}
	swapped, _, _ := MosaicURLs(base, []string{covers[1], covers[0], covers[2], covers[3]})
	if swapped == medium {
		t.Fatal("other covers must have another address")
	}
	for _, bad := range [][]string{covers[:3], {covers[0], covers[1], covers[2], "https://elsewhere.test/a.jpg"}} {
		if _, _, ok := MosaicURLs(base, bad); ok {
			t.Fatalf("no mosaic of %v", bad)
		}
	}

	r := chi.NewRouter()
	r.Route("/v1/media", h.ServeRoute)
	get := func(url string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest("GET", strings.TrimPrefix(url, base), nil))
		return rec
	}
	for url, side := range map[string]int{medium: 640, large: 1280} {
		rec := get(url)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || !strings.Contains(rec.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("get %s = %d %v", url, rec.Code, rec.Header())
		}
		img, err := jpeg.Decode(rec.Body)
		if err != nil || img.Bounds().Dx() != side || img.Bounds().Dy() != side {
			t.Fatalf("picture = %v %v", err, img.Bounds())
		}
		for i, c := range colours {
			cr, cg, cb, _ := img.At(side/4+i%2*side/2, side/4+i/2*side/2).RGBA()
			if abs(int(cr>>8)-int(c.R)) > 8 || abs(int(cg>>8)-int(c.G)) > 8 || abs(int(cb>>8)-int(c.B)) > 8 {
				t.Fatalf("tile %d at %d = %d %d %d", i, side, cr>>8, cg>>8, cb>>8)
			}
		}
	}
	for _, url := range []string{strings.Replace(medium, "medium", "small", 1), strings.Replace(medium, "img_a", "img_f", 1), base + "/v1/media/mosaic/nope/medium.jpg"} {
		if rec := get(url); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404", url, rec.Code)
		}
	}
}

func abs(n int) int { return max(n, -n) }
