package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/rsahara/timich-agent/internal/catalog"
)

var migrateGalleryCatalog = catalog.MigratePreReleaseCatalogV4ToV5

func preReleaseMigrateCatalogV4ToV5(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("timich-agent pre-release-migrate-catalog-v4-v5", flag.ContinueOnError)
	flags.SetOutput(stderr)
	source := flags.String("source", "", "existing V4 catalog.db; never overwritten")
	output := flags.String("output", "", "new standalone V5 database path; must not exist")
	stopped := flags.Bool("confirm-agent-stopped", false, "confirm all Agent writers are stopped for cutover")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q", flags.Arg(0))
	}
	if strings.TrimSpace(*source) == "" || strings.TrimSpace(*output) == "" {
		return errors.New("source and output are required")
	}
	if !*stopped {
		return errors.New("confirm-agent-stopped is required for the offline migration")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := migrateGalleryCatalog(ctx, *source, *output)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		catalog.CatalogGalleryMigrationResult
		AgentVersion string `json:"agentVersion"`
		AgentCommit  string `json:"agentCommit"`
	}{result, version, commit})
}
