package catalog

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const (
	upstreamChecksumAlgorithmSHA1     = "sha1"
	upstreamChecksumAlgorithmSHA1Path = "sha1-path"
	upstreamChecksumAlgorithmUnknown  = "unknown"
)

var errExternalContentIdentityScopeChanged = errors.New("external content identity datasource scope changed")

type immichExternalLibraryMapping struct {
	ImmichSourceKey    string
	LocalSourceKey     string
	LocalRootKey       string
	OriginalPathPrefix string
}

func configuredImmichExternalLibraryMappings(datasources []config.DatasourceConfig) []immichExternalLibraryMapping {
	mappings := make([]immichExternalLibraryMapping, 0)
	for _, datasource := range datasources {
		if datasource.Kind != config.DatasourceKindLocalFiles || datasource.Scan == nil {
			continue
		}
		localSourceKey := strings.TrimSpace(datasource.SourceKey)
		localRootKey := strings.TrimSpace(datasource.RootKey)
		for _, mapping := range datasource.Scan.ImmichExternalLibraryMappings {
			immichSourceKey := strings.TrimSpace(mapping.SourceKey)
			prefix := normalizeImmichExternalPath(mapping.OriginalPathPrefix)
			if localSourceKey == "" || immichSourceKey == "" || prefix == "" {
				continue
			}
			mappings = append(mappings, immichExternalLibraryMapping{
				ImmichSourceKey:    immichSourceKey,
				LocalSourceKey:     localSourceKey,
				LocalRootKey:       localRootKey,
				OriginalPathPrefix: prefix,
			})
		}
	}
	sort.Slice(mappings, func(i, j int) bool {
		if mappings[i].ImmichSourceKey != mappings[j].ImmichSourceKey {
			return mappings[i].ImmichSourceKey < mappings[j].ImmichSourceKey
		}
		if mappings[i].OriginalPathPrefix != mappings[j].OriginalPathPrefix {
			return mappings[i].OriginalPathPrefix < mappings[j].OriginalPathPrefix
		}
		return mappings[i].LocalSourceKey < mappings[j].LocalSourceKey
	})
	return mappings
}

func datasourceConfigsFromState(state *serviceDatasourceState) []config.DatasourceConfig {
	if state == nil || len(state.datasources) == 0 {
		return nil
	}
	datasources := make([]config.DatasourceConfig, 0, len(state.datasources))
	for sourceKey, datasource := range state.datasources {
		datasource.SourceKey = sourceKey
		datasources = append(datasources, datasource)
	}
	sort.Slice(datasources, func(i, j int) bool {
		return datasources[i].SourceKey < datasources[j].SourceKey
	})
	return datasources
}

func normalizeImmichExternalPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return path.Clean(value)
}

func immichExternalRelativePath(originalPath string, prefix string) (string, bool) {
	originalPath = normalizeImmichExternalPath(originalPath)
	prefix = normalizeImmichExternalPath(prefix)
	if originalPath == "" || prefix == "" || originalPath == prefix || !strings.HasPrefix(originalPath, prefix+"/") {
		return "", false
	}
	relativePath := strings.TrimPrefix(originalPath, prefix+"/")
	if relativePath == "" || relativePath == "." || relativePath == ".." || strings.HasPrefix(relativePath, "../") || path.IsAbs(relativePath) {
		return "", false
	}
	if cleaned := path.Clean(relativePath); cleaned != relativePath {
		return "", false
	}
	return relativePath, true
}

func mappedImmichExternalPath(mappings []immichExternalLibraryMapping, immichSourceKey string, originalPath string) (immichExternalLibraryMapping, string, bool) {
	immichSourceKey = strings.TrimSpace(immichSourceKey)
	for _, mapping := range mappings {
		if mapping.ImmichSourceKey != immichSourceKey {
			continue
		}
		relativePath, ok := immichExternalRelativePath(originalPath, mapping.OriginalPathPrefix)
		if ok {
			return mapping, relativePath, true
		}
	}
	return immichExternalLibraryMapping{}, "", false
}

