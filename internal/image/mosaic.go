package image

import (
	"bytes"
	"context"
	"errors"
	stdimage "image"
	"image/jpeg"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/go-chi/chi/v5"
	xdraw "golang.org/x/image/draw"

	"github.com/blacmusic/backend/internal/storage"
)

// A mosaic is four covers as one square picture, two by two: the cover of a playlist that has none of its own. Its
// address names the four, so it changes when they do and the picture behind an address never does. Mosaics are made
// when asked for and kept in memory only: any four covers make an address, so storing them would let a caller fill
// the disk.

const (
	mosaicTiles     = 4
	mosaicCacheSize = 16 << 20 // bytes of finished mosaics kept
	mosaicAtOnce    = 2        // each takes some 15 MB while being made
)

// MosaicURLs gives the picture made of four covers, at 640 and 1280 px. It needs all four to be pictures of ours.
func MosaicURLs(publicBase string, covers []string) (medium, large string, ok bool) {
	if len(covers) != mosaicTiles {
		return "", "", false
	}
	ids := make([]string, mosaicTiles)
	for i, u := range covers {
		rest, ours := strings.CutPrefix(u, publicBase+"/v1/media/")
		id, file, found := strings.Cut(rest, "/")
		if !ours || !found || !strings.HasSuffix(file, ".jpg") || !idRe.MatchString(id) {
			return "", "", false
		}
		ids[i] = id
	}
	base := publicBase + "/v1/media/mosaic/" + strings.Join(ids, "-") + "/"
	return base + SizeMedium + ".jpg", base + SizeLarge + ".jpg", true
}

type mosaicCache struct {
	mu    sync.Mutex
	made  map[string][]byte
	order []string // oldest first
	bytes int
	busy  chan struct{}
}

func newMosaicCache() *mosaicCache {
	return &mosaicCache{made: map[string][]byte{}, busy: make(chan struct{}, mosaicAtOnce)}
}

func (c *mosaicCache) get(key string) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.made[key]
}

func (c *mosaicCache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, there := c.made[key]; there || len(data) > mosaicCacheSize {
		return
	}
	c.made[key], c.order, c.bytes = data, append(c.order, key), c.bytes+len(data)
	for c.bytes > mosaicCacheSize {
		oldest := c.order[0]
		c.order, c.bytes = c.order[1:], c.bytes-len(c.made[oldest])
		delete(c.made, oldest)
	}
}

// serveMosaic is GET /v1/media/mosaic/{id-id-id-id}/{medium|large}.jpg.
func (h *Handler) serveMosaic(w http.ResponseWriter, r *http.Request) {
	ids := strings.Split(chi.URLParam(r, "ids"), "-")
	size, ok := strings.CutSuffix(chi.URLParam(r, "file"), ".jpg")
	if !ok || len(ids) != mosaicTiles || (size != SizeMedium && size != SizeLarge) {
		http.NotFound(w, r)
		return
	}
	for _, id := range ids {
		if !idRe.MatchString(id) {
			http.NotFound(w, r)
			return
		}
	}
	key := strings.Join(ids, "-") + "-" + size
	etag := `"` + key + `"`
	data := h.mosaics.get(key)
	if data == nil {
		var err error
		if data, err = h.makeMosaic(r.Context(), key, ids, sizePixels[size]); err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				http.NotFound(w, r)
				return
			}
			if r.Context().Err() == nil {
				slog.Error("make mosaic", "err", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
			return
		}
	}
	hd := w.Header()
	hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	hd.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	hd.Set("Content-Type", "image/jpeg")
	hd.Set("Content-Length", strconv.Itoa(len(data)))
	hd.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(data)
}

func (h *Handler) makeMosaic(ctx context.Context, key string, ids []string, side int) ([]byte, error) {
	select {
	case h.mosaics.busy <- struct{}{}:
		defer func() { <-h.mosaics.busy }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if data := h.mosaics.get(key); data != nil { // made while this request waited
		return data, nil
	}
	canvas := stdimage.NewRGBA(stdimage.Rect(0, 0, side, side))
	half := side / 2
	for i, id := range ids {
		if err := h.drawTile(ctx, canvas, id, stdimage.Rect(i%2*half, i/2*half, i%2*half+half, i/2*half+half)); err != nil {
			return nil, err
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, err
	}
	h.mosaics.put(key, buf.Bytes())
	return buf.Bytes(), nil
}

// drawTile fills at with the middle square of a stored picture.
func (h *Handler) drawTile(ctx context.Context, canvas *stdimage.RGBA, id string, at stdimage.Rectangle) error {
	body, _, err := h.blobs.Open(ctx, variantKey(id, SizeMedium))
	if err != nil {
		return err
	}
	defer body.Close()
	img, err := jpeg.Decode(io.LimitReader(body, MaxUploadBytes))
	if err != nil {
		return err
	}
	b := img.Bounds()
	cut := min(b.Dx(), b.Dy())
	from := stdimage.Rect(0, 0, cut, cut).Add(stdimage.Point{X: b.Min.X + (b.Dx()-cut)/2, Y: b.Min.Y + (b.Dy()-cut)/2})
	xdraw.CatmullRom.Scale(canvas, at, img, from, xdraw.Src, nil)
	return nil
}
