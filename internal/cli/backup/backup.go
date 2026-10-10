// Package backup implements "elythia backup", which takes database backups
// into storage outside the host and lists them (#3457).
//
// サブコマンドは internal/cli の Commands で "backup" の Sub に並べる。後の段階
// (verify / daemon / restore) も同じ親に足す。
package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
)

const defaultConfigPath = ".config/default.yml"

// env carries the process-level dependencies so tests can replace them.
type env struct {
	stdout, stderr io.Writer
	openStorage    func(config.BackupStorageOptions) (backup.Storage, error)
	// runner runs pg_dump. テストでは DB のコンテナの中で動かす Runner に差し替える。
	runner backup.Runner
	// dumpConn overrides how pg_dump connects. nil derives it from the config.
	dumpConn func(*config.Config) (backup.DumpConn, error)
}

func defaultEnv() env {
	return env{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		openStorage: backup.OpenStorage,
		runner:      backup.LocalRunner{},
	}
}

// Take implements "elythia backup take".
func Take(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return take(ctx, defaultEnv(), args)
}

// List implements "elythia backup list".
func List(args []string) int { return list(context.Background(), defaultEnv(), args) }

// setup opens the storage the backup: section of cfg selects.
func setup(e env, cfg *config.Config) (backup.Storage, error) {
	if cfg.Backup == nil {
		return nil, errors.New("the config file has no backup: section (see docs/configuration.md)")
	}
	return e.openStorage(cfg.Backup.Storage)
}

func take(ctx context.Context, e env, args []string) int {
	fs := cliflag.New("backup take", e.stderr)
	configPath := fs.String("config", defaultConfigPath, "path to configuration file")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	logger := slog.New(slog.NewTextHandler(e.stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	fail := func(msg string, err error) int {
		logger.Error("elythia backup take: "+msg, "error", err)
		return 1
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fail("failed to load config", err)
	}
	st, err := setup(e, cfg)
	if err != nil {
		return fail("failed to open storage", err)
	}
	opts := backup.TakeOptions{
		Storage:     st,
		DatabaseURL: cfg.DatabaseURL("postgres"),
		Runner:      e.runner,
		PgDump:      cfg.Backup.Tools.PgDump,
		Logger:      logger,
	}
	if e.dumpConn != nil {
		if opts.Dump, err = e.dumpConn(cfg); err != nil {
			return fail("failed to build the pg_dump connection", err)
		}
	}
	if cfg.Backup.Encryption.Enabled {
		if opts.Recipients, err = backup.ParseRecipients(cfg.Backup.Encryption.Recipients); err != nil {
			return fail("invalid encryption settings", err)
		}
	}
	meta, err := backup.Take(ctx, opts)
	if err != nil {
		return fail("backup failed", err)
	}
	fmt.Fprintf(e.stdout, "generation %s\n", meta.ID)
	fmt.Fprintf(e.stdout, "  dump      %s (%d bytes, encrypted: %t)\n", backup.Key(meta.ID, meta.DumpFile), meta.DumpSize, meta.Encrypted)
	fmt.Fprintf(e.stdout, "  sha256    %s\n", meta.DumpSHA256)
	fmt.Fprintf(e.stdout, "  tables    %d\n", len(meta.RowCounts))
	for _, m := range meta.Migrations {
		fmt.Fprintf(e.stdout, "  %-25s %s\n", m.Table, migrationLabel(m))
	}
	return 0
}

func list(ctx context.Context, e env, args []string) int {
	fs := cliflag.New("backup list", e.stderr)
	configPath := fs.String("config", defaultConfigPath, "path to configuration file")
	asJSON := fs.Bool("json", false, "print the generations as JSON")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup list: failed to load config: %v\n", err)
		return 1
	}
	st, err := setup(e, cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup list: %v\n", err)
		return 1
	}
	gens, err := backup.ListGenerations(ctx, st)
	if err != nil {
		fmt.Fprintf(e.stderr, "elythia backup list: %v\n", err)
		return 1
	}
	if *asJSON {
		return printJSON(e, gens)
	}
	tw := tabwriter.NewWriter(e.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tSIZE\tENCRYPTED\tVERIFIED\tELYTHIA\tMIGRATION")
	for _, g := range gens {
		status, encrypted, version, migration := "complete", "-", "-", "-"
		switch {
		case g.MetaError != nil:
			status = "unreadable"
		case g.Meta == nil:
			status = "incomplete"
		default:
			encrypted = fmt.Sprint(g.Meta.Encrypted)
			version = g.Meta.ElythiaVersion
			if len(g.Meta.Migrations) > 0 {
				migration = migrationLabel(g.Meta.Migrations[0])
			}
		}
		verified := "-"
		if g.Verify != nil {
			verified = map[bool]string{true: "ok", false: "failed"}[g.Verify.OK]
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\t%s\t%s\n", g.ID, status, g.Size, encrypted, verified, version, migration)
	}
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}

// listedGeneration is the JSON form of one row of "backup list -json".
type listedGeneration struct {
	ID        string               `json:"id"`
	Complete  bool                 `json:"complete"`
	Size      int64                `json:"size"`
	Meta      *backup.Meta         `json:"meta,omitempty"`
	MetaError string               `json:"metaError,omitempty"`
	Verify    *backup.VerifyResult `json:"verify,omitempty"`
}

func printJSON(e env, gens []backup.Generation) int {
	out := make([]listedGeneration, 0, len(gens))
	for _, g := range gens {
		lg := listedGeneration{ID: g.ID, Complete: g.Complete(), Size: g.Size, Meta: g.Meta, Verify: g.Verify}
		if g.MetaError != nil {
			lg.MetaError = g.MetaError.Error()
		}
		out = append(out, lg)
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		return 1
	}
	return 0
}

func migrationLabel(m backup.MigrationState) string {
	switch {
	case m.Missing:
		return "missing"
	case m.Version == backup.NilMigrationVersion:
		return "empty"
	case m.Dirty:
		return fmt.Sprintf("%d (dirty)", m.Version)
	default:
		return fmt.Sprint(m.Version)
	}
}
