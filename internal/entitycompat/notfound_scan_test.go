package entitycompat

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// notfound gate の抽出器自身のテスト。
//
// **これが無いと gate が空振りする。** allowlist が空になった今、gate は
// 「検出 0 件なら PASS」の向きなので、walk 対象を外しても述語を壊しても緑のまま
// 通る (実際、`internal/core` の walk を外す変異と sentinel 述語を壊す変異が
// どちらも生き残った)。
func writeGoFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, []byte("package x\n\n"+body), 0o644))
	return p
}

func TestScanCollapsedLookups_APILayer(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "api", "probe")
	p := writeGoFile(t, dir, "h.go", `
func (h *Handler) Collapsed(c *echo.Context) error {
	u, err := h.repo.FindByID("x")
	if err != nil {
		return c.JSON(http.StatusBadRequest, apierr.NotFound())
	}
	_ = u
	return nil
}

func (h *Handler) Guarded(c *echo.Context) error {
	u, err := h.repo.FindByID("x")
	if err != nil {
		if !repository.IsNotFound(err) {
			return c.JSON(http.StatusInternalServerError, apierr.InternalError())
		}
		return c.JSON(http.StatusBadRequest, apierr.NotFound())
	}
	_ = u
	return nil
}
`)
	got := scanCollapsedLookups(t, root, p)

	require.Len(t, got, 1, "潰している 1 件だけを拾う: %v", got)
	assert.Contains(t, got[0], "Collapsed")
}

// **core 層は 4xx ではなく domain sentinel で判定する。** ここが壊れると
// `internal/core` の検出が丸ごと 0 件になり、gate が黙る。
func TestScanCollapsedLookups_CoreLayer(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "internal", "core", "probe")
	p := writeGoFile(t, dir, "s.go", `
func (s *Service) Collapsed(id string) (*Thing, error) {
	v, err := s.repo.FindByID(id)
	if err != nil {
		return nil, ErrThingNotFound
	}
	return v, nil
}

func (s *Service) Guarded(id string) (*Thing, error) {
	v, err := s.repo.FindByID(id)
	if err != nil {
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrThingNotFound
	}
	return v, nil
}

func (s *Service) NotASentinel(id string) (*Thing, error) {
	v, err := s.repo.FindByID(id)
	if err != nil {
		return nil, err
	}
	return v, nil
}
`)
	got := scanCollapsedLookups(t, root, p)

	require.Len(t, got, 1, "潰している 1 件だけを拾う: %v", got)
	assert.Contains(t, got[0], "Collapsed")
}

// sentinel の判定そのもの。命名のゆれ (NotLiked / NoSuchXxx) も拾う。
func TestBodyReturnsNotFoundSentinel(t *testing.T) {
	for _, tt := range []struct {
		name string
		body string
		want bool
	}{
		{"ErrXxxNotFound", "return nil, ErrPageNotFound", true},
		{"ErrNotFound", "return ErrNotFound", true},
		{"ErrNotLiked", "return ErrNotLiked", true},
		{"ErrNoSuchUser", "return ErrNoSuchUser", true},
		{"パッケージ修飾", "return nil, corepage.ErrPageNotFound", true},
		{"raw error は sentinel ではない", "return nil, err", false},
		{"別の domain error は対象外", "return ErrAccessDenied", false},
		{"nil は対象外", "return nil", false},
		// **未知の命名も拾えること。** 述語を「not-found を意味する語の列挙」に
		// 戻すと、この 4 つが黙って素通りする — gate の目的そのものが果たせない。
		{"未知の命名 Missing", "return ErrThingMissing", true},
		{"未知の命名 Gone", "return ErrThingGone", true},
		{"未知の命名 Unknown", "return ErrUnknownThing", true},
		{"未知の命名 No", "return ErrNoThing", true},
		// **正しい形を検出し続けないこと。** ラップは種別を保つので潰しではない。
		{"ラップした raw error", `return nil, fmt.Errorf("find: %w", err)`, false},
		{"別名のローカル err", "return nil, cerr", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "internal", "core", "probe")
			p := writeGoFile(t, dir, "s.go", `
func (s *Service) F(id string) (*Thing, error) {
	v, err := s.repo.FindByID(id)
	if err != nil {
		`+tt.body+`
	}
	return v, nil
}
`)
			got := scanCollapsedLookups(t, root, p)
			assert.Equal(t, tt.want, len(got) == 1, "検出結果: %v", got)
		})
	}
}

// **walk 対象に `internal/core` が入っていること。** 外す変異は検出 0 件に
// なるだけなので、対象そのものを固定する。
func TestNotFoundGateWalksCoreLayer(t *testing.T) {
	assert.Contains(t, notFoundGateDirs, "internal/core",
		"internal/core を walk しないと service 層の潰しが検出できない (#2799)")
	assert.Contains(t, notFoundGateDirs, "internal/api")
	assert.Contains(t, notFoundGateDirs, "internal/server")
}
