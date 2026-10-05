package ap

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

type qaStore map[string]*model.NoteQuoteAuthorization

func (s qaStore) FindByIDAndNoteID(id, noteID string) (*model.NoteQuoteAuthorization, error) {
	if id == "dberr" {
		return nil, errors.New("db down")
	}
	a, ok := s[id]
	if !ok || a.NoteID != noteID {
		return nil, repository.ErrNotFound
	}
	return a, nil
}

type qaGate struct{ disabled bool }

func (qaGate) IsBlocked(string) bool      { return false }
func (qaGate) IsAllowed(string) bool      { return true }
func (g qaGate) FederationDisabled() bool { return g.disabled }

func qaReq(t *testing.T, noteID, authID string) (echo.Context, func() (int, string, http.Header)) {
	t.Helper()
	c, rec := newReq(t, "id", noteID)
	c.SetParamNames("id", "authId")
	c.SetParamValues(noteID, authID)
	return c, func() (int, string, http.Header) { return rec.Code, rec.Body.String(), rec.Header() }
}

func TestQuoteAuthorization(t *testing.T) {
	h, userRepo, noteRepo, _ := newHandler(t)
	userRepo.Users["u1"] = &model.User{ID: "u1", Username: "alice"}
	userRepo.Users["u3"] = &model.User{ID: "u3", Username: "carol", IsSuspended: true}
	userRepo.Users["u4"] = &model.User{ID: "u4", Username: "dave", IsDeleted: true}
	host := "remote.example"
	text := "hi"
	noteRepo.Notes["n1"] = &model.Note{ID: "n1", UserID: "u1", Text: &text, Visibility: model.NoteVisibilityPublic}
	// followers 限定の投稿への承認も配る (第三者が確かめに来るため)。
	noteRepo.Notes["fol"] = &model.Note{ID: "fol", UserID: "u1", Text: &text, Visibility: model.NoteVisibilityFollowers}
	noteRepo.Notes["remote"] = &model.Note{ID: "remote", UserID: "u2", UserHost: &host, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["lonly"] = &model.Note{ID: "lonly", UserID: "u1", Visibility: model.NoteVisibilityPublic, LocalOnly: true}
	noteRepo.Notes["susp"] = &model.Note{ID: "susp", UserID: "u3", Text: &text, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["del"] = &model.Note{ID: "del", UserID: "u4", Text: &text, Visibility: model.NoteVisibilityPublic}
	noteRepo.Notes["orphan"] = &model.Note{ID: "orphan", UserID: "nobody", Text: &text, Visibility: model.NoteVisibilityPublic}
	store := qaStore{
		"qa1": {ID: "qa1", NoteID: "n1", QuotingURI: "https://remote.example/statuses/1"},
		"qa2": {ID: "qa2", NoteID: "fol", QuotingURI: "https://remote.example/statuses/2"},
		"qa3": {ID: "qa3", NoteID: "remote", QuotingURI: "https://remote.example/statuses/3"},
		"qa4": {ID: "qa4", NoteID: "lonly", QuotingURI: "https://remote.example/statuses/4"},
		"qa5": {ID: "qa5", NoteID: "susp", QuotingURI: "https://remote.example/statuses/5"},
		"qa7": {ID: "qa7", NoteID: "del", QuotingURI: "https://remote.example/statuses/7"},
		"qa6": {ID: "qa6", NoteID: "orphan", QuotingURI: "https://remote.example/statuses/6"},
	}

	// 未配線なら 404。
	c, res := qaReq(t, "n1", "qa1")
	require.NoError(t, h.QuoteAuthorization(c))
	code, _, _ := res()
	assert.Equal(t, http.StatusNotFound, code)

	h.SetNoteRepo(noteRepo)
	h.SetQuoteAuthorizationStore(store)

	for _, tc := range []struct{ note, auth, target string }{{"n1", "qa1", "n1"}, {"fol", "qa2", "fol"}} {
		c, res = qaReq(t, tc.note, tc.auth)
		require.NoError(t, h.QuoteAuthorization(c))
		code, body, hdr := res()
		require.Equal(t, http.StatusOK, code, tc.auth)
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(body), &m))
		assert.Equal(t, "QuoteAuthorization", m["type"])
		assert.Equal(t, "https://example.com/notes/"+tc.target+"/quote-authorizations/"+tc.auth, m["id"])
		assert.Equal(t, "https://example.com/users/u1", m["attributedTo"])
		assert.Equal(t, "https://example.com/notes/"+tc.target, m["interactionTarget"])
		assert.Equal(t, store[tc.auth].QuotingURI, m["interactingObject"])
		assert.Contains(t, hdr.Get("Content-Type"), "application/activity+json")
		assert.Equal(t, "public, max-age=180", hdr.Get("Cache-Control"))
	}

	for name, tc := range map[string]struct {
		note, auth string
		want       int
	}{
		"other note's approval": {"fol", "qa1", http.StatusNotFound},
		"missing approval":      {"n1", "nope", http.StatusNotFound},
		"missing note":          {"nope", "qa1", http.StatusNotFound},
		"remote note":           {"remote", "qa3", http.StatusNotFound},
		"local only note":       {"lonly", "qa4", http.StatusNotFound},
		"approval lookup fails": {"n1", "dberr", http.StatusInternalServerError},
		// 凍結された作者の承認は配らない (投稿そのものも配っていない)。
		"suspended author": {"susp", "qa5", http.StatusNotFound},
		"missing author":   {"orphan", "qa6", http.StatusNotFound},
		"deleted author":   {"del", "qa7", http.StatusNotFound},
	} {
		c, res = qaReq(t, tc.note, tc.auth)
		require.NoError(t, h.QuoteAuthorization(c))
		code, _, _ := res()
		assert.Equal(t, tc.want, code, name)
	}

	noteRepo.FindErr = errors.New("db down")
	c, res = qaReq(t, "n1", "qa1")
	require.NoError(t, h.QuoteAuthorization(c))
	code, _, _ = res()
	assert.Equal(t, http.StatusInternalServerError, code)
	noteRepo.FindErr = nil

	userRepo.FindErr = errors.New("db down")
	c, res = qaReq(t, "n1", "qa1")
	require.NoError(t, h.QuoteAuthorization(c))
	code, _, _ = res()
	assert.Equal(t, http.StatusInternalServerError, code, "an author lookup failure is not a 404")
	userRepo.FindErr = nil

	h.SetFederationGate(qaGate{disabled: true}, "example.com")
	c, res = qaReq(t, "n1", "qa1")
	require.NoError(t, h.QuoteAuthorization(c))
	code, _, _ = res()
	assert.Equal(t, http.StatusForbidden, code)
}
