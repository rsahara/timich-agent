package catalog

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rsahara/timich-agent/internal/config"
)

const immichMirrorClock = "upstream-http-date-v1"

// Sample before fetching any pages. Agent wall time is only diagnostic: it
// cannot be compared with timestamps assigned by another host. HTTP Date is
// second-granular; the lower edge of that second deliberately overlaps reads.
// The configured endpoint/proxy must preserve the Immich server's Date header.
func (s *Service) immichMirrorWindowEnd(ctx context.Context, datasource *config.DatasourceConfig) (time.Time, error) {
	request, err := s.newRequestForDatasource(datasource, http.MethodGet, "/api/server/ping", nil)
	if err != nil {
		return time.Time{}, err
	}
	request = request.WithContext(ctx)
	request.Header.Set("Cache-Control", "no-cache, no-store")
	response, err := s.client.Do(request)
	if err != nil {
		return time.Time{}, fmt.Errorf("sample Immich sync clock: %w", err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode != http.StatusOK {
		return time.Time{}, fmt.Errorf("sample Immich sync clock: status %d", response.StatusCode)
	}
	if age := strings.TrimSpace(response.Header.Get("Age")); age != "" && age != "0" {
		return time.Time{}, fmt.Errorf("sample Immich sync clock: cached response; configure the proxy to bypass caching for /api/server/ping")
	}
	value, err := http.ParseTime(response.Header.Get("Date"))
	if err != nil {
		return time.Time{}, fmt.Errorf("sample Immich sync clock: missing or invalid Date header; the proxy must preserve the upstream Date header")
	}
	return value.UTC(), nil
}
