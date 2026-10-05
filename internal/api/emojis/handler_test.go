package emojis_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/api/emojis"
	"github.com/elythia-network/elythia/internal/entitycompat/shapetest"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setup() (*emojis.Handler, *testutil.MockEmojiRepository) {
	repo := testutil.NewMockEmojiRepository()
	h := emojis.NewHandler(repo)
	return h, repo
}

func doPost(h *emojis.Handler) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/emojis", strings.NewReader("{}"))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Emojis(c)
	return rec
}

func TestEmojis_Empty(t *testing.T) {
	h, _ := setup()
	rec := doPost(h)
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr, ok := body["emojis"].([]any)
	require.True(t, ok)
	assert.Empty(t, arr)
}

func TestEmojis_WithLocalEmojis(t *testing.T) {
	h, repo := setup()
	cat := "faces"
	repo.Emojis["smile@"] = &model.Emoji{
		Name:      "smile",
		Category:  &cat,
		PublicURL: "https://example.com/emoji/smile.png",
		Aliases:   []string{"happy"},
	}

	rec := doPost(h)
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr, ok := body["emojis"].([]any)
	require.True(t, ok)
	assert.Len(t, arr, 1)

	emoji := arr[0].(map[string]any)
	assert.Equal(t, "smile", emoji["name"])
	assert.Equal(t, "faces", emoji["category"])
	assert.Equal(t, "https://example.com/emoji/smile.png", emoji["url"])
	shapetest.Assert(t, "EmojiSimple", emoji) // L3 (#1286)
}

// #1556 url は publicUrl 空のとき originalUrl に fallback、roleIds は length>0 の
// ときだけ emit する (upstream packSimple)。
func TestEmojis_URLFallbackAndRoleIds(t *testing.T) {
	h, repo := setup()
	// publicUrl 空 → originalUrl に fallback。roleIds 付き。
	repo.Emojis["a@"] = &model.Emoji{
		Name:                                    "a",
		PublicURL:                               "",
		OriginalURL:                             "https://example.com/emoji/a-orig.png",
		Aliases:                                 []string{},
		RoleIDsThatCanBeUsedThisEmojiAsReaction: []string{"role1", "role2"},
	}
	// roleIds 空 → field 省略。
	repo.Emojis["b@"] = &model.Emoji{
		Name:      "b",
		PublicURL: "https://example.com/emoji/b.png",
		Aliases:   []string{},
	}

	rec := doPost(h)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr := body["emojis"].([]any)
	byName := map[string]map[string]any{}
	for _, e := range arr {
		m := e.(map[string]any)
		byName[m["name"].(string)] = m
	}

	a := byName["a"]
	assert.Equal(t, "https://example.com/emoji/a-orig.png", a["url"], "publicUrl 空 → originalUrl fallback")
	roleIds, ok := a["roleIdsThatCanBeUsedThisEmojiAsReaction"].([]any)
	require.True(t, ok, "roleIds が length>0 のとき emit される")
	assert.Len(t, roleIds, 2)
	shapetest.Assert(t, "EmojiSimple", a)

	b := byName["b"]
	_, hasRole := b["roleIdsThatCanBeUsedThisEmojiAsReaction"]
	assert.False(t, hasRole, "roleIds 空のとき省略される")
}

// #1781 localOnly / isSensitive は upstream packSimple の `? true : undefined`
// に倣い、false のとき key 自体を省く。true のときだけ emit する。
func TestEmojis_LocalOnlyIsSensitiveOmittedWhenFalse(t *testing.T) {
	h, repo := setup()
	repo.Emojis["plain@"] = &model.Emoji{
		Name:        "plain",
		PublicURL:   "https://example.com/emoji/plain.png",
		Aliases:     []string{},
		LocalOnly:   false,
		IsSensitive: false,
	}
	repo.Emojis["flagged@"] = &model.Emoji{
		Name:        "flagged",
		PublicURL:   "https://example.com/emoji/flagged.png",
		Aliases:     []string{},
		LocalOnly:   true,
		IsSensitive: true,
	}

	rec := doPost(h)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr := body["emojis"].([]any)
	byName := map[string]map[string]any{}
	for _, e := range arr {
		m := e.(map[string]any)
		byName[m["name"].(string)] = m
	}

	plain := byName["plain"]
	_, hasLocalOnly := plain["localOnly"]
	assert.False(t, hasLocalOnly, "localOnly=false は省略される")
	_, hasIsSensitive := plain["isSensitive"]
	assert.False(t, hasIsSensitive, "isSensitive=false は省略される")
	shapetest.Assert(t, "EmojiSimple", plain)

	flagged := byName["flagged"]
	assert.Equal(t, true, flagged["localOnly"], "localOnly=true は emit される")
	assert.Equal(t, true, flagged["isSensitive"], "isSensitive=true は emit される")
	shapetest.Assert(t, "EmojiSimple", flagged)
}