func immichExternalOriginalPath(prefix string, relativePath string) (string, bool) {
	prefix = normalizeImmichExternalPath(prefix)
	relativePath = strings.TrimSpace(relativePath)
	if prefix == "" || relativePath == "" || relativePath == "." || relativePath == ".." || strings.HasPrefix(relativePath, "../") || path.IsAbs(relativePath) {
		return "", false
	}
	if cleaned := path.Clean(relativePath); cleaned != relativePath {
		return "", false
	}
	return prefix + "/" + relativePath, true
}

func immichExternalPathChecksumHex(originalPath string) string {
	digest := sha1.Sum([]byte("path:" + strings.TrimSpace(originalPath)))
	return hex.EncodeToString(digest[:])
}

func normalizeMirrorUpstreamChecksumAlgorithm(asset ImmichMirrorAsset) string {
	algorithm := strings.TrimSpace(asset.UpstreamChecksumAlgorithm)
	switch algorithm {
	case upstreamChecksumAlgorithmSHA1, upstreamChecksumAlgorithmSHA1Path, upstreamChecksumAlgorithmUnknown:
		return algorithm
	case "":
		if normalizeCatalogSHA1Hex(asset.ContentSHA1Hex) != "" {
			return upstreamChecksumAlgorithmSHA1
		}
	}
	return upstreamChecksumAlgorithmUnknown
}

func (s *CatalogStore) resolveImmichMirrorAssetCanonicalIdentityInTx(ctx context.Context, tx *sql.Tx, asset *ImmichMirrorAsset) error {
	if asset == nil || normalizeMirrorUpstreamChecksumAlgorithm(*asset) != upstreamChecksumAlgorithmSHA1Path ||
		strings.TrimSpace(asset.MappedLocalSourceKey) == "" || strings.TrimSpace(asset.MappedLocalRootKey) == "" ||
		strings.TrimSpace(asset.MappedLocalRelativePath) == "" {
		return nil
	}
	var sha1Hex string
	var sizeBytes int64
	err := tx.QueryRowContext(ctx, `SELECT local.sha1_hex, local.content_size_bytes
		FROM local_asset_locations location
		JOIN local_assets local
		  ON local.source_key = location.source_key
		 AND local.asset_id = location.asset_id
		WHERE location.source_key = ?
			AND location.root_key = ?
			AND location.relative_path = ?
			AND location.status = 'active'
			AND local.visibility_status = 'active'
		LIMIT 1`, asset.MappedLocalSourceKey, asset.MappedLocalRootKey, asset.MappedLocalRelativePath).Scan(&sha1Hex, &sizeBytes)
	if err == sql.ErrNoRows {
		asset.CanonicalContentSHA1Hex = ""
		asset.CanonicalContentSizeBytes = 0
		return nil
	}
	if err != nil {
		return fmt.Errorf("resolve incremental external content identity: %w", err)
	}
	asset.CanonicalContentSHA1Hex = normalizeCatalogSHA1Hex(sha1Hex)
	asset.CanonicalContentSizeBytes = sizeBytes
	return nil
}

func immichExternalIdentityScopeKey(datasources []config.DatasourceConfig) string {
	hash := sha256.New()
	for _, mapping := range configuredImmichExternalLibraryMappings(datasources) {
		for _, value := range []string{mapping.ImmichSourceKey, mapping.LocalSourceKey, mapping.LocalRootKey, mapping.OriginalPathPrefix} {
			hash.Write([]byte(value))
			hash.Write([]byte{0})
		}
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func catalogExternalIdentityScopeMatchesInTx(ctx context.Context, tx *sql.Tx, expectedScopeKey string) (bool, error) {
	expectedScopeKey = strings.TrimSpace(expectedScopeKey)
	if expectedScopeKey == "" {
		return false, nil
	}
	var matches int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM catalog_external_identity_state
		WHERE singleton_id = 1 AND scope_key = ?
	)`, expectedScopeKey).Scan(&matches); err != nil {
		return false, fmt.Errorf("validate external content identity scope: %w", err)
	}
	return matches == 1, nil
}

func lockImmichMirrorExternalIdentityScopeInTx(ctx context.Context, tx *sql.Tx, assets []ImmichMirrorAsset) error {
	expectedScopeKey := ""
	for _, asset := range assets {
		scopeKey := strings.TrimSpace(asset.ExternalContentIdentityScopeKey)
		if scopeKey == "" {
			continue
		}
		if expectedScopeKey != "" && expectedScopeKey != scopeKey {
			return fmt.Errorf("%w: mirror batch contains multiple scopes", errExternalContentIdentityScopeChanged)
		}
		expectedScopeKey = scopeKey
	}
	if expectedScopeKey == "" {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE catalog_external_identity_state
		SET reconciled_at = reconciled_at
		WHERE singleton_id = 1 AND scope_key = ?`, expectedScopeKey)
	if err != nil {
		return fmt.Errorf("lock external content identity scope: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read external content identity scope lock: %w", err)
	}
	if affected != 1 {
		return errExternalContentIdentityScopeChanged
	}
	return nil
}

