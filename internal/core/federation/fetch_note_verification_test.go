package federation_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/core/federation"
)

const verifyNoteBody = `{
	"@context": "https://www.w3.org/ns/activitystreams",
	"id": "https://remote.example/statuses/1",
	"type": "Note",
	"attributedTo": "https://remote.example/users/bob",
	"quote": "https://example.com/notes/n1",
	"content": "quoting"
}`

// QuoteRequest の instrument を確かめるための取得は、取り込まずに読むだけ (#3234)。
// 取り込むと後から届く Create が「既にある」になり、引用の通知が飛ばされる。
func TestFetchNoteForVerification(t *testing.T) {
	r, _ := newResolverWithFetcher(t, &finalURLStubFetcher{body: []byte(verifyNoteBody), finalURL: "https://remote.example/statuses/1"})
	note, err := r.FetchNoteForVerification("https://remote.example/statuses/1")
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/statuses/1", note.ID)
	assert.Equal(t, "https://example.com/notes/n1", string(note.Quote))
}

func TestFetchNoteForVerification_DoesNotIngest(t *testing.T) {
	r, _, notes := newGatedResolver(t, verifyNoteBody, nil)
	_, err := r.FetchNoteForVerification("https://remote.example/statuses/1")
	require.NoError(t, err)
	assert.Empty(t, notes.Notes, "the quoting note is not stored")
}

func TestFetchNoteForVerification_Rejects(t *testing.T) {
	// 応答したホストと id のホストが違う (なりすまし)。
	r, _ := newResolverWithFetcher(t, &finalURLStubFetcher{body: []byte(verifyNoteBody), finalURL: "https://evil.example/statuses/1"})
	_, err := r.FetchNoteForVerification("https://remote.example/statuses/1")
	assert.ErrorIs(t, err, federation.ErrObjectHostMismatch)

	// 要求したホストと id のホストが違う。
	r, _ = newResolverWithFetcher(t, &finalURLStubFetcher{body: []byte(verifyNoteBody), finalURL: "https://remote.example/statuses/1"})
	_, err = r.FetchNoteForVerification("https://other.example/statuses/1")
	assert.ErrorIs(t, err, federation.ErrObjectHostMismatch)

	// AS の @context が無い。
	r, _ = newResolverWithFetcher(t, &finalURLStubFetcher{body: []byte(`{"id":"https://remote.example/statuses/1","type":"Note"}`), finalURL: "https://remote.example/statuses/1"})
	_, err = r.FetchNoteForVerification("https://remote.example/statuses/1")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)

	// JSON でない。
	r, _ = newResolverWithFetcher(t, &finalURLStubFetcher{body: []byte(`<html>`), finalURL: "https://remote.example/statuses/1"})
	_, err = r.FetchNoteForVerification("https://remote.example/statuses/1")
	assert.ErrorIs(t, err, federation.ErrInvalidNote)

	// 連合を止めているホストには取りに行かない。
	r, _, _ = newGatedResolver(t, verifyNoteBody, &stubHostBlocker{blocked: map[string]bool{"remote.example": true}})
	_, err = r.FetchNoteForVerification("https://remote.example/statuses/1")
	assert.ErrorIs(t, err, federation.ErrHostNotAllowed)

	// 取得の失敗はそのまま返す (一時的なものは inbox が再試行する)。
	r, _ = newResolverWithFetcher(t, &stubFetcher{err: errors.New("connection reset")})
	_, err = r.FetchNoteForVerification("https://remote.example/statuses/1")
	require.Error(t, err)
}
