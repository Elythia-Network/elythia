package federation_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/core/federation"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
)

type qdAllow struct{}

func (qdAllow) IsBlocked(string, string) (bool, error)   { return false, nil }
func (qdAllow) IsFollowing(string, string) (bool, error) { return false, nil }

type qdStore struct{ n int }

func (s *qdStore) Ensure(a *model.NoteQuoteAuthorization) (*model.NoteQuoteAuthorization, error) {
	s.n++
	return a, nil
}

type qdResponder struct{ accepted, rejected int }

func (r *qdResponder) SendQuoteAccept(*model.Note, *model.NoteQuoteAuthorization, *model.User) error {
	r.accepted++
	return nil
}

func (r *qdResponder) SendQuoteReject(*model.Note, string, string, *model.User) error {
	r.rejected++
	return nil
}

func TestProcess_QuoteRequestDispatch(t *testing.T) {
	quoteRequest := func(typ, id string) []byte {
		raw, err := json.Marshal(map[string]any{
			"id": id, "type": typ, "actor": "https://remote.example/users/alice",
			"object": "https://example.com/notes/n1",
			"instrument": map[string]any{
				"id": "https://remote.example/notes/q1", "type": "Note",
				"attributedTo":   "https://remote.example/users/alice",
				"_misskey_quote": "https://example.com/notes/n1",
			},
		})
		require.NoError(t, err)
		return raw
	}

	p, users, _, notes := newProcessor(t, aliceActor)
	text := "hi"
	users.Users["bob"] = &model.User{ID: "bob", Username: "bob"}
	notes.Notes["n1"] = &model.Note{ID: "n1", UserID: "bob", Text: &text, Visibility: model.NoteVisibilityPublic}

	// 未配線なら「対応していない」扱い。
	assert.ErrorIs(t, p.Process(quoteRequest("QuoteRequest", "https://remote.example/qr/0")), federation.ErrUnsupportedActivity)

	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	store := &qdStore{}
	resp := &qdResponder{}
	p.SetQuoteRequestHandler(federation.NewQuoteRequestHandler(federation.QuoteRequestDeps{
		Notes: notes, Users: users, Blocks: qdAllow{}, Follows: qdAllow{}, Approvals: store,
		FetchNote: func(string) (*activitypub.Note, error) {
			t.Fatal("inline instrument must not be fetched")
			return nil, nil
		},
		Respond: resp, URLs: activitypub.NewURLBuilder("https://example.com"), IDGen: gen,
	}))

	require.NoError(t, p.Process(quoteRequest("QuoteRequest", "https://remote.example/qr/1")))
	// JSON-LD で展開された型 (compact できなかったとき) も受ける。
	require.NoError(t, p.Process(quoteRequest("https://w3id.org/fep/044f#QuoteRequest", "https://remote.example/qr/2")))
	assert.Equal(t, 2, resp.accepted)

	// activity の id が actor と別のホストなら答えない (他人の activity の id を返さない)。
	require.NoError(t, p.Process(quoteRequest("QuoteRequest", "https://evil.example/qr/3")))
	require.NoError(t, p.Process(quoteRequest("QuoteRequest", "")))
	assert.Equal(t, 2, resp.accepted)
	assert.Equal(t, 0, resp.rejected)
	assert.Equal(t, 2, store.n)
}

// 引用した actor を解決できないとき: 恒久的な失敗は ack、一時的な失敗は再試行。
func TestProcess_QuoteRequestActorUnresolvable(t *testing.T) {
	raw := []byte(`{"id":"https://remote.example/qr/1","type":"QuoteRequest","actor":"https://remote.example/users/alice","object":"https://example.com/notes/n1","instrument":"https://remote.example/notes/q1"}`)
	for name, tc := range map[string]struct {
		err     error
		wantErr bool
	}{
		"gone":      {err: &activitypub.StatusError{StatusCode: 410}},
		"transient": {err: errors.New("connection reset"), wantErr: true},
	} {
		users := testutil.NewMockUserRepository()
		notes := testutil.NewMockNoteRepository()
		gen, _ := id.NewGenerator("aidx")
		urls := activitypub.NewURLBuilder("https://example.com")
		resolver := federation.NewResolver(users, notes, urls, &stubFetcher{err: tc.err}, gen)
		p := federation.NewProcessor(resolver, nil, nil, nil, users, notes)
		resp := &qdResponder{}
		p.SetQuoteRequestHandler(federation.NewQuoteRequestHandler(federation.QuoteRequestDeps{
			Notes: notes, Users: users, Blocks: qdAllow{}, Follows: qdAllow{}, Approvals: &qdStore{},
			Respond: resp, URLs: urls, IDGen: gen,
		}))
		err := p.Process(raw)
		if tc.wantErr {
			assert.Error(t, err, name)
		} else {
			assert.NoError(t, err, name)
		}
		assert.Zero(t, resp.accepted+resp.rejected, name)
	}
}
