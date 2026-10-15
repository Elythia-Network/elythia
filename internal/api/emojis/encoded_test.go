package emojis_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/api/emojis"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

func addEmojis(t *testing.T, repo *testutil.MockEmojiRepository, n int, prefix string) {
	t.Helper()
	for i := range n {
		require.NoError(t, repo.Create(&model.Emoji{
			ID:        fmt.Sprintf("%s%03d", prefix, i),
			Name:      fmt.Sprintf("%s_%03d", prefix, i),
			PublicURL: fmt.Sprintf("https://files.example/%s/%03d.png", prefix, i),
			Aliases:   []string{},
		}))
	}
}

func getEmojis(h *emojis.Handler, acceptEncoding string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/emojis", nil)
	if acceptEncoding != "" {
		req.Header.Set(echo.HeaderAcceptEncoding, acceptEncoding)
	}
	rec := httptest.NewRecorder()
	_ = h.Emojis(e.NewContext(req, rec))
	return rec
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return out
}

func emojiNames(t *testing.T, body []byte) []string {
	t.Helper()
	var got struct {
		Emojis []struct {
			Name string `json:"name"`
		} `json:"emojis"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	names := make([]string, 0, len(got.Emojis))
	for _, e := range got.Emojis {
		names = append(names, e.Name)
	}
	return names
}

func TestEmojis_ContentEncodingNegotiation(t *testing.T) {
	repo := testutil.NewMockEmojiRepository()
	addEmojis(t, repo, 50, "a")
	h := emojis.NewHandler(repository.NewCachedEmojiRepository(repo))

	plain := getEmojis(h, "")
	require.Equal(t, http.StatusOK, plain.Code)
	assert.Empty(t, plain.Header().Get(echo.HeaderContentEncoding))
	assert.Equal(t, "application/json; charset=utf-8", plain.Header().Get(echo.HeaderContentType))
	assert.Equal(t, echo.HeaderAcceptEncoding, plain.Header().Get(echo.HeaderVary))
	assert.Len(t, emojiNames(t, plain.Body.Bytes()), 50)

	gz := getEmojis(h, "gzip, deflate, br")
	require.Equal(t, http.StatusOK, gz.Code)
	assert.Equal(t, "gzip", gz.Header().Get(echo.HeaderContentEncoding))
	assert.Equal(t, echo.HeaderAcceptEncoding, gz.Header().Get(echo.HeaderVary))
	assert.Equal(t, plain.Body.Bytes(), gunzip(t, gz.Body.Bytes()))
	assert.Equal(t, fmt.Sprint(gz.Body.Len()), gz.Header().Get(echo.HeaderContentLength))

	refused := getEmojis(h, "gzip;q=0")
	assert.Empty(t, refused.Header().Get(echo.HeaderContentEncoding))
	assert.Equal(t, plain.Body.Bytes(), refused.Body.Bytes())
}

// 本文は、これまでの c.JSON (json.Encoder、末尾の改行付き) と同じバイト列。
func TestEmojis_BodyMatchesJSONEncoder(t *testing.T) {
	repo := testutil.NewMockEmojiRepository()
	cat := "<faces>&"
	require.NoError(t, repo.Create(&model.Emoji{ID: "x1", Name: "smile", Category: &cat, PublicURL: "https://e.example/a?b=1&c=2", Aliases: []string{"happy"}, LocalOnly: true}))
	h := emojis.NewHandler(repo)

	rec := getEmojis(h, "")
	var want bytes.Buffer
	require.NoError(t, json.NewEncoder(&want).Encode(map[string]any{"emojis": []map[string]any{{
		"name": "smile", "category": &cat, "aliases": []string{"happy"}, "url": "https://e.example/a?b=1&c=2", "localOnly": true,
	}}}))
	assert.Equal(t, want.String(), rec.Body.String())
}

// 小さな本文は圧縮しない (gzip の middleware の MinLength と同じ)。
func TestEmojis_SmallBodyIsNotCompressed(t *testing.T) {
	h := emojis.NewHandler(repository.NewCachedEmojiRepository(testutil.NewMockEmojiRepository()))
	rec := getEmojis(h, "gzip")
	assert.Empty(t, rec.Header().Get(echo.HeaderContentEncoding))
	assert.JSONEq(t, `{"emojis":[]}`, rec.Body.String())
}

// 符号化した本文は、絵文字の cache を捨てる契機 (書き込み) まで使い回し、
// 捨てた後の要求では新しい一覧を返す。
func TestEmojis_EncodedBodyFollowsRepositoryInvalidation(t *testing.T) {
	inner := testutil.NewMockEmojiRepository()
	addEmojis(t, inner, 30, "a")
	repo := repository.NewCachedEmojiRepository(inner)
	h := emojis.NewHandler(repo)

	first := getEmojis(h, "")
	assert.Len(t, emojiNames(t, first.Body.Bytes()), 30)

	// cache を経由しない書き込みは、cache を捨てるまで見えない (ListLocal と同じ)。
	addEmojis(t, inner, 1, "b")
	assert.Len(t, emojiNames(t, getEmojis(h, "").Body.Bytes()), 30)
	assert.Len(t, emojiNames(t, gunzip(t, getEmojis(h, "gzip").Body.Bytes())), 30)

	// cache の wrapper を通した書き込みで捨てられ、次の要求で作り直す。
	require.NoError(t, repo.Create(&model.Emoji{ID: "c000", Name: "c_000", PublicURL: "https://files.example/c.png", Aliases: []string{}}))
	assert.Len(t, emojiNames(t, getEmojis(h, "").Body.Bytes()), 32)
	assert.Len(t, emojiNames(t, gunzip(t, getEmojis(h, "gzip").Body.Bytes())), 32)

	// 他プロセスからの無効化 (pub/sub) でも同じ。
	addEmojis(t, inner, 1, "d")
	repo.ApplyRemoteInvalidation(repository.EmojiCacheInvalidation{All: true})
	assert.Len(t, emojiNames(t, getEmojis(h, "gzip;q=0").Body.Bytes()), 33)
}

// cache の無い repository (毎回新しい slice を返す) でも、毎回正しい一覧を返す。
func TestEmojis_UncachedRepositoryAlwaysFresh(t *testing.T) {
	inner := testutil.NewMockEmojiRepository()
	addEmojis(t, inner, 2, "a")
	h := emojis.NewHandler(inner)
	assert.Len(t, emojiNames(t, getEmojis(h, "").Body.Bytes()), 2)
	addEmojis(t, inner, 1, "b")
	assert.Len(t, emojiNames(t, getEmojis(h, "").Body.Bytes()), 3)
}
