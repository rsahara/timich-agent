package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const immichMirrorMaxPageSize = 1000

// Every request reads page one of a smaller capture-time range. Removing an
// already-read row from the updated-time window cannot shift an offset past an
// unread row. Inclusive boundaries also retain equal capture timestamps.
type immichMirrorPager struct {
	service    *Service
	datasource *config.DatasourceConfig
	options    immichMirrorFetchOptions
	before     *time.Time
	size       int
	seen       map[string]time.Time
	done       bool
	pendingErr error
}

func (p *immichMirrorPager) next(ctx context.Context) ([]immichAsset, error) {
	if p.pendingErr != nil {
		return nil, p.pendingErr
	}
	if p.size == 0 {
		p.size = maxPageSize
	}
	visibility := p.options.Visibility
	if visibility == "" {
		visibility = "timeline"
	}
	body := map[string]any{"page": 1, "size": p.size, "order": SortDirectionDesc, "visibility": visibility}
	// Trash keeps its visibility but disappears from default metadata searches.
	// Include tombstones only in delta windows so full-sync limits still count
	// live timeline assets and retained exclusions can be applied atomically.
	if p.options.UpdatedAfter != nil {
		body["withDeleted"] = true
	}
	for name, value := range map[string]*time.Time{
		"updatedAfter": p.options.UpdatedAfter, "updatedBefore": p.options.UpdatedBefore, "takenBefore": p.before,
	} {
		if value != nil && !value.IsZero() {
			body[name] = value.UTC().Format(time.RFC3339Nano)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal immich mirror sync request: %w", err)
	}
	request, err := p.service.newRequestForDatasource(p.datasource, http.MethodPost, "/api/search/metadata", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	request = request.WithContext(ctx)
	request.Header.Set("Content-Type", "application/json")
	response, err := p.service.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("perform immich mirror sync request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("immich mirror sync returned status %d", response.StatusCode)
	}
	var envelope searchAssetsEnvelope
	if err := json.NewDecoder(response.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode immich mirror sync response: %w", err)
	}
	page := envelope.Assets
	if page.NextPage == nil {
		p.done = true
	} else {
		if *page.NextPage != 2 || len(page.Items) == 0 {
			return nil, fmt.Errorf("immich mirror sync returned invalid continuation for page one")
		}
		var oldest time.Time
		for _, asset := range page.Items {
			if asset.FileCreatedAt.IsZero() {
				return nil, fmt.Errorf("immich mirror sync cannot page an asset without a capture timestamp")
			}
			if oldest.IsZero() || asset.FileCreatedAt.Time.Before(oldest) {
				oldest = asset.FileCreatedAt.Time.UTC()
			}
		}
		// Immich's date filters are JavaScript Dates (millisecond precision).
		// Overlap the entire last millisecond, including sub-ms database values.
		boundary := oldest.Truncate(time.Millisecond).Add(time.Millisecond)
		if p.before != nil && !boundary.Before(*p.before) {
			if p.size < immichMirrorMaxPageSize {
				p.size = immichMirrorMaxPageSize
			} else {
				// Never silently fall back to an offset or advance the durable
				// cursor past an overfull, indivisible timestamp group. Defer the
				// error so an explicitly limited full sync can finish its prefix.
				p.pendingErr = fmt.Errorf("immich mirror sync has more than %d assets at one capture-time boundary; sync is incomplete and its checkpoint was not advanced", immichMirrorMaxPageSize)
			}
		} else {
			p.before = &boundary
		}
	}
	// Only adjacent pages overlap. Bound deduplication memory by the API page
	// size instead of retaining another index of the entire photo library.
	seen := make(map[string]time.Time, len(page.Items))
	items := page.Items[:0]
	for _, asset := range page.Items {
		var updated time.Time
		if asset.UpdatedAt != nil {
			updated = asset.UpdatedAt.Time
		}
		seen[asset.ID] = updated
		if previous, ok := p.seen[asset.ID]; ok && !updated.After(previous) {
			continue
		}
		items = append(items, asset)
	}
	p.seen = seen
	return items, nil
}
