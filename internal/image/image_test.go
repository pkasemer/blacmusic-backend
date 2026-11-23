package image

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	stdimage "image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

func solidPNG(t *testing.T, w, h int, c color.Color) []byte {
	t.Helper()
	img := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func decodeJPEG(t *testing.T, b []byte) stdimage.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("variant is not a JPEG: %v", err)
	}
	return img
}

func TestProcessProducesThreeSizesWithoutUpscaling(t *testing.T) {
	res, err := Process(solidPNG(t, 1000, 500, color.RGBA{200, 30, 30, 255}))
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 1000 || res.Height != 500 {
		t.Fatalf("original size = %dx%d", res.Width, res.Height)
	}
	want := map[string][2]int{SizeSmall: {300, 150}, SizeMedium: {640, 320}, SizeLarge: {1000, 500}} // large is not enlarged
	for name, dims := range want {
		b := decodeJPEG(t, res.Variants[name]).Bounds()
		if b.Dx() != dims[0] || b.Dy() != dims[1] {
			t.Errorf("%s = %dx%d, want %dx%d", name, b.Dx(), b.Dy(), dims[0], dims[1])
		}
	}
}

func TestProcessFlattensTransparencyOntoWhite(t *testing.T) {
	res, err := Process(solidPNG(t, 400, 400, color.NRGBA{0, 0, 0, 0})) // fully transparent
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decodeJPEG(t, res.Variants[SizeSmall]).At(10, 10).RGBA()
	if r>>8 < 250 || g>>8 < 250 || b>>8 < 250 {
		t.Fatalf("transparent pixel became %d,%d,%d, want white (JPEG has no alpha)", r>>8, g>>8, b>>8)
	}
}

func TestProcessAcceptsJPEGInput(t *testing.T) {
	src := stdimage.NewRGBA(stdimage.Rect(0, 0, 500, 500))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Process(buf.Bytes()); err != nil {
		t.Fatalf("jpeg: %v", err)
	}
}

func TestProcessRejectsBadImages(t *testing.T) {
	// A PNG header that claims 30000x30000: far over the limit, with no pixel data at all.
	// It must be refused from the header, before any decoding allocates memory.
	bomb := pngHeader(30000, 30000)

	cases := map[string][]byte{
		"text":         []byte("definitely not an image"),
		"empty":        nil,
		"too small":    solidPNG(t, 100, 100, color.White),
		"decomp. bomb": bomb,
		"truncated":    solidPNG(t, 400, 400, color.White)[:60],
		"svg":          []byte(`<svg xmlns="http://www.w3.org/2000/svg" width="500" height="500"><script>alert(1)</script></svg>`),
	}
	for name, data := range cases {
		_, err := Process(data)
		var bad *InvalidImageError
		if !errors.As(err, &bad) {
			t.Errorf("%s: err = %v, want InvalidImageError", name, err)
		}
	}
}

// pngHeader builds just the signature and IHDR chunk of a PNG with the given size.
func pngHeader(w, h uint32) []byte {
	var out bytes.Buffer
	out.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 0 // 8-bit grayscale
	chunk := append([]byte("IHDR"), ihdr...)
	binary.Write(&out, binary.BigEndian, uint32(len(ihdr))) //nolint:errcheck
	out.Write(chunk)
	binary.Write(&out, binary.BigEndian, crc32.ChecksumIEEE(chunk)) //nolint:errcheck
	return out.Bytes()
}

// exifJPEG returns a JPEG of the given size whose EXIF says it must be rotated.
func exifJPEG(t *testing.T, w, h, orientation int) []byte {
	t.Helper()
	src := stdimage.NewRGBA(stdimage.Rect(0, 0, w, h))
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, src, nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()

	tiff := []byte{'M', 'M', 0, 42, 0, 0, 0, 8, // big-endian header, IFD at offset 8
		0, 1, // one entry
		0x01, 0x12, 0, 3, 0, 0, 0, 1, byte(orientation >> 8), byte(orientation), 0, 0, // Orientation, SHORT, count 1
		0, 0, 0, 0} // no next IFD
	seg := append([]byte("Exif\x00\x00"), tiff...)
	app1 := []byte{0xFF, 0xE1, byte((len(seg) + 2) >> 8), byte(len(seg) + 2)}
	app1 = append(app1, seg...)
	return append(append(append([]byte{}, raw[:2]...), app1...), raw[2:]...) // SOI, APP1, rest
}

func TestExifOrientationIsAppliedBeforeMetadataIsDropped(t *testing.T) {
	// A 400x300 photo marked "rotate 90 clockwise" must come out 300x400.
	res, err := Process(exifJPEG(t, 400, 300, 6))
	if err != nil {
		t.Fatal(err)
	}
	if res.Width != 300 || res.Height != 400 {
		t.Fatalf("size after orientation = %dx%d, want 300x400", res.Width, res.Height)
	}
	if bytes.Contains(res.Variants[SizeLarge], []byte("Exif")) {
		t.Fatal("EXIF must not survive re-encoding (it can carry GPS coordinates)")
	}
	if res, err = Process(exifJPEG(t, 400, 300, 1)); err != nil || res.Width != 400 {
		t.Fatalf("orientation 1 must leave the image alone: %v %v", res, err)
	}
}

func TestOrientMovesPixelsCorrectly(t *testing.T) {
	// 3x2 image, top-left pixel red. Where does it go for each orientation?
	src := stdimage.NewRGBA(stdimage.Rect(0, 0, 3, 2))
	red := color.RGBA{255, 0, 0, 255}
	src.Set(0, 0, red)
	cases := map[int]struct{ w, h, x, y int }{
		1: {3, 2, 0, 0}, // as is
		2: {3, 2, 2, 0}, // mirrored horizontally
		3: {3, 2, 2, 1}, // rotated 180
		4: {3, 2, 0, 1}, // mirrored vertically
		5: {2, 3, 0, 0}, // transposed
		6: {2, 3, 1, 0}, // rotated 90 clockwise: top-left goes to top-right
		7: {2, 3, 1, 2}, // transverse
		8: {2, 3, 0, 2}, // rotated 90 counter-clockwise: top-left goes to bottom-left
	}
	for o, want := range cases {
		out := orient(src, o)
		b := out.Bounds()
		if b.Dx() != want.w || b.Dy() != want.h {
			t.Errorf("orientation %d: size %dx%d, want %dx%d", o, b.Dx(), b.Dy(), want.w, want.h)
			continue
		}
		if got := color.RGBAModel.Convert(out.At(want.x, want.y)).(color.RGBA); got != red {
			t.Errorf("orientation %d: red pixel not at (%d,%d)", o, want.x, want.y)
		}
	}
}

func TestJPEGOrientationParserIsRobust(t *testing.T) {
	for _, junk := range [][]byte{nil, {0xFF}, {0xFF, 0xD8}, {0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, []byte("not a jpeg at all")} {
		if got := jpegOrientation(junk); got != 1 {
			t.Errorf("jpegOrientation(%v) = %d, want 1", junk, got)
		}
	}
}

func TestURL(t *testing.T) {
	if got := URL("http://api.test", "img_abc", SizeMedium); got != "http://api.test/v1/media/img_abc/medium.jpg" {
		t.Fatalf("URL = %s", got)
	}
}
