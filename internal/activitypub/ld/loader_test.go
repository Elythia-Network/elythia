package ld_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/activitypub/ld"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPreloadedLoader_LoadsActivityStreams(t *testing.T) {
	l := ld.NewPreloadedLoader()
	doc, err := l.LoadDocument("https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)
	require.NotNil(t, doc)
	// AS 2.0 context は @context object に "Activity" / "Note" 等を含む。
	m, ok := doc.Document.(map[string]any)
	require.True(t, ok, "AS 2.0 context root must be a JSON object")
	ctx, ok := m["@context"].(map[string]any)
	require.True(t, ok, "AS 2.0 root must have @context object")
	_, hasActivity := ctx["Activity"]
	assert.True(t, hasActivity, "AS 2.0 context must define Activity term")
}

func TestPreloadedLoader_LoadsSecurityV1(t *testing.T) {
	l := ld.NewPreloadedLoader()
	doc, err := l.LoadDocument("https://w3id.org/security/v1")
	require.NoError(t, err)
	require.NotNil(t, doc)
}

func TestPreloadedLoader_LoadsIdentityV1(t *testing.T) {
	l := ld.NewPreloadedLoader()
	doc, err := l.LoadDocument("https://w3id.org/identity/v1")
	require.NoError(t, err)
	require.NotNil(t, doc)
}

func TestPreloadedLoader_RejectsUnknownIRI(t *testing.T) {
	l := ld.NewPreloadedLoader()
	_, err := l.LoadDocument("https://evil.example/malicious-context")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ld.ErrContextNotPreloaded),
		"unknown URL must return ErrContextNotPreloaded sentinel for caller gating")
}

func TestProcessor_NormalizeActivityStreamsNote(t *testing.T) {
	p := ld.NewProcessor()
	// 最小 Create(Note) を URDNA2015 で正規化できることを確認 (= context fetch
	// が PRELOADED 経由で完結する steady state)。
	doc := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"id":       "https://example.com/notes/n1",
		"type":     "Note",
		"content":  "hello",
	}
	nq, err := p.Normalize(doc)
	require.NoError(t, err)
	assert.NotEmpty(t, nq, "n-quads output should be non-empty for a Note")
}

// Compact は upstream の @context shape (= raw context object) を引数に取り、
// AS 2.0 context に対して shape を正規化する経路を最低限カバーする。
func TestProcessor_CompactWithActivityStreamsContext(t *testing.T) {
	p := ld.NewProcessor()
	doc := map[string]any{
		"@context": "https://www.w3.org/ns/activitystreams",
		"type":     "Note",
		"id":       "https://example.com/notes/n1",
		"content":  "hello",
	}
	out, err := p.Compact(doc, "https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "Note", out["type"], "compact 後も type=Note を保持")
}

// Normalize に不正 doc を渡したときに error path を通ることを確認。
func TestProcessor_NormalizeMalformedReturnsError(t *testing.T) {
	p := ld.NewProcessor()
	// json-gold は invalid @context (= 数値) で error を返す。
	_, err := p.Normalize(map[string]any{
		"@context": 12345,
		"type":     "Note",
	})
	require.Error(t, err)
}

// loadDocument の cache hit 経路 (= 同 URL を 2 度 fetch) を test。
func TestProcessor_LoadDocumentCacheHit(t *testing.T) {
	p := ld.NewProcessor()
	doc1, err := p.LoadDocument("https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)
	doc2, err := p.LoadDocument("https://www.w3.org/ns/activitystreams")
	require.NoError(t, err)
	// 同 instance を返す (= cache hit)
	assert.Same(t, doc1, doc2)
}

// Compact に不正 doc を渡したときに error path を通ることを確認。
func TestProcessor_CompactReturnsError(t *testing.T) {
	p := ld.NewProcessor()
	// json-gold は invalid @context (= 数値) で error を返す。
	_, err := p.Compact(map[string]any{
		"@context": 12345,
		"type":     "Note",
	}, "https://www.w3.org/ns/activitystreams")
	require.Error(t, err)
}

// freeze 後に preload set 外 URL を要求すると ErrCacheFrozen を返す
// (= loadDocument が cache miss + frozen の経路)。
func TestProcessor_LoadDocumentFreezeBlocksUnknownURL(t *testing.T) {
	p := ld.NewProcessor()
	p.Freeze()
	_, err := p.LoadDocument("https://evil.example/unknown")
	require.Error(t, err)
	assert.ErrorIs(t, err, ld.ErrCacheFrozen)
}
