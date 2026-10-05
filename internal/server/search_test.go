package server

import (
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	coresearch "github.com/elythia-network/elythia/internal/core/search"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pgroongaCapturingRepo records the last NoteSearchFilter so that tests can
// assert the Pgroonga flag wired by buildSearchProvider reaches the repository
// without depending on actual SQL execution.
type pgroongaCapturingRepo struct {
	*testutil.MockNoteRepository
	lastFilter model.NoteSearchFilter
}

func (r *pgroongaCapturingRepo) SearchByFilter(f model.NoteSearchFilter) ([]*model.Note, error) {
	r.lastFilter = f
	return r.MockNoteRepository.SearchByFilter(f)
}

func newRepoAndIDGen(t *testing.T) (*pgroongaCapturingRepo, id.Generator) {
	t.Helper()
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return &pgroongaCapturingRepo{MockNoteRepository: testutil.NewMockNoteRepository()}, idGen
}

func TestBuildSearchProvider_SQLLikeWhenUnset(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	p := buildSearchProvider(&config.Config{}, repo, nil, idGen)
	require.IsType(t, &coresearch.SQLLikeProvider{}, p)

	_, err := p.SearchNote(nil, "hello", coresearch.SearchOpts{}, coresearch.Pagination{Limit: 10})
	require.NoError(t, err)
	assert.False(t, repo.lastFilter.Pgroonga, "default provider must not use pgroonga")
}

func TestBuildSearchProvider_SQLLikeExplicit(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	cfg := &config.Config{FulltextSearch: &config.FulltextSearchOptions{Provider: "sqlLike"}}
	p := buildSearchProvider(cfg, repo, nil, idGen)
	require.IsType(t, &coresearch.SQLLikeProvider{}, p)

	_, err := p.SearchNote(nil, "hello", coresearch.SearchOpts{}, coresearch.Pagination{Limit: 10})
	require.NoError(t, err)
	assert.False(t, repo.lastFilter.Pgroonga)
}

func TestBuildSearchProvider_Pgroonga(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	cfg := &config.Config{FulltextSearch: &config.FulltextSearchOptions{Provider: "sqlPgroonga"}}
	p := buildSearchProvider(cfg, repo, nil, idGen)
	require.IsType(t, &coresearch.SQLLikeProvider{}, p)

	_, err := p.SearchNote(nil, "hello", coresearch.SearchOpts{}, coresearch.Pagination{Limit: 10})
	require.NoError(t, err)
	assert.True(t, repo.lastFilter.Pgroonga, "Pgroonga flag must be set when provider is sqlPgroonga")
}

func TestBuildSearchProvider_PgroongaCaseInsensitive(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	// upstream の YAML 表記は "sqlPgroonga"。strings.ToLower + TrimSpace で
	// 正規化されることを確認する。
	cfg := &config.Config{FulltextSearch: &config.FulltextSearchOptions{Provider: "  sqlPgroonga  "}}
	p := buildSearchProvider(cfg, repo, nil, idGen)

	_, err := p.SearchNote(nil, "hello", coresearch.SearchOpts{}, coresearch.Pagination{Limit: 10})
	require.NoError(t, err)
	assert.True(t, repo.lastFilter.Pgroonga)
}

func TestBuildSearchProvider_UnknownProviderFallsBackToSQLLike(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	cfg := &config.Config{FulltextSearch: &config.FulltextSearchOptions{Provider: "elasticsearch"}}
	p := buildSearchProvider(cfg, repo, nil, idGen)
	require.IsType(t, &coresearch.SQLLikeProvider{}, p)

	_, err := p.SearchNote(nil, "hello", coresearch.SearchOpts{}, coresearch.Pagination{Limit: 10})
	require.NoError(t, err)
	assert.False(t, repo.lastFilter.Pgroonga)
}

func TestBuildSearchProvider_MeilisearchSelected(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	cfg := &config.Config{
		FulltextSearch: &config.FulltextSearchOptions{Provider: "meilisearch"},
		Meilisearch:    &config.MeilisearchOptions{Host: "meili.local", Port: "7700"},
	}
	p := buildSearchProvider(cfg, repo, nil, idGen)
	require.IsType(t, &coresearch.MeilisearchProvider{}, p)
}

func TestBuildSearchProvider_MeilisearchWithoutHostFallsBack(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	cfg := &config.Config{
		FulltextSearch: &config.FulltextSearchOptions{Provider: "meilisearch"},
		Meilisearch:    &config.MeilisearchOptions{}, // host 未設定
	}
	p := buildSearchProvider(cfg, repo, nil, idGen)
	// Host 空なら sqlLike にフォールバック。
	require.IsType(t, &coresearch.SQLLikeProvider{}, p)
}

// TestBuildSearchProvider_NoneSelectsNoop verifies that fulltextSearch.provider
// = "none" returns a NoopProvider so notes/search responds with 400
// UNAVAILABLE (= upstream Misskey TS strict-mode、#877)。
func TestBuildSearchProvider_NoneSelectsNoop(t *testing.T) {
	repo, idGen := newRepoAndIDGen(t)
	cfg := &config.Config{FulltextSearch: &config.FulltextSearchOptions{Provider: "none"}}
	p := buildSearchProvider(cfg, repo, nil, idGen)
	require.IsType(t, &coresearch.NoopProvider{}, p)

	_, err := p.SearchNote(nil, "hello", coresearch.SearchOpts{}, coresearch.Pagination{Limit: 10})
	require.ErrorIs(t, err, coresearch.ErrUnavailable)
}

func TestBuildMeilisearchHost(t *testing.T) {
	cases := []struct {
		name string
		opts *config.MeilisearchOptions
		want string
	}{
		{
			name: "host with scheme is left untouched",
			opts: &config.MeilisearchOptions{Host: "http://meili.local:7700"},
			want: "http://meili.local:7700",
		},
		{
			name: "host without scheme picks http and includes port",
			opts: &config.MeilisearchOptions{Host: "meili.local", Port: "7700"},
			want: "http://meili.local:7700",
		},
		{
			name: "ssl flag promotes to https",
			opts: &config.MeilisearchOptions{Host: "meili.local", Port: "7700", SSL: true},
			want: "https://meili.local:7700",
		},
		{
			name: "missing port omits trailing colon",
			opts: &config.MeilisearchOptions{Host: "meili.local"},
			want: "http://meili.local",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, buildMeilisearchHost(tc.opts))
		})
	}
}
