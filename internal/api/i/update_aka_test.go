package i

import (
	"encoding/json"
	"net/http"
	"strings"
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

// Local alias targets must be externally available before they can be saved.
func TestUpdate_AlsoKnownAs_LocalFederationAvailability(t *testing.T) {
	for _, status := range []model.FederationStatus{model.FederationStatusOff, model.FederationStatusPending} {
		t.Run(string(status), func(t *testing.T) {
			h, me := akaHandler(t)
			nativeToken := "native-login-token"
			me.Token = &nativeToken
			me.FederationStatus = model.FederationStatusOn
			me.AlsoKnownAs = strPtr("https://mk.example/users/old")
			target := repoOf(h).addLocal("alias1", "aliasuser")
			target.FederationStatus = status
			h.SetLocalFederationObserver(&moveAvailabilityObserver{})

			rec := updateWithToken(h, "{\"alsoKnownAs\":[\"@aliasuser\"]}", me, nativeToken)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "NO_SUCH_USER")
			got := repoOf(h).get("me")
			require.NotNil(t, got.AlsoKnownAs)
			assert.Equal(t, "https://mk.example/users/old", *got.AlsoKnownAs)
		})
	}
}

func TestUpdate_AlsoKnownAs_LocalFederationEnabled(t *testing.T) {
	h, me := akaHandler(t)
	nativeToken := "native-login-token"
	me.Token = &nativeToken
	me.FederationStatus = model.FederationStatusOn
	target := repoOf(h).addLocal("alias1", "aliasuser")
	target.FederationStatus = model.FederationStatusOn
	h.SetLocalFederationObserver(&moveAvailabilityObserver{})

	rec := updateWithToken(h, "{\"alsoKnownAs\":[\"@aliasuser\"]}", me, nativeToken)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := repoOf(h).get("me")
	require.NotNil(t, got.AlsoKnownAs)
	assert.Equal(t, "https://mk.example/users/alias1", *got.AlsoKnownAs)
}

// #2106 L3: 不正な regex リテラルの mutedWords は INVALID_REGEXP(400) で拒否する。
func TestUpdate_InvalidMutedWordsRegex(t *testing.T) {
	h, me := akaHandler(t)
	rec := post(h.Update, `{"mutedWords":["/[/"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_REGEXP")
}

// hardMutedWords は正規表現の大きさの上限を超えたら INVALID_REGEXP(400)。
// サーバーがノートごとに評価するので、巨大なパターン 1 件で CPU と配信が止まる。
// 同じパターンでも mutedWords (クライアントだけが評価する) は受け付ける。
func TestUpdate_HardMutedWordsRegexTooComplex(t *testing.T) {
	h, me := akaHandler(t)
	big := "/" + strings.Repeat(".?", 400) + "b/"
	body, err := json.Marshal(map[string]any{"hardMutedWords": []string{big}})
	require.NoError(t, err)
	rec := post(h.Update, string(body), me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_REGEXP")

	body, err = json.Marshal(map[string]any{"mutedWords": []string{big}})
	require.NoError(t, err)
	rec = post(h.Update, string(body), me)
	assert.Equal(t, http.StatusOK, rec.Code, "mutedWords には大きさの上限を掛けない: %s", rec.Body.String())

	body, err = json.Marshal(map[string]any{"hardMutedWords": []string{"/(?i)spam|scam/"}})
	require.NoError(t, err)
	rec = post(h.Update, string(body), me)
	assert.Equal(t, http.StatusOK, rec.Code, "普通の正規表現は通ること: %s", rec.Body.String())
}

// 負の wordMuteLimit は upstream と同じく、ミュートする語を 1 つも置けないこと
// (`count > limit` が常に真)。上限なしにしない。
func TestUpdate_NegativeWordMuteLimitDenies(t *testing.T) {
	h, me := akaHandler(t)
	h.roleProvider = &stubRoleProvider{policies: map[string]any{"wordMuteLimit": -1}}
	rec := post(h.Update, `{"hardMutedWords":["spam"]}`, me)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_MUTED_WORDS")
}

// 件数の検査は正規表現の検査より先に行うこと (upstream と同じ順)。件数で断る
// 入力の正規表現をコンパイルさせない。
func TestUpdate_MuteWordCountCheckedBeforeRegex(t *testing.T) {
	h, me := akaHandler(t)
	h.roleProvider = &stubRoleProvider{policies: map[string]any{"wordMuteLimit": 10}}
	for _, key := range []string{"mutedWords", "hardMutedWords"} {
		// 11 件 (上限 10 を超える) の、どれも正規表現として読めない語。
		words := make([]string, 11)
		for i := range words {
			words[i] = "/(unclosed/"
		}
		body, err := json.Marshal(map[string]any{key: words})
		require.NoError(t, err)
		rec := post(h.Update, string(body), me)
		assert.Equal(t, http.StatusBadRequest, rec.Code, key)
		assert.Contains(t, rec.Body.String(), "TOO_MANY_MUTED_WORDS", key)
	}
}

// wordMuteLimit は upstream と同じく件数ではなく文字数 (UTF-16 の単位) の合計で
// 数えること。AND の組の中の語も足す。
func TestUpdate_WordMuteLimitCountsCharacters(t *testing.T) {
	h, me := akaHandler(t)
	h.roleProvider = &stubRoleProvider{policies: map[string]any{"wordMuteLimit": 10}}
	for body, want := range map[string]int{
		`{"hardMutedWords":[["abcdefghijk"]]}`:        http.StatusBadRequest, // 1 件でも 11 文字
		`{"hardMutedWords":[["abcde"],["fgh","ij"]]}`: http.StatusOK,         // 10 文字
		`{"hardMutedWords":["/abc/",["fgh","ij"]]}`:   http.StatusOK,         // 正規表現も文字数で数える (/ を含め 5 + 5)
		`{"mutedWords":[["abcde"],["fgh","ijk"]]}`:    http.StatusBadRequest, // 11 文字
		`{"mutedWords":[["😀😀😀😀😀"]]}`:                  http.StatusOK,         // サロゲートで 10
		`{"mutedWords":[["😀😀😀😀😀a"]]}`:                 http.StatusBadRequest, // 11
	} {
		rec := post(h.Update, body, me)
		assert.Equal(t, want, rec.Code, "%s: %s", body, rec.Body.String())
		if want == http.StatusBadRequest {
			assert.Contains(t, rec.Body.String(), "TOO_MANY_MUTED_WORDS", body)
		}
	}
}