func (s *CatalogStore) reconcileConfiguredImmichExternalIdentities(ctx context.Context, datasources []config.DatasourceConfig) (int64, error) {
	if s == nil || s.db == nil {
		return 0, ErrCatalogNotConfigured
	}
	scopeKey := immichExternalIdentityScopeKey(datasources)
	var currentScope string
	err := s.db.QueryRowContext(ctx, `SELECT scope_key FROM catalog_external_identity_state WHERE singleton_id = 1`).Scan(&currentScope)
	if err == nil && currentScope == scopeKey {
		return 0, nil
	}
	if err != nil && err != sql.ErrNoRows {
		return 0, fmt.Errorf("read external identity scope: %w", err)
	}

	nowText := formatCatalogTime(time.Now().UTC())
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin external identity reconciliation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.immich_external_identity_desired`); err != nil {
		return 0, fmt.Errorf("reset external identity desired state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE immich_external_identity_desired (
		immich_source_key TEXT NOT NULL,
		upstream_asset_id TEXT NOT NULL,
		content_sha1_hex TEXT NOT NULL,
		content_size_bytes INTEGER NOT NULL,
		PRIMARY KEY(immich_source_key, upstream_asset_id)
	)`); err != nil {
		return 0, fmt.Errorf("create external identity desired state: %w", err)
	}

	insertDesired, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO immich_external_identity_desired (
			immich_source_key, upstream_asset_id, content_sha1_hex, content_size_bytes
		)
		SELECT source_key, upstream_asset_id, ?, ?
		FROM catalog_assets
		WHERE source_key = ?
			AND datasource_kind = 'immich'
			AND upstream_checksum_algorithm = ?
			AND content_sha1_hex = ?
			AND media_type = ?`)
	if err != nil {
		return 0, fmt.Errorf("prepare external identity desired row: %w", err)
	}
	defer insertDesired.Close()

	for _, mapping := range configuredImmichExternalLibraryMappings(datasources) {
		rows, err := tx.QueryContext(ctx, `SELECT location.relative_path, asset.sha1_hex,
				asset.content_size_bytes, asset.media_type
			FROM local_asset_locations location
			JOIN local_assets asset
			  ON asset.source_key = location.source_key
			 AND asset.asset_id = location.asset_id
			WHERE location.source_key = ?
				AND location.root_key = ?
				AND location.status = 'active'
				AND asset.visibility_status = 'active'
			ORDER BY location.id`, mapping.LocalSourceKey, mapping.LocalRootKey)
		if err != nil {
			return 0, fmt.Errorf("query Local identities for external mapping: %w", err)
		}
		for rows.Next() {
			var relativePath string
			var sha1Hex string
			var sizeBytes int64
			var mediaType string
			if err := rows.Scan(&relativePath, &sha1Hex, &sizeBytes, &mediaType); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("scan Local identity for external mapping: %w", err)
			}
			originalPath, ok := immichExternalOriginalPath(mapping.OriginalPathPrefix, relativePath)
			if !ok {
				continue
			}
			if _, err := insertDesired.ExecContext(ctx,
				normalizeCatalogSHA1Hex(sha1Hex),
				sizeBytes,
				mapping.ImmichSourceKey,
				upstreamChecksumAlgorithmSHA1Path,
				immichExternalPathChecksumHex(originalPath),
				mediaType,
			); err != nil {
				_ = rows.Close()
				return 0, fmt.Errorf("record external identity desired row: %w", err)
			}
		}
		if err := rows.Close(); err != nil {
			return 0, fmt.Errorf("close Local identities for external mapping: %w", err)
		}
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("iterate Local identities for external mapping: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS temp.immich_external_identity_changed`); err != nil {
		return 0, fmt.Errorf("reset changed external identity state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE immich_external_identity_changed (
		source_key TEXT NOT NULL,
		upstream_asset_id TEXT NOT NULL,
		old_canonical_asset_id TEXT NOT NULL,
		PRIMARY KEY(source_key, upstream_asset_id)
	)`); err != nil {
		return 0, fmt.Errorf("create changed external identity state: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO immich_external_identity_changed (
			source_key, upstream_asset_id, old_canonical_asset_id
		)
		SELECT source_key, upstream_asset_id, COALESCE(canonical_asset_id, '')
		FROM catalog_assets
		WHERE upstream_checksum_algorithm = ?
			AND (
				((canonical_content_sha1_hex IS NOT NULL OR canonical_content_size_bytes IS NOT NULL)
					AND NOT EXISTS (
						SELECT 1 FROM immich_external_identity_desired desired
						WHERE desired.immich_source_key = catalog_assets.source_key
							AND desired.upstream_asset_id = catalog_assets.upstream_asset_id
					))
				OR EXISTS (
					SELECT 1 FROM immich_external_identity_desired desired
					WHERE desired.immich_source_key = catalog_assets.source_key
						AND desired.upstream_asset_id = catalog_assets.upstream_asset_id
						AND (catalog_assets.canonical_content_sha1_hex IS NOT desired.content_sha1_hex
							OR catalog_assets.canonical_content_size_bytes IS NOT desired.content_size_bytes)
				)
			)`, upstreamChecksumAlgorithmSHA1Path); err != nil {
		return 0, fmt.Errorf("record changed external identities: %w", err)
	}
	var changed int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM immich_external_identity_changed`).Scan(&changed); err != nil {
		return 0, fmt.Errorf("count changed external identities: %w", err)
	}

	_, err = tx.ExecContext(ctx, `UPDATE catalog_assets
		SET canonical_content_sha1_hex = NULL,
			canonical_content_size_bytes = NULL,
			updated_at = ?
		WHERE upstream_checksum_algorithm = ?
			AND (canonical_content_sha1_hex IS NOT NULL OR canonical_content_size_bytes IS NOT NULL)
			AND NOT EXISTS (
				SELECT 1 FROM immich_external_identity_desired desired
				WHERE desired.immich_source_key = catalog_assets.source_key
					AND desired.upstream_asset_id = catalog_assets.upstream_asset_id
			)`, nowText, upstreamChecksumAlgorithmSHA1Path)
	if err != nil {
		return 0, fmt.Errorf("clear stale external identities: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE catalog_assets
		SET canonical_content_sha1_hex = (
				SELECT desired.content_sha1_hex FROM immich_external_identity_desired desired
				WHERE desired.immich_source_key = catalog_assets.source_key
					AND desired.upstream_asset_id = catalog_assets.upstream_asset_id
			),
			canonical_content_size_bytes = (
				SELECT desired.content_size_bytes FROM immich_external_identity_desired desired
				WHERE desired.immich_source_key = catalog_assets.source_key
					AND desired.upstream_asset_id = catalog_assets.upstream_asset_id
			),
			updated_at = ?
		WHERE EXISTS (
				SELECT 1 FROM immich_external_identity_desired desired
				WHERE desired.immich_source_key = catalog_assets.source_key
					AND desired.upstream_asset_id = catalog_assets.upstream_asset_id
					AND (catalog_assets.canonical_content_sha1_hex IS NOT desired.content_sha1_hex
						OR catalog_assets.canonical_content_size_bytes IS NOT desired.content_size_bytes)
			)`, nowText)
	if err != nil {
		return 0, fmt.Errorf("apply external identities: %w", err)
	}
	if changed > 0 {
		if err := s.rebuildChangedImmichExternalCanonicalAssetsInTx(ctx, tx, nowText); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO catalog_external_identity_state(singleton_id, scope_key, reconciled_at)
		VALUES (1, ?, ?)
		ON CONFLICT(singleton_id) DO UPDATE SET
			scope_key = excluded.scope_key,
			reconciled_at = excluded.reconciled_at`, scopeKey, nowText); err != nil {
		return 0, fmt.Errorf("store external identity scope: %w", err)
	}
	if changed > 0 {
		if err := s.commitCatalogAssetChanges(ctx, tx, true); err != nil {
			return 0, fmt.Errorf("commit external identity reconciliation: %w", err)
		}
	} else if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit external identity scope: %w", err)
	}
	return changed, nil
}

func (s *CatalogStore) rebuildChangedImmichExternalCanonicalAssetsInTx(ctx context.Context, tx *sql.Tx, nowText string) error {
	ids := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx, `SELECT old_canonical_asset_id
		FROM immich_external_identity_changed
		WHERE old_canonical_asset_id <> ''`)
	if err != nil {
		return fmt.Errorf("query old external canonical identities: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan old external canonical identity: %w", err)
		}
		ids[id] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close old external canonical identities: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate old external canonical identities: %w", err)
	}

	rows, err = tx.QueryContext(ctx, `SELECT asset.source_key, asset.datasource_kind, asset.upstream_asset_id,
			COALESCE(asset.canonical_asset_id, ''), asset.upstream_checksum_algorithm,
			asset.content_sha1_hex, asset.content_size_bytes,
			asset.canonical_content_sha1_hex, asset.canonical_content_size_bytes,
			asset.media_type, asset.filename, asset.captured_at, asset.duration,
			asset.visibility_status, asset.is_favorite, asset.place_label, asset.description,
			asset.first_seen_at, asset.updated_at
		FROM catalog_assets asset
		JOIN immich_external_identity_changed changed
		  ON changed.source_key = asset.source_key
		 AND changed.upstream_asset_id = asset.upstream_asset_id
		ORDER BY asset.source_key, asset.upstream_asset_id`)
	if err != nil {
		return fmt.Errorf("query changed external canonical sources: %w", err)
	}
	sources := make([]catalogCanonicalSourceRow, 0)
	for rows.Next() {
		row, err := scanCatalogCanonicalSourceRow(rows)
		if err != nil {
			_ = rows.Close()
			return err
		}
		sources = append(sources, row)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close changed external canonical sources: %w", err)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate changed external canonical sources: %w", err)
	}
	for _, row := range sources {
		id := catalogCanonicalAssetID(row)
		ids[id] = struct{}{}
		if row.CanonicalAssetID != id {
			if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
				SET canonical_asset_id = ?, updated_at = ?
				WHERE source_key = ? AND upstream_asset_id = ?`,
				id, nowText, row.SourceKey, row.UpstreamAssetID); err != nil {
				return fmt.Errorf("update mapped external canonical link: %w", err)
			}
		}
	}
	return s.rebuildCatalogCanonicalIDsInTx(ctx, tx, ids, nowText)
}

