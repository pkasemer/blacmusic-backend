// Package image accepts avatars and cover art: it validates what the client
// uploaded, re-encodes it into three JPEG sizes (dropping EXIF and any other
// embedded data) and serves them.
package image

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	stdimage "image"
	"image/color"
	"image/jpeg"
	_ "image/png" // register the PNG decoder
	"regexp"

	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // register the WebP decoder

	"github.com/jackc/pgx/v5"
)

// Stored sizes (longest side, pixels).
const (
	SizeSmall  = "small"
	SizeMedium = "medium"
	SizeLarge  = "large"
)

var sizePixels = map[string]int{SizeSmall: 300, SizeMedium: 640, SizeLarge: 1280}

// Sizes lists the stored sizes in ascending order.
var Sizes = []string{SizeSmall, SizeMedium, SizeLarge}

const (
	// MaxUploadBytes is the largest raw image accepted.
	MaxUploadBytes = 10 << 20
	maxSide        = 6000
	maxPixels      = 25_000_000
	minSide        = 200
	jpegQuality    = 85
)

var idRe = regexp.MustCompile(`^img_[0-9a-f]{32}$`)

// URL is the public link to one stored size of an image.
func URL(publicBase, id, size string) string {
	return publicBase + "/v1/media/" + id + "/" + size + ".jpg"
}

// Querier is satisfied by *pgxpool.Pool and pgx.Tx.
type Querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// CheckOwned verifies that a finished image of the given purpose belongs to the user.
func CheckOwned(ctx context.Context, q Querier, imageID, ownerID, purpose string) error {
	if !idRe.MatchString(imageID) {
		return errors.New("image: not found")
	}
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM images WHERE id = $1 AND owner_id = $2 AND purpose = $3 AND status = 'ready')`,
		imageID, ownerID, purpose).Scan(&ok)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("image: not found")
	}
	return nil
}

// InvalidImageError means the upload is not an acceptable image; the message is safe to show.
type InvalidImageError struct{ Msg string }

func (e *InvalidImageError) Error() string { return e.Msg }

// Processed is the result of Process.
type Processed struct {
	Width, Height int               // of the original after orientation
	Variants      map[string][]byte // size name -> JPEG bytes
}

// Process validates raw image bytes and produces the stored sizes.
func Process(raw []byte) (*Processed, error) {
	cfg, format, err := stdimage.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, &InvalidImageError{"this file is not a JPEG, PNG or WebP image"}
	}
	// Check the declared size BEFORE decoding, so a tiny file that claims to be
	// enormous cannot exhaust memory.
	if cfg.Width > maxSide || cfg.Height > maxSide || cfg.Width*cfg.Height > maxPixels {
		return nil, &InvalidImageError{fmt.Sprintf("the image is too large (at most %d px on a side, 25 megapixels)", maxSide)}
	}
	if cfg.Width < minSide || cfg.Height < minSide {
		return nil, &InvalidImageError{fmt.Sprintf("the image is too small (at least %d px on each side)", minSide)}
	}
	img, _, err := stdimage.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, &InvalidImageError{"the image file is damaged"}
	}
	if format == "jpeg" {
		img = orient(img, jpegOrientation(raw))
	}
	flat := flatten(img)

	b := flat.Bounds()
	out := &Processed{Width: b.Dx(), Height: b.Dy(), Variants: map[string][]byte{}}
	for _, name := range Sizes {
		scaled := fit(flat, sizePixels[name])
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
			return nil, err
		}
		out.Variants[name] = buf.Bytes()
	}
	return out, nil
}

// flatten draws img onto white so transparency does not turn black in a JPEG.
func flatten(img stdimage.Image) *stdimage.RGBA {
	b := img.Bounds()
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, b.Dx(), b.Dy()))
	xdraw.Draw(dst, dst.Bounds(), stdimage.NewUniform(color.White), stdimage.Point{}, xdraw.Src)
	xdraw.Draw(dst, dst.Bounds(), img, b.Min, xdraw.Over)
	return dst
}

// fit scales img so its longest side is at most side, never enlarging it.
func fit(img *stdimage.RGBA, side int) *stdimage.RGBA {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= side && h <= side {
		return img
	}
	nw, nh := side, side
	if w >= h {
		nh = max(1, h*side/w)
	} else {
		nw = max(1, w*side/h)
	}
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, nw, nh))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, xdraw.Src, nil)
	return dst
}

// jpegOrientation reads the EXIF orientation (1-8) of a JPEG, or 1 when absent.
// Phones store portrait photos sideways plus an orientation tag; since we drop
// EXIF, the pixels must be rotated first.
func jpegOrientation(b []byte) int {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return 1
	}
	i := 2
	for i+4 <= len(b) {
		if b[i] != 0xFF {
			return 1
		}
		marker := b[i+1]
		if marker == 0xD9 || marker == 0xDA { // end of image / start of scan
			return 1
		}
		segLen := int(binary.BigEndian.Uint16(b[i+2 : i+4]))
		if segLen < 2 || i+2+segLen > len(b) {
			return 1
		}
		if marker == 0xE1 && segLen >= 16 && string(b[i+4:i+10]) == "Exif\x00\x00" {
			return tiffOrientation(b[i+10 : i+2+segLen])
		}
		i += 2 + segLen
	}
	return 1
}

func tiffOrientation(t []byte) int {
	if len(t) < 8 {
		return 1
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 1
	}
	off := int(bo.Uint32(t[4:8]))
	if off < 8 || off+2 > len(t) {
		return 1
	}
	n := int(bo.Uint16(t[off : off+2]))
	for k := 0; k < n; k++ {
		e := off + 2 + k*12
		if e+12 > len(t) {
			return 1
		}
		if bo.Uint16(t[e:e+2]) == 0x0112 { // Orientation
			if v := int(bo.Uint16(t[e+8 : e+10])); v >= 1 && v <= 8 {
				return v
			}
			return 1
		}
	}
	return 1
}

// orient applies an EXIF orientation so the returned image is upright.
func orient(img stdimage.Image, o int) stdimage.Image {
	if o <= 1 || o > 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	swap := o >= 5
	dw, dh := w, h
	if swap {
		dw, dh = h, w
	}
	dst := stdimage.NewRGBA(stdimage.Rect(0, 0, dw, dh))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var nx, ny int
			switch o {
			case 2: // mirrored horizontally
				nx, ny = w-1-x, y
			case 3: // rotated 180
				nx, ny = w-1-x, h-1-y
			case 4: // mirrored vertically
				nx, ny = x, h-1-y
			case 5: // transposed
				nx, ny = y, x
			case 6: // rotated 90 clockwise
				nx, ny = h-1-y, x
			case 7: // transverse
				nx, ny = h-1-y, w-1-x
			case 8: // rotated 90 counter-clockwise
				nx, ny = y, w-1-x
			}
			dst.Set(nx, ny, img.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return dst
}
