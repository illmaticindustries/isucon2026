package jpegvalidate

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func compare(t *testing.T, data []byte) {
	t.Helper()
	want, wantErr := jpeg.Decode(bytes.NewReader(data))
	got, gotErr := Validate(bytes.NewReader(data))
	if (wantErr == nil) != (gotErr == nil) {
		t.Fatalf("different acceptance: standard=%v validator=%v", wantErr, gotErr)
	}
	if wantErr != nil {
		if wantErr.Error() != gotErr.Error() {
			t.Fatalf("different error: standard=%v validator=%v", wantErr, gotErr)
		}
	} else if got != want.Bounds() {
		t.Fatalf("bounds: got %v want %v", got, want.Bounds())
	}
}

func TestStandardCorpusAndTruncation(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(runtime.GOROOT(), "src/image/testdata/*.jpeg"))
	if err != nil || len(files) < 20 {
		t.Fatalf("Go JPEG corpus unavailable: files=%d err=%v", len(files), err)
	}
	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			// Go 1.27 added these four fixtures and support for their sampling
			// ratios. Preserve the Go 1.26.5 acceptance policy used by the app;
			// upgrading the toolchain must not expand accepted upload formats.
			switch filepath.Base(path) {
			case "video-001.q50.121121.jpeg", "video-001.q50.211211.jpeg",
				"video-001.q50.221122.jpeg", "video-001.q50.222112.jpeg":
				if _, err := Validate(bytes.NewReader(data)); err != errUnsupportedSubsamplingRatio {
					t.Fatalf("Go 1.26.5 rejection: got %v want %v", err, errUnsupportedSubsamplingRatio)
				}
				return
			}
			compare(t, data)
			for _, end := range []int{0, 1, 2, len(data) / 4, len(data) / 2, len(data) - 3, len(data) - 1} {
				compare(t, data[:end])
			}
		})
	}
}

func encodedImage() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 512, 512))
	r := rand.New(rand.NewSource(1))
	for y := 0; y < 512; y++ {
		for x := 0; x < 512; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(r.Intn(256)), uint8(r.Intn(256)), uint8(r.Intn(256)), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: 80}); err != nil {
		panic(err)
	}
	return b.Bytes()
}

func TestEntropyCorruption(t *testing.T) {
	data := encodedImage()
	r := rand.New(rand.NewSource(2))
	for i := 0; i < 100; i++ {
		mutated := bytes.Clone(data)
		// Change entropy data, preserving bounded dimensions in the header.
		at := 1024 + r.Intn(len(data)-1024)
		mutated[at] ^= uint8(1 + r.Intn(255))
		compare(t, mutated)
	}
}

func TestSequentialEveryTruncation(t *testing.T) {
	img := image.NewGray(image.Rect(0, 0, 17, 9))
	r := rand.New(rand.NewSource(41))
	_, _ = r.Read(img.Pix)
	for _, quality := range []int{1, 50, 100} {
		var encoded bytes.Buffer
		if err := jpeg.Encode(&encoded, img, &jpeg.Options{Quality: quality}); err != nil {
			t.Fatal(err)
		}
		data := encoded.Bytes()
		for end := 0; end <= len(data); end++ {
			compare(t, data[:end])
		}
	}
}

func FuzzEquivalentAcceptance(f *testing.F) {
	f.Add(encodedImage())
	f.Add([]byte("not a jpeg"))
	f.Fuzz(func(t *testing.T, data []byte) {
		// Bound both decoders' resource usage; do not fuzz giant image dimensions.
		config, err := jpeg.DecodeConfig(bytes.NewReader(data))
		if err == nil && (config.Width > 2048 || config.Height > 2048) {
			return
		}
		compare(t, data)
	})
}

func BenchmarkValidation(b *testing.B) {
	data := encodedImage()
	if path := os.Getenv("ISUCON_JPEG_BENCHMARK"); path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			b.Fatal(err)
		}
	}
	b.Run("standard", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("without-pixels", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := Validate(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
