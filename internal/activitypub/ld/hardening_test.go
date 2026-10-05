package ld_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub/ld"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckForForbiddenDirectives_DetectsAtGraph(t *testing.T) {
	p := ld.NewProcessor()
	err := p.CheckForForbiddenDirectives(map[string]any{
		"@graph": []any{
			map[string]any{"id": "https://example.com/notes/n1"},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ld.ErrForbiddenDirective)
}

func TestCheckForForbiddenDirectives_DetectsAtReverse(t *testing.T) {
	p := ld.NewProcessor()
	err := p.CheckForForbiddenDirectives(map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Note",
		"@reverse": map[string]any{"creator": "https://evil.example/spoof"},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ld.ErrForbiddenDirective)
}

func TestCheckForForbiddenDirectives_DetectsAtIncluded(t *testing.T) {
	p := ld.NewProcessor()
	err := p.CheckForForbiddenDirectives(map[string]any{
		"@included": []any{
			map[string]any{"type": "Note"},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ld.ErrForbiddenDirective)
}

func TestCheckForForbiddenDirectives_AllowsCleanDocument(t *testing.T) {
	p := ld.NewProcessor()
	err := p.CheckForForbiddenDirectives(map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Note",
		"id":       "https://example.com/notes/n1",
		"content":  "hello",
		"to":       []any{"https://www.w3.org/ns/activitystreams#Public"},
	})
	require.NoError(t, err)
}

func TestCheckForForbiddenDirectives_DetectsNested(t *testing.T) {
	p := ld.NewProcessor()
	err := p.CheckForForbiddenDirectives(map[string]any{
		"object": []any{
			map[string]any{
				"nested": map[string]any{
					"@graph": "deep!",
				},
			},
		},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, ld.ErrForbiddenDirective)
}

func TestFreeze_BlocksSubsequentContextFetch(t *testing.T) {
	p := ld.NewProcessor()
	// Freeze する前は preload 経由で取得できる
	_, err := p.LoadDocument("https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)

	p.Freeze()

	// Freeze 後は preload と cache hit のみ許可。それ以外は ErrCacheFrozen で
	// fail-closed。
	//
	// **遮断の確認に preload 済みの URL を使わないこと。** 以前ここは
	// `w3id.org/security/v1` を使っており、「未 cache」と「remote」を混同して
	// **preload まで塞ぐ実装 (#2680) を固定していた**。その状態では
	// LD-Signature 検証が常に失敗する。
	_, err = p.LoadDocument("https://remote.example/context.json")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ld.ErrCacheFrozen),
		"freeze 後に preload 外の URL fetch は ErrCacheFrozen を返すこと")
}

func TestFreeze_CachedURLsRemainAccessible(t *testing.T) {
	p := ld.NewProcessor()
	_, err := p.LoadDocument("https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)
	p.Freeze()
	// すでに cache 済の URL は freeze 後も読める (= verify 中の compact /
	// normalize で同じ context を re-resolve するときに死なないため)。
	doc, err := p.LoadDocument("https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)
	require.NotNil(t, doc)
}

// #2680: freeze は remote fetch を塞ぐためのもので、go:embed 済みの preload
// context は塞がない。**production と同じ順序** (空 cache のまま Freeze) を通す。
//
// これが無いと LD-Signature 検証が常に失敗する。verifier は cache を温めずに
// Freeze してから verify するので、verify が差し込む identity/v1 が解決できない。
func TestFreeze_PreloadedContextsRemainAvailable(t *testing.T) {
	for _, iri := range []string{
		"https://w3id.org/identity/v1",
		"https://www.w3.org/ns/activitystreams",
		"https://w3id.org/security/v1",
	} {
		t.Run(iri, func(t *testing.T) {
			// 温めない。本番の verifier と同じ状態にする。
			p := ld.NewProcessor()
			p.Freeze()
			doc, err := p.LoadDocument(iri)
			require.NoError(t, err, "preload 済み context が freeze 後に引けない")
			require.NotNil(t, doc)
		})
	}
}

// 遮断そのものは維持する。preload 以外は freeze 後に取れない。
func TestFreeze_NonPreloadedStillBlocked(t *testing.T) {
	p := ld.NewProcessor()
	p.Freeze()
	_, err := p.LoadDocument("https://evil.example/ctx.json")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ld.ErrCacheFrozen),
		"preload 以外は freeze 後も ErrCacheFrozen であること")
}
