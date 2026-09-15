package catalog

import (
	"context"
	"encoding/hex"
	"image"
	"image/png"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/image/webp"
)

// This optional integration test uses libwebp's fixture tools and the real
// helper, exercising the same pinned descriptor and durable jobs as the Agent.
func TestLocalCaptureMetadataLargeWebPWithRealHelper(t *testing.T) {
	helper := os.Getenv("TIMICH_TEST_MEDIA_HELPER")
	if helper == "" {
		t.Skip("set TIMICH_TEST_MEDIA_HELPER to the built media helper")
	}
	helper, err := filepath.Abs(helper)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"cwebp", "webpmux"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("fixture tool %s unavailable", tool)
		}
	}
	fixtures := t.TempDir()
	pixels := image.NewNRGBA(image.Rect(0, 0, 2500, 2500))
	if _, err := rand.New(rand.NewSource(42)).Read(pixels.Pix); err != nil {
		t.Fatal(err)
	}
	for i := 3; i < len(pixels.Pix); i += 4 {
		pixels.Pix[i] = 255
	}
	pngPath := filepath.Join(fixtures, "source.png")
	file, err := os.Create(pngPath)
	if err != nil {
		t.Fatal(err)
	}
	encoder := png.Encoder{CompressionLevel: png.NoCompression}
	encodeErr := encoder.Encode(file, pixels)
	closeErr := file.Close()
	if encodeErr != nil {
		t.Fatal(encodeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	runTool := func(name string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if output, err := exec.CommandContext(ctx, name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", name, err, output)
		}
	}
	plainPath := filepath.Join(fixtures, "plain.webp")
	runTool("cwebp", "-quiet", "-lossless", "-m", "0", "-q", "0", pngPath, "-o", plainPath)
	// Little-endian TIFF: IFD0 points to an EXIF IFD containing
	// DateTimeOriginal (20 ASCII bytes) and OffsetTimeOriginal (7 ASCII bytes).
	exif, err := hex.DecodeString("49492a0008000000010069870400010000001a00000000000000020003900200140000003800000011900200070000004c00000000000000")
	if err != nil {
		t.Fatal(err)
	}
	exif = append(exif, []byte("2026:09:06 12:34:56\x00+09:00\x00")...)
	exifPath := filepath.Join(fixtures, "date.exif")
	if err := os.WriteFile(exifPath, exif, 0o600); err != nil {
		t.Fatal(err)
	}
	datedPath := filepath.Join(fixtures, "dated.webp")
	runTool("webpmux", "-set", "exif", exifPath, plainPath, "-o", datedPath)
	for _, test := range []struct{ name, path, source string }{
		{"without EXIF", plainPath, "file_mtime"},
		{"with EXIF", datedPath, "exif_original"},
	} {
		t.Run(test.name, func(t *testing.T) {
			input, err := os.Open(test.path)
			if err != nil {
				t.Fatal(err)
			}
			decoded, decodeErr := webp.Decode(input)
			closeErr := input.Close()
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			if closeErr != nil {
				t.Fatal(closeErr)
			}
			if decoded.Bounds() != pixels.Bounds() {
				t.Fatalf("invalid dimensions: %v", decoded.Bounds())
			}
			bytes, err := os.ReadFile(test.path)
			if err != nil {
				t.Fatal(err)
			}
			if len(bytes) <= 16*1024*1024 {
				t.Fatalf("WebP fixture too small: %d", len(bytes))
			}
			root := t.TempDir()
			path := filepath.Join(root, "large.webp")
			if err := os.WriteFile(path, bytes, 0o600); err != nil {
				t.Fatal(err)
			}
			mtime := time.Date(2026, 9, 13, 13, 19, 29, 0, time.UTC)
			if err := os.Chtimes(path, mtime, mtime); err != nil {
				t.Fatal(err)
			}
			s := newLocalVideoMetadataTestService(t, root, filepath.Join(root, "missing-helper"))
			ctx := context.Background()
			if _, err := s.RunLocalReconciliationScan(ctx, "1111111111111111"); err != nil {
				t.Fatal(err)
			}
			if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.RegisteredAssets != 1 {
				t.Fatalf("register: %+v, %v", result, err)
			}
			assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 0)
			s.mediaHelperPath, s.mediaHelperCheck = helper, localMediaHelperCapabilityStatus{}
			if result, err := s.RepairLocalMetadata(ctx); err != nil || result.CaptureDateQueued != 1 {
				t.Fatalf("repair: %+v, %v", result, err)
			}
			if result, err := s.RunLocalMetadataBatch(ctx, 10); err != nil || result.CompletedJobs != 1 || result.FailedJobs != 0 {
				t.Fatalf("refresh: %+v, %v", result, err)
			}
			assertLocalScanCount(t, s, `SELECT COUNT(*) FROM local_capture_metadata_state`, 1)
			var source, date string
			if err := s.catalog.db.QueryRow(`SELECT captured_at_source,captured_at FROM local_assets`).Scan(&source, &date); err != nil {
				t.Fatal(err)
			}
			want := formatCatalogTime(mtime)
			if test.source == "exif_original" {
				want = "2026-09-06T03:34:56.000000000Z"
			}
			if source != test.source || date != want {
				t.Fatalf("date=%s source=%s, want %s %s", date, source, want, test.source)
			}
			if result, err := s.RepairLocalMetadata(ctx); err != nil || result.Queued != 0 {
				t.Fatalf("repeat: %+v, %v", result, err)
			}
			t.Logf("repaired a decoded %d-byte WebP, source=%s; completed check is not requeued", len(bytes), source)
		})
	}
}
