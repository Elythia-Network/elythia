package upstreamsrc

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeVersion(t *testing.T, root, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(root, VersionFile), []byte(content), 0o644))
}

func TestVersion(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
		wantErr bool
	}{
		{name: "single line with newline", content: "2026.10.0\n", want: "2026.10.0"},
		{name: "surrounding whitespace is trimmed", content: "  2026.10.0  \n", want: "2026.10.0"},
		{name: "empty is rejected", content: "\n", wantErr: true},
		{name: "two lines are rejected", content: "2026.10.0\n2026.9.1\n", wantErr: true},
		{name: "inner space is rejected", content: "2026.10.0 x", wantErr: true},
		{name: "slash is rejected", content: "../x", wantErr: true},
		{name: "dot-dot is rejected", content: "..", wantErr: true},
		{name: "pre-release tag is accepted", content: "2026.10.0-beta.1\n", want: "2026.10.0-beta.1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeVersion(t, root, tc.content)
			got, err := Version(root)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVersion_MissingFile(t *testing.T) {
	_, err := Version(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), VersionFile)
}

func TestDir(t *testing.T) {
	t.Run("defaults to the per-version cache", func(t *testing.T) {
		t.Setenv(EnvDir, "")
		root := t.TempDir()
		writeVersion(t, root, "2026.10.0\n")
		got, err := Dir(root)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, ".cache", "misskey", "2026.10.0"), got)
	})
	t.Run("env overrides without reading the version file", func(t *testing.T) {
		t.Setenv(EnvDir, "/somewhere/misskey")
		got, err := Dir(t.TempDir())
		require.NoError(t, err)
		assert.Equal(t, "/somewhere/misskey", got)
	})
	t.Run("relative env is resolved against root", func(t *testing.T) {
		t.Setenv(EnvDir, "vendor/misskey")
		root := t.TempDir()
		got, err := Dir(root)
		require.NoError(t, err)
		assert.Equal(t, filepath.Join(root, "vendor", "misskey"), got)
	})
	t.Run("missing version file fails", func(t *testing.T) {
		t.Setenv(EnvDir, "")
		_, err := Dir(t.TempDir())
		require.Error(t, err)
	})
}

func TestLocate(t *testing.T) {
	t.Run("absent checkout reports ErrNotFetched with the fetch hint", func(t *testing.T) {
		t.Setenv(EnvDir, "")
		root := t.TempDir()
		writeVersion(t, root, "2026.10.0\n")
		_, err := Locate(root)
		require.ErrorIs(t, err, ErrNotFetched)
		assert.Contains(t, err.Error(), "make upstream-fetch")
	})
	t.Run("present checkout is returned", func(t *testing.T) {
		t.Setenv(EnvDir, "")
		root := t.TempDir()
		writeVersion(t, root, "2026.10.0\n")
		want := filepath.Join(root, ".cache", "misskey", "2026.10.0")
		require.NoError(t, os.MkdirAll(want, 0o755))
		got, err := Locate(root)
		require.NoError(t, err)
		assert.Equal(t, want, got)
	})
	t.Run("version error propagates", func(t *testing.T) {
		t.Setenv(EnvDir, "")
		_, err := Locate(t.TempDir())
		require.Error(t, err)
		assert.NotErrorIs(t, err, ErrNotFetched)
	})
}

func TestRequired(t *testing.T) {
	t.Setenv(EnvRequire, "1")
	assert.True(t, Required())
	t.Setenv(EnvRequire, "")
	assert.False(t, Required())
	t.Setenv(EnvRequire, "true")
	assert.True(t, Required(), "空でなければ有効 (MK_FRONTEND_GATES_REQUIRE_SUBMODULE と同じ約束)")
}
