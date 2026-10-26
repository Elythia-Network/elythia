package reversi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 本家の paramDef は整数の値を持つ小数 (10.0、1e1) を integer として通し、limit の
// 範囲外は丸めずに拒否する (#3843)。
func TestGames_IntegerParamsLikeUpstream(t *testing.T) {
	h, repo := newTestHandler()
	for _, id := range []string{"g1", "g2", "g3"} {
		g := sampleGame()
		g.ID = id
		g.IsStarted = true
		repo.games[id] = g
	}
	rec := post(h.Games, `{"limit":2.0,"sinceDate":1e1,"untilDate":4102444800000.0}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Len(t, out, 2)
	for _, body := range []string{`{"limit":0}`, `{"limit":-1}`, `{"limit":101}`} {
		rec := post(h.Games, body, nil)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM", body)
	}
}

// 本家は crc32 を required の string とするだけなので、空文字も通して保存済みの値と
// 比べる。キーが無ければ今までどおり 400 (#3843)。
func TestVerify_EmptyCRC32IsCompared(t *testing.T) {
	h, repo := newTestHandler()
	g := sampleGame()
	stored := "123456789"
	g.CRC32 = &stored
	repo.games["g1"] = g
	rec := post(h.Verify, `{"gameId":"g1","crc32":""}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, true, resp["desynced"])

	assert.Contains(t, post(h.Verify, `{"gameId":"ghost","crc32":""}`, nil).Body.String(), "NO_SUCH_GAME")
	assert.Equal(t, http.StatusBadRequest, post(h.Verify, `{"gameId":"g1"}`, nil).Code)
}

// 本家 match.ts は相手を引けなければ NO_SUCH_USER を返す。以前は game 行を作ろうと
// して FK 違反の 500 になっていた。DB の障害は 500 のまま (#3843)。
func TestMatch_UnknownUserIsNoSuchUser(t *testing.T) {
	h, repo := newTestHandler()
	userRepo := testutil.NewMockUserRepository()
	h.SetFederation("https://example.com", nil, nil, userRepo)

	rec := post(h.Match, `{"userId":"ghost"}`, u1)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "0b4f0559-b484-4e31-9581-3f73cee89b28")
	assert.Empty(t, repo.games)

	userRepo.FindErr = errors.New("db down")
	rec = post(h.Match, `{"userId":"ghost"}`, u1)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Empty(t, repo.games)

	userRepo.FindErr = nil
	userRepo.Users["u2"] = &model.User{ID: "u2", Username: "bob", UsernameLower: "bob"}
	rec = post(h.Match, `{"userId":"u2"}`, u1)
	assert.Less(t, rec.Code, 300, rec.Body.String())
	assert.Len(t, repo.games, 1)
}
