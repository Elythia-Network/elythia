// Command schemadrift regenerates the golden snapshot of Misskey TS table
// columns used by the schema drift gate in internal/entitycompat.
//
// The gate detects columns that mk-go's migrations define **only inside**
// `CREATE TABLE IF NOT EXISTS` and that upstream does not have. Such columns
// silently fail to appear on a TS-created database (the CREATE is a no-op
// there), so any mk-go code that reads or writes them breaks on drop-in only
// — a class of bug that shipped three times before it was caught (#2243:
// app.createdAt / auth_session.createdAt / clip.notesCount).
//
// Columns added with `ALTER TABLE ... ADD COLUMN` are safe (idempotent on both
// shapes) and are therefore ignored.
//
// Regenerate whenever UPSTREAM_MISSKEY_VERSION is bumped (run `make upstream-fetch` first):
//
//	go run ./tools/schemadrift
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"

	"github.com/shiroha-a/mk/internal/upstreamsrc"
)

const (
	goldenPath           = "internal/entitycompat/testdata/golden_upstream_columns.json"
	migrationsGoldenPath = "internal/entitycompat/testdata/golden_upstream_migrations.json"
)

var (
	entityRe = regexp.MustCompile(`@Entity\('([^']+)'\)`)
	// TypeORM の未実行判定に使われる migration 名。`name = '...'` プロパティが
	// あればそれ、無ければ class 名 (MigrationExecutor の
	// `migration.name ?? migration.constructor.name`)。
	migrationNameRe  = regexp.MustCompile(`(?m)^\s*name\s*=\s*'([^']+)'`)
	migrationClassRe = regexp.MustCompile(`export class (\w+)`)
	// @Column / @PrimaryColumn / @PrimaryGeneratedColumn の後に来る最初の
	// `public <name>` を列名とみなす。@Index 等が間に挟まることがあるので、
	// relation decorator だけを境界にする (relation プロパティは列を作らない)。
	columnRe   = regexp.MustCompile(`@(?:Primary(?:Generated)?)?Column\(`)
	publicRe   = regexp.MustCompile(`public\s+(\w+)\s*[:!?]`)
	relationRe = regexp.MustCompile(`\n\s*@(?:ManyToOne|OneToOne|OneToMany|ManyToMany)\b`)
)

func main() {
	// 本家は submodule ではなく .cache/misskey/<版> から読む (#3378)。
	up, _ := upstreamsrc.Dir(".")
	modelsFlag := flag.String("models", filepath.Join(up, "packages/backend/src/models"), "path to Misskey backend models dir")
	migrationsFlag := flag.String("migrations", filepath.Join(up, "packages/backend/migration"), "path to Misskey backend migration dir")
	flag.Parse()
	upstreamModelsDir, upstreamMigrationsDir := *modelsFlag, *migrationsFlag
	if err := upstreamsrc.Check(upstreamModelsDir); err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	tables, err := collect(upstreamModelsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	if len(tables) == 0 {
		fmt.Fprintf(os.Stderr, "schemadrift: no entities found under %s\n", upstreamModelsDir)
		os.Exit(1)
	}
	blob, err := json.MarshalIndent(tables, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(goldenPath, append(blob, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d tables)\n", goldenPath, len(tables))

	migrations, err := collectMigrationNames(upstreamMigrationsDir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	if len(migrations) == 0 {
		fmt.Fprintf(os.Stderr, "schemadrift: no migrations found under %s\n", upstreamMigrationsDir)
		os.Exit(1)
	}
	mblob, err := json.MarshalIndent(migrations, "", "  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(migrationsGoldenPath, append(mblob, '\n'), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "schemadrift:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d migrations)\n", migrationsGoldenPath, len(migrations))
}

// collectMigrationNames returns the TypeORM migration names, sorted by the
// 13-digit timestamp embedded in each name.
//
// 1690796169261-play-visibility.js のように class 名と name プロパティが
// 食い違う例があるので、TypeORM と同じ優先順位で解決する。
func collectMigrationNames(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".js" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		name := ""
		if m := migrationNameRe.FindSubmatch(src); m != nil {
			name = string(m[1])
		} else if m := migrationClassRe.FindSubmatch(src); m != nil {
			name = string(m[1])
		} else {
			return nil, fmt.Errorf("%s: no migration name found", e.Name())
		}
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return tsOf(names[i]) < tsOf(names[j]) })
	return names, nil
}

// tsOf extracts the 13-digit timestamp TypeORM appends to every migration name.
func tsOf(name string) int64 {
	if len(name) < 13 {
		return 0
	}
	n, _ := strconv.ParseInt(name[len(name)-13:], 10, 64)
	return n
}

// collect maps upstream table name -> sorted column names.
func collect(dir string) (map[string][]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".ts" {
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		m := entityRe.FindSubmatch(src)
		if m == nil {
			continue
		}
		cols := map[string]bool{}
		for _, loc := range columnRe.FindAllIndex(src, -1) {
			rest := src[loc[1]:]
			pm := publicRe.FindSubmatchIndex(rest)
			if pm == nil {
				continue
			}
			if rm := relationRe.FindIndex(rest); rm != nil && rm[0] < pm[0] {
				continue
			}
			cols[string(rest[pm[2]:pm[3]])] = true
		}
		names := make([]string, 0, len(cols))
		for c := range cols {
			names = append(names, c)
		}
		sort.Strings(names)
		out[string(m[1])] = names
	}
	return out, nil
}
