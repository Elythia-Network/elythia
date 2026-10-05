package i

import (
	"net/http"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strPtr(s string) *string { return &s }

// akaHandler wires an i.Handler whose Update resolution path (userRepo +
// serverURL) is ready, plus the self user row so UpdateProfile succeeds.
func akaHandler(t *testing.T) (*Handler, *model.User) {
	t.Helper()
	h, repo, _, _ := newTestHandler(t)
	h.SetUserRepo(repo)
	h.SetServerURL("https://mk.example")
	me := &model.User{ID: "me", Username: "me", UsernameLower: "me"}
	repo.Users["me"] = me
	return h, me
}

// repoOf reaches the mock user repo backing the handler so tests can register
// resolvable alias targets and assert the persisted alsoKnownAs csv.
func repoOf(h *Handler) *mockRepoAccessor { return &mockRepoAccessor{h: h} }

type mockRepoAccessor struct{ h *Handler }

func (a *mockRepoAccessor) addLocal(id, username string) *model.User {
	u := &model.User{ID: id, Username: username, UsernameLower: username}
	a.put(u)
	return u
}

func (a *mockRepoAccessor) put(u *model.User) {
	_ = a.h.userRepo.Create(u)
}

func (a *mockRepoAccessor) get(id string) *model.User {
	u, _ := a.h.userRepo.FindByID(id)
	return u
}

// 局所 acct (@user) を解決し canonical local URI を csv に保存する。
func TestUpdate_AlsoKnownAs_LocalAcctResolvesToURI(t *testing.T) {
	h, me := akaHandler(t)
	repoOf(h).addLocal("alias1", "aliasuser")

	rec := post(h.Update, `{"alsoKnownAs":["@aliasuser"]}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
	got := repoOf(h).get("me")
	require.NotNil(t, got.AlsoKnownAs)
	assert.Equal(t, "https://mk.example/users/alias1", *got.AlsoKnownAs)
}

// 入力が canonical URI (remote actor) でも FindByURI で解決して保存する。
func TestUpdate_AlsoKnownAs_URIResolves(t *testing.T) {
	h, me := akaHandler(t)
	remote := &model.User{ID: "rem1", Username: "remoteu", UsernameLower: "remoteu", Host: strPtr("other.example"), URI: strPtr("https://other.example/users/rem1")}
	repoOf(h).put(remote)

	rec := post(h.Update, `{"alsoKnownAs":["https://other.example/users/rem1"]}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
	got := repoOf(h).get("me")
	require.NotNil(t, got.AlsoKnownAs)
	assert.Equal(t, "https://other.example/users/rem1", *got.AlsoKnownAs)
}

// 複数 alias は csv 結合され、重複は除去される (uniqueItems 相当)。
func TestUpdate_AlsoKnownAs_DedupAndJoin(t *testing.T) {
	h, me := akaHandler(t)
	repoOf(h).addLocal("alias1", "aliasone")
	repoOf(h).addLocal("alias2", "aliastwo")

	rec := post(h.Update, `{"alsoKnownAs":["@aliasone","@aliastwo","@aliasone"]}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
	got := repoOf(h).get("me")
	require.NotNil(t, got.AlsoKnownAs)
	assert.Equal(t, "https://mk.example/users/alias1,https://mk.example/users/alias2", *got.AlsoKnownAs)
}

// 空配列はクリア (= NULL) として書き込まれる。
func TestUpdate_AlsoKnownAs_EmptyClears(t *testing.T) {
	h, me := akaHandler(t)
	// 事前に値を入れておく。
	me.AlsoKnownAs = strPtr("https://mk.example/users/old")

	rec := post(h.Update, `{"alsoKnownAs":[]}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
	got := repoOf(h).get("me")
	assert.Nil(t, got.AlsoKnownAs)
}

// 自分自身を alias に指定すると FORBIDDEN_TO_SET_YOURSELF。
func TestUpdate_AlsoKnownAs_Self(t *testing.T) {
	h, me := akaHandler(t)

	rec := post(h.Update, `{"alsoKnownAs":["@me"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "FORBIDDEN_TO_SET_YOURSELF")
	assert.Contains(t, rec.Body.String(), "25c90186-4ab0-49c8-9bba-a1fa6c202ba4")
}

// 解決できない acct は NO_SUCH_USER。
func TestUpdate_AlsoKnownAs_NoSuchUser(t *testing.T) {
	h, me := akaHandler(t)

	rec := post(h.Update, `{"alsoKnownAs":["@ghost"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
	assert.Contains(t, rec.Body.String(), "fcd2eef9-a9b2-4c4f-8624-038099e90aa5")
}

// 空文字 line も NO_SUCH_USER (upstream の `if (!line) throw noSuchUser`)。
func TestUpdate_AlsoKnownAs_EmptyLine(t *testing.T) {
	h, me := akaHandler(t)

	rec := post(h.Update, `{"alsoKnownAs":[""]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
}

// 解決した user の canonical URI が空 (remote だが uri 列が NULL) なら URI_NULL。
func TestUpdate_AlsoKnownAs_URINull(t *testing.T) {
	h, me := akaHandler(t)
	// remote user だが URI なし → canonicalUserURI が空 → URI_NULL。
	remote := &model.User{ID: "rem2", Username: "nouri", UsernameLower: "nouri", Host: strPtr("other.example")}
	repoOf(h).put(remote)

	rec := post(h.Update, `{"alsoKnownAs":["@nouri@other.example"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "URI_NULL")
	assert.Contains(t, rec.Body.String(), "bf326f31-d430-4f97-9933-5d61e4d48a23")
}

// 既に movedToUri 済なら alsoKnownAs を弾く (YOUR_ACCOUNT_MOVED, 403)。
func TestUpdate_AlsoKnownAs_AlreadyMoved(t *testing.T) {
	h, me := akaHandler(t)
	me.MovedToURI = strPtr("https://other.example/users/dest")
	repoOf(h).addLocal("alias1", "aliasuser")

	rec := post(h.Update, `{"alsoKnownAs":["@aliasuser"]}`, me)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "YOUR_ACCOUNT_MOVED")
	assert.Contains(t, rec.Body.String(), "56f20ec9-fd06-4fa5-841b-edd6d7d4fa31")
}

// maxItems 10 を超える要求は INVALID_PARAM。
func TestUpdate_AlsoKnownAs_TooMany(t *testing.T) {
	h, me := akaHandler(t)

	body := `{"alsoKnownAs":["@a","@b","@c","@d","@e","@f","@g","@h","@i","@j","@k"]}`
	rec := post(h.Update, body, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// alsoKnownAs 未指定 (省略) は何も書かない。
func TestUpdate_AlsoKnownAs_OmittedLeavesUnchanged(t *testing.T) {
	h, me := akaHandler(t)
	me.AlsoKnownAs = strPtr("https://mk.example/users/keep")

	rec := post(h.Update, `{"name":"x"}`, me)
	assert.Equal(t, http.StatusOK, rec.Code)
	got := repoOf(h).get("me")
	require.NotNil(t, got.AlsoKnownAs)
	assert.Equal(t, "https://mk.example/users/keep", *got.AlsoKnownAs)
}

// 解決不能な URI line は NO_SUCH_USER (FindByURI miss の fail-closed)。
func TestUpdate_AlsoKnownAs_UnknownURI(t *testing.T) {
	h, me := akaHandler(t)
	rec := post(h.Update, `{"alsoKnownAs":["https://ghost.example/users/zzz"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
}

// serverURL 未配線時はローカル alias の canonical URI を作れず URI_NULL に倒す
// (fail-closed: 空 URI を alsoKnownAs に書かない)。
func TestUpdate_AlsoKnownAs_LocalURINullWhenNoServerURL(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	h.SetUserRepo(repo)
	// serverURL を敢えて設定しない。
	me := &model.User{ID: "me", Username: "me", UsernameLower: "me"}
	repo.Users["me"] = me
	repo.Users["alias1"] = &model.User{ID: "alias1", Username: "aliasuser", UsernameLower: "aliasuser"}

	rec := post(h.Update, `{"alsoKnownAs":["@aliasuser"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "URI_NULL")
}

// userRepo 未配線時は alias を解決できず NO_SUCH_USER に倒す (fail-closed)。
func TestUpdate_AlsoKnownAs_NoUserRepoFailsClosed(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	// SetUserRepo を呼ばない → h.userRepo == nil。
	h.SetServerURL("https://mk.example")
	me := &model.User{ID: "me", Username: "me", UsernameLower: "me"}
	repo.Users["me"] = me

	rec := post(h.Update, `{"alsoKnownAs":["@anyone"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
}

// #2106 L3: 不正な regex リテラルの mutedWords は INVALID_REGEXP(400) で拒否する。
func TestUpdate_InvalidMutedWordsRegex(t *testing.T) {
	h, me := akaHandler(t)
	rec := post(h.Update, `{"mutedWords":["/[/"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_REGEXP")
}
