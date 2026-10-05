// Package upstreamsrc locates the pristine upstream Misskey checkout that the
// parity tools and tests compare against (#3378).
//
// The checkout lives at `.cache/misskey/<version>/` (gitignored), where the
// version is the single line in UPSTREAM_MISSKEY_VERSION at the repository
// root. `make upstream-fetch` creates it. MK_UPSTREAM_DIR overrides the
// location (CI checks upstream out elsewhere, or an operator keeps a clone).
package upstreamsrc

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	// VersionFile is the repository-root file that pins the upstream version.
	VersionFile = "UPSTREAM_MISSKEY_VERSION"
	// EnvDir overrides the upstream checkout location.
	EnvDir = "MK_UPSTREAM_DIR"
	// EnvRequire makes tests fail instead of skip when the checkout is absent.
	EnvRequire = "MK_UPSTREAM_REQUIRE"
	// cacheDir is where `make upstream-fetch` puts per-version checkouts.
	cacheDir = ".cache/misskey"
)

// ErrNotFetched reports that the upstream checkout does not exist.
var ErrNotFetched = errors.New("upstream Misskey is not fetched")

// versionRe accepts a release tag such as 2026.10.0 or 2026.10.0-beta.1.
var versionRe = regexp.MustCompile(`^[0-9][0-9A-Za-z.\-]*$`)

// Version returns the pinned upstream version read from root/VersionFile.
func Version(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, VersionFile))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", VersionFile, err)
	}
	v := strings.TrimSpace(string(b))
	// 1 行に版だけを書く約束。空・複数行・パスに化けうる値は書き損じなので、推測
	// せずに落とす (`..` を通すと Dir が .cache/misskey の外を指す)。
	if !versionRe.MatchString(v) || strings.Contains(v, "..") {
		return "", fmt.Errorf("%s must hold a single version, got %q", VersionFile, v)
	}
	return v, nil
}

// Dir returns the upstream checkout location without checking that it exists:
// MK_UPSTREAM_DIR when set (relative values are resolved against root),
// otherwise root/.cache/misskey/<version>.
func Dir(root string) (string, error) {
	if d := os.Getenv(EnvDir); d != "" {
		// 相対パスはリポジトリ直下を基準にする。テストはパッケージのディレクトリを
		// cwd にして走るので、そのまま返すと tools とテストで別の場所を見る。
		if !filepath.IsAbs(d) {
			d = filepath.Join(root, d)
		}
		return d, nil
	}
	v, err := Version(root)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, cacheDir, v), nil
}

// Locate returns Dir and fails with ErrNotFetched (and a hint to run
// `make upstream-fetch`) when the checkout is absent.
func Locate(root string) (string, error) {
	d, err := Dir(root)
	if err != nil {
		return "", err
	}
	if err := Check(d); err != nil {
		return "", err
	}
	return d, nil
}

// Check fails with ErrNotFetched when path does not exist.
func Check(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("%w: %s が無い。`make upstream-fetch` で本家を取得するか、%s で場所を指定する", ErrNotFetched, path, EnvDir)
	}
	return nil
}

// Required reports whether MK_UPSTREAM_REQUIRE asks tests to fail instead of
// skipping when the checkout is absent. Any non-empty value enables it.
func Required() bool {
	return os.Getenv(EnvRequire) != ""
}