// failingEmojiRepo always fails on ListLocal.
type failingEmojiRepo struct {
	*testutil.MockEmojiRepository
}

func (f *failingEmojiRepo) ListLocal() ([]*model.Emoji, error) {
	return nil, assert.AnError
}

func TestEmojis_RepoError_ReturnsEmptyArray(t *testing.T) {
	repo := &failingEmojiRepo{MockEmojiRepository: testutil.NewMockEmojiRepository()}
	h := emojis.NewHandler(repo)
	rec := doPost(h)
	assert.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr := body["emojis"].([]any)
	assert.Empty(t, arr)
}

func TestEmojis_ExcludesRemote(t *testing.T) {
	h, repo := setup()
	host := "remote.example.com"
	repo.Emojis["alien@remote.example.com"] = &model.Emoji{
		Name: "alien",
		Host: &host,
	}

	rec := doPost(h)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr := body["emojis"].([]any)
	assert.Empty(t, arr) // リモート絵文字は除外
}

func doGetEmojis(h *emojis.Handler) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/emojis", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Emojis(c)
	return rec
}

func TestEmojis_GET(t *testing.T) {
	h, repo := setup()
	cat := "faces"
	repo.Emojis["wave@"] = &model.Emoji{
		Name:      "wave",
		Category:  &cat,
		PublicURL: "https://example.com/emoji/wave.png",
	}
	rec := doGetEmojis(h)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	arr := body["emojis"].([]any)
	assert.Len(t, arr, 1)
}

// --- Emoji (singular) ---

func doGetEmoji(h *emojis.Handler, name string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodGet, "/api/emoji?name="+name, nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Emoji(c)
	return rec
}

func TestEmoji_GET_Found(t *testing.T) {
	h, repo := setup()
	repo.Emojis["smile@"] = &model.Emoji{
		ID:        "e2",
		Name:      "smile",
		PublicURL: "https://example.com/emoji/smile.png",
	}
	rec := doGetEmoji(h, "smile")
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "smile", body["name"])
}

func TestEmoji_GET_MissingName(t *testing.T) {
	h, _ := setup()
	rec := doGetEmoji(h, "")
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// /api/emoji は EmojiDetailed を返し、license と
// roleIdsThatCanBeUsedThisEmojiAsReaction を含む (以前は欠落していた)。
func TestEmoji_GET_IncludesLicenseAndRoleIds(t *testing.T) {
	h, repo := setup()
	lic := "CC-BY-4.0"
	repo.Emojis["smile@"] = &model.Emoji{
		ID:                                      "e3",
		Name:                                    "smile",
		OriginalURL:                             "https://example.com/emoji/smile.png",
		License:                                 &lic,
		RoleIDsThatCanBeUsedThisEmojiAsReaction: []string{"r1"},
	}
	rec := doGetEmoji(h, "smile")
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "CC-BY-4.0", body["license"])
	assert.Equal(t, []any{"r1"}, body["roleIdsThatCanBeUsedThisEmojiAsReaction"])
	// url は publicUrl 空時 originalUrl にフォールバックする。
	assert.Equal(t, "https://example.com/emoji/smile.png", body["url"])
}

func doPostEmoji(h *emojis.Handler, body string) *httptest.ResponseRecorder {
	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/api/emoji", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	_ = h.Emoji(c)
	return rec
}

func TestEmoji_MissingName(t *testing.T) {
	h, _ := setup()
	rec := doPostEmoji(h, `{}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestEmoji_NotFound(t *testing.T) {
	h, _ := setup()
	rec := doPostEmoji(h, `{"name":"nonexist"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	errObj := body["error"].(map[string]any)
	assert.Equal(t, "NO_SUCH_EMOJI", errObj["code"])
}

func TestEmoji_Found(t *testing.T) {
	h, repo := setup()
	cat := "faces"
	repo.Emojis["smile@"] = &model.Emoji{
		ID:        "e1",
		Name:      "smile",
		Category:  &cat,
		PublicURL: "https://example.com/emoji/smile.png",
		Aliases:   []string{"happy"},
	}
	rec := doPostEmoji(h, `{"name":"smile"}`)
	assert.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "e1", body["id"])
	assert.Equal(t, "smile", body["name"])
	assert.Equal(t, "faces", body["category"])
	assert.Equal(t, "https://example.com/emoji/smile.png", body["url"])
}
