package catalog

import (
	"context"
	"testing"
	"time"
)

func TestCanonicalDurationFallsBackToActiveDuplicateSource(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	nowText := formatCatalogTime(time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	const (
		localSource  = "1111111111111111"
		immichSource = "2222222222222222"
		sha1Hex      = "0123456789abcdef0123456789abcdef01234567"
		duration     = "0:00:12.345000"
	)
	for _, row := range []struct {
		sourceKey      string
		datasourceKind string
		assetID        string
		duration       any
	}{
		{sourceKey: localSource, datasourceKind: "local_filesystem", assetID: "local-video", duration: nil},
		{sourceKey: immichSource, datasourceKind: "immich", assetID: "immich-video", duration: duration},
	} {
		if _, err := store.db.ExecContext(ctx, `INSERT INTO catalog_assets (
				source_key, datasource_kind, upstream_asset_id, media_type, filename,
				captured_at, duration, visibility_status, upstream_checksum_algorithm,
				content_sha1_hex, content_size_bytes, canonical_content_sha1_hex,
				canonical_content_size_bytes, first_seen_at, updated_at
			) VALUES (?, ?, ?, 'video', 'clip.mov', ?, ?, 'active', 'sha1', ?, 1234, ?, 1234, ?, ?)`,
			row.sourceKey, row.datasourceKind, row.assetID, nowText, row.duration, sha1Hex, sha1Hex, nowText, nowText); err != nil {
			t.Fatalf("insert %s catalog source: %v", row.datasourceKind, err)
		}
	}
	if _, err := store.RebuildCatalogCanonicalAssets(ctx); err != nil {
		t.Fatalf("RebuildCatalogCanonicalAssets() error = %v", err)
	}

	var primarySource string
	var canonicalDuration string
	if err := store.queryDB().QueryRowContext(ctx, `SELECT primary_source_key, duration
		FROM catalog_canonical_assets`).Scan(&primarySource, &canonicalDuration); err != nil {
		t.Fatalf("read canonical video metadata: %v", err)
	}
	if primarySource != localSource {
		t.Fatalf("canonical primary source = %q, want Local source %q", primarySource, localSource)
	}
	if canonicalDuration != duration {
		t.Fatalf("canonical duration = %q, want duplicate fallback %q", canonicalDuration, duration)
	}
}