// reconcileImmichExternalIdentitiesForLocalIdentityLossInTx updates only the
// Immich path projections that could have depended on Local locations that
// just lost an active, verified content identity. The caller's frozen mapping
// scope is validated once inside the same writer transaction so a datasource
// reconfiguration cannot publish a stale identity after the transition commits.
func (s *CatalogStore) reconcileImmichExternalIdentitiesForLocalIdentityLossInTx(
	ctx context.Context,
	tx *sql.Tx,
	mappings []immichExternalLibraryMapping,
	expectedScopeKey string,
	localSourceKey string,
	localRootKey string,
	relativePaths []string,
	nowText string,
) (int, error) {
	scopeMatches, err := catalogExternalIdentityScopeMatchesInTx(ctx, tx, expectedScopeKey)
	if err != nil {
		return 0, err
	}
	if !scopeMatches {
		return 0, errExternalContentIdentityScopeChanged
	}

	type affectedImmichAsset struct {
		sourceKey            string
		upstreamAssetID      string
		originalPath         string
		mediaType            string
		canonicalContentSHA1 sql.NullString
		canonicalContentSize sql.NullInt64
	}
	affected := make(map[string]affectedImmichAsset)
	uniqueRelativePaths := make(map[string]struct{}, len(relativePaths))
	for _, relativePath := range relativePaths {
		relativePath = strings.TrimSpace(relativePath)
		if relativePath != "" {
			uniqueRelativePaths[relativePath] = struct{}{}
		}
	}
	orderedRelativePaths := make([]string, 0, len(uniqueRelativePaths))
	for relativePath := range uniqueRelativePaths {
		orderedRelativePaths = append(orderedRelativePaths, relativePath)
	}
	sort.Strings(orderedRelativePaths)
	for _, relativePath := range orderedRelativePaths {
		for _, mapping := range mappings {
			if mapping.LocalSourceKey != strings.TrimSpace(localSourceKey) || mapping.LocalRootKey != strings.TrimSpace(localRootKey) {
				continue
			}
			originalPath, ok := immichExternalOriginalPath(mapping.OriginalPathPrefix, relativePath)
			if !ok {
				continue
			}
			rows, err := tx.QueryContext(ctx, `SELECT upstream_asset_id, media_type,
				canonical_content_sha1_hex, canonical_content_size_bytes
			FROM catalog_assets
			WHERE source_key = ?
				AND datasource_kind = 'immich'
				AND upstream_checksum_algorithm = ?
				AND content_sha1_hex = ?
			ORDER BY upstream_asset_id`,
				mapping.ImmichSourceKey,
				upstreamChecksumAlgorithmSHA1Path,
				immichExternalPathChecksumHex(originalPath),
			)
			if err != nil {
				return 0, fmt.Errorf("query Immich identities affected by Local identity loss: %w", err)
			}
			for rows.Next() {
				target := affectedImmichAsset{
					sourceKey:    mapping.ImmichSourceKey,
					originalPath: originalPath,
				}
				if err := rows.Scan(
					&target.upstreamAssetID,
					&target.mediaType,
					&target.canonicalContentSHA1,
					&target.canonicalContentSize,
				); err != nil {
					_ = rows.Close()
					return 0, fmt.Errorf("scan Immich identity affected by Local identity loss: %w", err)
				}
				affected[target.sourceKey+"\x00"+target.upstreamAssetID] = target
			}
			if err := rows.Close(); err != nil {
				return 0, fmt.Errorf("close Immich identities affected by Local identity loss: %w", err)
			}
			if err := rows.Err(); err != nil {
				return 0, fmt.Errorf("iterate Immich identities affected by Local identity loss: %w", err)
			}
		}
	}

	keys := make([]string, 0, len(affected))
	for key := range affected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	changed := 0
	for _, key := range keys {
		target := affected[key]
		var desiredSHA1 sql.NullString
		var desiredSize sql.NullInt64
		for _, mapping := range mappings {
			if mapping.ImmichSourceKey != target.sourceKey {
				continue
			}
			mappedRelativePath, ok := immichExternalRelativePath(target.originalPath, mapping.OriginalPathPrefix)
			if !ok {
				continue
			}
			var sha1Hex string
			var sizeBytes int64
			err := tx.QueryRowContext(ctx, `SELECT asset.sha1_hex, asset.content_size_bytes
				FROM local_asset_locations location
				JOIN local_assets asset
				  ON asset.source_key = location.source_key
				 AND asset.asset_id = location.asset_id
				WHERE location.source_key = ?
					AND location.root_key = ?
					AND location.relative_path = ?
					AND location.status = 'active'
					AND asset.visibility_status = 'active'
					AND asset.media_type = ?
				ORDER BY location.id DESC
				LIMIT 1`,
				mapping.LocalSourceKey,
				mapping.LocalRootKey,
				mappedRelativePath,
				target.mediaType,
			).Scan(&sha1Hex, &sizeBytes)
			if err == sql.ErrNoRows {
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("resolve replacement Local identity after identity loss: %w", err)
			}
			desiredSHA1 = sql.NullString{String: normalizeCatalogSHA1Hex(sha1Hex), Valid: true}
			desiredSize = sql.NullInt64{Int64: sizeBytes, Valid: true}
		}

		currentSHA1 := target.canonicalContentSHA1
		if currentSHA1.Valid {
			currentSHA1.String = normalizeCatalogSHA1Hex(currentSHA1.String)
		}
		if currentSHA1 == desiredSHA1 && target.canonicalContentSize == desiredSize {
			continue
		}
		var desiredSHA1Value any
		var desiredSizeValue any
		if desiredSHA1.Valid {
			desiredSHA1Value = desiredSHA1.String
			desiredSizeValue = desiredSize.Int64
		}
		if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
			SET canonical_content_sha1_hex = ?,
				canonical_content_size_bytes = ?,
				updated_at = ?
			WHERE source_key = ? AND upstream_asset_id = ?`,
			desiredSHA1Value,
			desiredSizeValue,
			nowText,
			target.sourceKey,
			target.upstreamAssetID,
		); err != nil {
			return 0, fmt.Errorf("apply replacement Immich identity after Local identity loss: %w", err)
		}
		if err := s.refreshCatalogCanonicalAssetInTx(ctx, tx, target.sourceKey, target.upstreamAssetID, nowText); err != nil {
			return 0, err
		}
		changed++
	}
	return changed, nil
}

func (s *CatalogStore) reconcileImmichExternalIdentityForLocalInTx(
	ctx context.Context,
	tx *sql.Tx,
	datasource config.DatasourceConfig,
	expectedScopeKey string,
	relativePath string,
	contentSHA1Hex string,
	contentSizeBytes int64,
	mediaType string,
	nowText string,
) error {
	scopeMatches, err := catalogExternalIdentityScopeMatchesInTx(ctx, tx, expectedScopeKey)
	if err != nil {
		return err
	}
	if !scopeMatches {
		return errExternalContentIdentityScopeChanged
	}
	if datasource.Scan == nil || len(datasource.Scan.ImmichExternalLibraryMappings) == 0 {
		return nil
	}
	for _, configured := range datasource.Scan.ImmichExternalLibraryMappings {
		originalPath, ok := immichExternalOriginalPath(configured.OriginalPathPrefix, relativePath)
		if !ok {
			continue
		}
		rows, err := tx.QueryContext(ctx, `SELECT upstream_asset_id
			FROM catalog_assets
			WHERE source_key = ?
				AND datasource_kind = 'immich'
				AND upstream_checksum_algorithm = ?
				AND content_sha1_hex = ?
				AND media_type = ?
				AND (canonical_content_sha1_hex IS NOT ? OR canonical_content_size_bytes IS NOT ?)
			ORDER BY upstream_asset_id`,
			strings.TrimSpace(configured.SourceKey),
			upstreamChecksumAlgorithmSHA1Path,
			immichExternalPathChecksumHex(originalPath),
			mediaType,
			normalizeCatalogSHA1Hex(contentSHA1Hex),
			contentSizeBytes,
		)
		if err != nil {
			return fmt.Errorf("query mapped Immich external identity: %w", err)
		}
		upstreamAssetIDs := make([]string, 0, 1)
		for rows.Next() {
			var upstreamAssetID string
			if err := rows.Scan(&upstreamAssetID); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan mapped Immich external identity: %w", err)
			}
			upstreamAssetIDs = append(upstreamAssetIDs, upstreamAssetID)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("close mapped Immich external identities: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iterate mapped Immich external identities: %w", err)
		}
		for _, upstreamAssetID := range upstreamAssetIDs {
			if _, err := tx.ExecContext(ctx, `UPDATE catalog_assets
				SET canonical_content_sha1_hex = ?,
					canonical_content_size_bytes = ?,
					updated_at = ?
				WHERE source_key = ? AND upstream_asset_id = ?`,
				normalizeCatalogSHA1Hex(contentSHA1Hex),
				contentSizeBytes,
				nowText,
				strings.TrimSpace(configured.SourceKey),
				upstreamAssetID,
			); err != nil {
				return fmt.Errorf("apply mapped Immich external identity: %w", err)
			}
			if err := s.refreshCatalogCanonicalAssetInTx(ctx, tx, configured.SourceKey, upstreamAssetID, nowText); err != nil {
				return err
			}
		}
	}
	return nil
}
