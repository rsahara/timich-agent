package catalog

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestFormatCatalogTimeSortsChronologically(t *testing.T) {
	t.Parallel()

	earlier := time.Date(2026, 7, 18, 4, 2, 31, 184090000, time.UTC)
	later := earlier.Add(time.Microsecond)
	earlierText := formatCatalogTime(earlier)
	laterText := formatCatalogTime(later)

	if earlierText != "2026-07-18T04:02:31.184090000Z" {
		t.Fatalf("formatCatalogTime(earlier) = %q, want fixed-width UTC", earlierText)
	}
	if earlierText >= laterText {
		t.Fatalf("formatted order = %q >= %q, want chronological lexical order", earlierText, laterText)
	}
	if parsed, err := time.Parse(time.RFC3339Nano, earlierText); err != nil {
		t.Fatalf("parse fixed-width timestamp: %v", err)
	} else if !parsed.Equal(earlier) {
		t.Fatalf("parsed timestamp = %s, want %s", parsed, earlier)
	}
}

func TestReplaceFullRollsBackWhenChangeQueryFails(t *testing.T) {
	t.Parallel()

	store, err := LoadOrCreateCatalogStore(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreateCatalogStore() error = %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	if _, err := store.db.Exec(`DROP TABLE catalog_assets`); err != nil {
		t.Fatalf("drop catalog_assets: %v", err)
	}
	_, err = store.ReplaceFull(context.Background(), "1111111111111111", nil, 0, time.Now().UTC())
	if err == nil || !strings.Contains(err.Error(), "query immich full-sync changes") {
		t.Fatalf("ReplaceFull() error = %v, want change-query failure", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var suppressionCount int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM semantic_generation_suppression`).Scan(&suppressionCount); err != nil {
		t.Fatalf("query after failed ReplaceFull() error = %v, want released writer connection", err)
	}
	if suppressionCount != 0 {
		t.Fatalf("semantic generation suppressions = %d, want rolled back transaction", suppressionCount)
	}
}
