package i

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// カスタム絵文字をアバターデコレーションとして使う (#2975) の検証。

// failingEmojiRepo makes FindByNameAndHost fail so the "DB 障害を not-found に
// 丸めない" (#2792) 経路を踏める。
type failingEmojiRepo struct {
	*testutil.MockEmojiRepository
	findErr error
}

func (f *failingEmojiRepo) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.MockEmojiRepository.FindByNameAndHost(name, host)
}

func (f *failingEmojiRepo) FindByID(id string) (*model.Emoji, error) {
	if f.findErr != nil {
		return nil, f.findErr
	}
	return f.MockEmojiRepository.FindByID(id)
}

// emojiDecoFixture builds a handler with one local emoji ready to be worn.
func emojiDecoFixture(t *testing.T, policies map[string]any) (*Handler, *testutil.MockUserRepository, *testutil.MockEmojiRepository, *model.User) {
	t.Helper()
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{
		ID:                "user1",
		Username:          "user1",
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}

	emojis := testutil.NewMockEmojiRepository()
	emojis.Emojis["party@"] = &model.Emoji{ID: "e1", Name: "party", PublicURL: "https://e/party.webp"}
	h.SetEmojiRepo(emojis)
	h.SetAvatarDecorationRepo(testutil.NewMockAvatarDecorationRepository())
	h.SetRoleProvider(&stubRoleProvider{policies: policies})
	return h, repo, emojis, user
}

func defaultEmojiDecoPolicies() map[string]any {
	return map[string]any{"avatarDecorationLimit": 1, "canUseEmojiAsAvatarDecoration": true}
}

func TestUpdate_EmojiDecoration_Attaches(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party","angle":0.25,"flipH":true}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"party","angle":0.25,"flipH":true,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **センシティブな絵文字は設定できない。** 表示側 (EmojiResolver) でも落ちるが、
// 設定時に弾かないと「保存はできたのに出ない」になる。
func TestUpdate_EmojiDecoration_RejectsSensitive(t *testing.T) {
	h, repo, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	emojis.Emojis["nsfw@"] = &model.Emoji{ID: "e2", Name: "nsfw", PublicURL: "u", IsSensitive: true}

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"nsfw"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SENSITIVE_EMOJI_NOT_ALLOWED")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

func TestUpdate_EmojiDecoration_RejectsUnknownName(t *testing.T) {
	h, _, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"nope"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_EMOJI")
}

// リモート絵文字は FindByNameAndHost(name, nil) の対象外なので届かない。
func TestUpdate_EmojiDecoration_RejectsRemoteEmoji(t *testing.T) {
	h, _, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	host := "other.example"
	emojis.Emojis["remote@other.example"] = &model.Emoji{ID: "e3", Name: "remote", PublicURL: "u", Host: &host}

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"remote"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_EMOJI")
}

// **ロールで使用可否を制御できる。**
func TestUpdate_EmojiDecoration_RestrictedByRole(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         1,
		"canUseEmojiAsAvatarDecoration": false,
	})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

// policy が false でも catalog 由来はそのまま通る (絞っているのは絵文字だけ)。
func TestUpdate_EmojiDecoration_PolicyDoesNotBlockCatalog(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         1,
		"canUseEmojiAsAvatarDecoration": false,
	})
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	h.SetAvatarDecorationRepo(deco)

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **個数は catalog 由来と合算する。** 別枠にすると「1 つまで」と言いながら
// 合計 2 つ付く。
func TestUpdate_EmojiDecoration_CountsTowardSharedLimit(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	h.SetAvatarDecorationRepo(deco)

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"},{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_AVATAR_DECORATIONS")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

// limit を上げれば混在して装着できる。
func TestUpdate_EmojiDecoration_MixedWithinLimit(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         2,
		"canUseEmojiAsAvatarDecoration": true,
	})
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	h.SetAvatarDecorationRepo(deco)

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1"},{"emojiName":"party"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0},`+
			`{"id":"e1","emojiName":"party","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **emojiName があれば id は読まない。** クライアントは配列を丸ごと送り直すので、
// 装着済みの行から来た id が一緒に返ってくる。catalog を引きに行くと必ず落ちる。
func TestUpdate_EmojiDecoration_IgnoresIDWhenEmojiNameGiven(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"id":"not-a-decoration","emojiName":"party"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"party","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// 空文字列の emojiName は catalog 由来として扱う (判別子は「非空」)。
func TestUpdate_EmojiDecoration_EmptyEmojiNameFallsBackToCatalog(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	h.SetAvatarDecorationRepo(deco)

	rec := post(h.Update, `{"avatarDecorations":[{"id":"dec1","emojiName":""}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **DB 障害を「そんな絵文字は無い」にしない** (#2792)。
func TestUpdate_EmojiDecoration_DBFailureIsInternalError(t *testing.T) {
	h, _, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	h.SetEmojiRepo(&failingEmojiRepo{MockEmojiRepository: emojis, findErr: errors.New("db down")})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "NO_SUCH_EMOJI")
}

// 絵文字を引けない構成でこの経路に入るのは配線の誤り。検証を skip して保存すると
// 表示側で必ず drop される行だけが残る。
func TestUpdate_EmojiDecoration_UnwiredRepoIsInternalError(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}
	h.SetRoleProvider(&stubRoleProvider{policies: defaultEmojiDecoPolicies()})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

// **roleProvider が配線されているのにキーが無ければ拒否する (fail-closed)。**
// 本番の `GetUserPolicies` は `DefaultPoliciesClone()` を起点にするのでキーは
// 必ず入る。欠けているのは policy を解決できていない状態なので、既定 true を
// 当てにしない。
func TestUpdate_EmojiDecoration_MissingPolicyKeyDenies(t *testing.T) {
	h, _, _, user := emojiDecoFixture(t, map[string]any{"avatarDecorationLimit": 1})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
}

// roleProvider そのものが未配線なら既定 (true) を使う。catalog / role 検証が
// 同じ fallback を採っているのに合わせてある。未配線で制限が外れることは
// `i.roleProvider` の criticalWiring entry に書いてある。
func TestUpdate_EmojiDecoration_AllowedWhenRoleProviderUnwired(t *testing.T) {
	h, repo, _, _ := newTestHandler(t)
	user := &model.User{ID: "user1", Username: "user1", AvatarDecorations: datatypes.JSON([]byte("[]"))}
	repo.Users["user1"] = user
	repo.Profiles["user1"] = &model.UserProfile{UserID: "user1", Fields: datatypes.JSON([]byte("[]"))}
	emojis := testutil.NewMockEmojiRepository()
	emojis.Emojis["party@"] = &model.Emoji{ID: "e1", Name: "party", PublicURL: "u"}
	h.SetEmojiRepo(emojis)

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// stubbornEmojiRepo は host を無視して行を返す。**`FindByNameAndHost(name, nil)`
// は host IS NULL で引くので通常ここには来ない**が、述語 (EmojiUsableAsDecoration)
// を 1 箇所に閉じてあることを固定する — lookup の条件が将来広がっても、
// リモート絵文字は装着できないままであること。
type stubbornEmojiRepo struct {
	*testutil.MockEmojiRepository
	row *model.Emoji
}

func (s *stubbornEmojiRepo) FindByNameAndHost(string, *string) (*model.Emoji, error) {
	return s.row, nil
}

func (s *stubbornEmojiRepo) FindByID(string) (*model.Emoji, error) {
	return s.row, nil
}

func TestUpdate_EmojiDecoration_RefusesRemoteEvenIfLookupReturnsIt(t *testing.T) {
	h, repo, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	host := "other.example"
	h.SetEmojiRepo(&stubbornEmojiRepo{
		MockEmojiRepository: emojis,
		row:                 &model.Emoji{ID: "e9", Name: "remote", PublicURL: "u", Host: &host},
	})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"remote"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_EMOJI")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

// **保存するのは DB 上の名前で、入力の写しではない。** `FindByNameAndHost` は
// 完全一致なので今日は同じ値になるが、写しを保存する実装だと lookup の条件が
// 緩んだ瞬間に食い違う。
func TestUpdate_EmojiDecoration_StoresDBNameNotInput(t *testing.T) {
	h, repo, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	h.SetEmojiRepo(&stubbornEmojiRepo{
		MockEmojiRepository: emojis,
		row:                 &model.Emoji{ID: "e1", Name: "canonical", PublicURL: "u"},
	})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"WhateverTheClientSent"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"canonical","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **policy が bool でなければ拒否する (fail-closed)。**
// `admin/roles/update-default-policies` は値を型検証せず meta.policies へ書き、
// `coerceToBaseType` に bool の枝が無いので文字列がそのまま届きうる。
func TestUpdate_EmojiDecoration_NonBoolPolicyDenies(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         1,
		"canUseEmojiAsAvatarDecoration": "true",
	})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

// **policy を外すと、装着済みの絵文字を含む更新も通らなくなる。**
// これは catalog 由来の `roleIdsThatCanBeUsedThisDecoration` と同じ挙動で
// (upstream `i/update.ts` も配列の全要素を検証する)、絵文字に限った話では
// ない。利用者はその絵文字を外せば他の装飾を編集できる。
func TestUpdate_EmojiDecoration_PolicyOffBlocksArrayContainingWornEmoji(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         2,
		"canUseEmojiAsAvatarDecoration": false,
	})
	deco := testutil.NewMockAvatarDecorationRepository()
	deco.Decorations["dec1"] = &model.AvatarDecoration{ID: "dec1"}
	h.SetAvatarDecorationRepo(deco)

	rec := post(h.Update,
		`{"avatarDecorations":[{"id":"e1","emojiName":"party"},{"id":"dec1"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")

	// 絵文字を外せば通る (= 詰みではない)。
	rec = post(h.Update, `{"avatarDecorations":[{"id":"dec1"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t, `[{"id":"dec1","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// --- emojiName を落とすクライアント向けの id fallback ---

// **型付きクライアントは未知の field を落とす。** 装着済みの絵文字エントリが
// `{id, angle, ...}` で戻ってくるので、catalog に無い id をそのまま弾くと
// 絵文字を 1 つ着けた時点で装飾を一切編集できなくなる。
func TestUpdate_EmojiDecoration_ResolvesByIDWhenEmojiNameDropped(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"id":"e1","angle":0.5}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"party","angle":0.5,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **fallback でも制限は素通しにしない。** `emojiName` を落とすだけで policy を
// 迂回できてはいけない。
func TestUpdate_EmojiDecoration_IDFallbackStillChecksPolicy(t *testing.T) {
	h, _, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         1,
		"canUseEmojiAsAvatarDecoration": false,
	})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"e1"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
}

func TestUpdate_EmojiDecoration_IDFallbackStillChecksSensitive(t *testing.T) {
	h, _, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	emojis.Emojis["nsfw@"] = &model.Emoji{ID: "e2", Name: "nsfw", PublicURL: "u", IsSensitive: true}

	rec := post(h.Update, `{"avatarDecorations":[{"id":"e2"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "SENSITIVE_EMOJI_NOT_ALLOWED")
}

// catalog にも emoji にも無い id は従来どおり NO_SUCH_AVATAR_DECORATION。
func TestUpdate_EmojiDecoration_UnknownIDStillNoSuchAvatarDecoration(t *testing.T) {
	h, _, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"id":"nope"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_AVATAR_DECORATION")
}

// **fallback の DB 障害も not-found に丸めない** (#2792)。
func TestUpdate_EmojiDecoration_IDFallbackDBFailureIsInternalError(t *testing.T) {
	h, _, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	h.SetEmojiRepo(&failingEmojiRepo{MockEmojiRepository: emojis, findErr: errors.New("db down")})

	rec := post(h.Update, `{"avatarDecorations":[{"id":"e1"}]}`, user)
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.NotContains(t, rec.Body.String(), "NO_SUCH_AVATAR_DECORATION")
}

// --- 絵文字側のロール制限 (#2975) ---

// `reaction_service.go` が roleIdsThatCanBeUsedThisEmojiAsReaction をリアクション
// で強制しているので、アイコンにだけ載せられると設定が片側だけ効く形になる。
func TestUpdate_EmojiDecoration_RestrictedEmojiNeedsRole(t *testing.T) {
	h, repo, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	emojis.Emojis["vip@"] = &model.Emoji{
		ID: "e5", Name: "vip", PublicURL: "u",
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"role-vip"},
	}

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"vip"}]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "RESTRICTED_BY_ROLE")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

func TestUpdate_EmojiDecoration_RestrictedEmojiAllowedWithRole(t *testing.T) {
	h, repo, emojis, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	emojis.Emojis["vip@"] = &model.Emoji{
		ID: "e5", Name: "vip", PublicURL: "u",
		RoleIDsThatCanBeUsedThisEmojiAsReaction: model.StringArray{"role-vip"},
	}
	h.SetRoleProvider(&stubRoleProvider{
		policies: defaultEmojiDecoPolicies(),
		roles:    []*model.Role{{ID: "role-vip"}},
	})

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"vip"}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e5","emojiName":"vip","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// --- 配置値の範囲 (upstream paramDef と同じ、#2975 で到達性が変わったので揃えた) ---

func TestUpdate_AvatarDecorations_RejectsOutOfRangePlacement(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"angle が上限超え", `{"avatarDecorations":[{"emojiName":"party","angle":0.6}]}`},
		{"angle が下限未満", `{"avatarDecorations":[{"emojiName":"party","angle":-0.6}]}`},
		{"offsetX が上限超え", `{"avatarDecorations":[{"emojiName":"party","offsetX":0.26}]}`},
		{"offsetY が下限未満", `{"avatarDecorations":[{"emojiName":"party","offsetY":-1000000}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
			rec := post(h.Update, tc.body, user)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
			assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
			assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
		})
	}
}

func TestUpdate_AvatarDecorations_AcceptsRangeBoundaries(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
	rec := post(h.Update,
		`{"avatarDecorations":[{"emojiName":"party","angle":0.5,"offsetX":-0.25,"offsetY":0.25}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"party","angle":0.5,"flipH":false,"offsetX":-0.25,"offsetY":0.25}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **`maxItems` は policy の上限とは別の層。** upstream は schema で 16 に
// 切っており、policy を 100 にしても 17 件目は通らない。
func TestUpdate_AvatarDecorations_RejectsMoreThanMaxItems(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, map[string]any{
		"avatarDecorationLimit":         100,
		"canUseEmojiAsAvatarDecoration": true,
	})
	entries := make([]string, 17)
	for i := range entries {
		entries[i] = `{"emojiName":"party"}`
	}
	rec := post(h.Update, `{"avatarDecorations":[`+strings.Join(entries, ",")+`]}`, user)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
}

// --- サイズ (mk-go 独自の scale、#2975) ---

func TestUpdate_AvatarDecorations_StoresScale(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party","scale":0.5}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"party","angle":0,"flipH":false,"offsetX":0,"offsetY":0,"scale":0.5}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **既定 (1) は保存しない。** 意味が変わらないのに全員の jsonb を書き換えない。
func TestUpdate_AvatarDecorations_OmitsDefaultScale(t *testing.T) {
	h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())

	rec := post(h.Update, `{"avatarDecorations":[{"emojiName":"party","scale":1}]}`, user)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.JSONEq(t,
		`[{"id":"e1","emojiName":"party","angle":0,"flipH":false,"offsetX":0,"offsetY":0}]`,
		string(repo.Users["user1"].AvatarDecorations))
}

// **拡大は許さない。** `.decoration` は既にアバターの 2 倍の枠なので、1 を
// 超えるとアイコンの外へはみ出して周囲の UI を覆える。
func TestUpdate_AvatarDecorations_RejectsOutOfRangeScale(t *testing.T) {
	for _, body := range []string{
		`{"avatarDecorations":[{"emojiName":"party","scale":1.01}]}`,
		`{"avatarDecorations":[{"emojiName":"party","scale":100}]}`,
		`{"avatarDecorations":[{"emojiName":"party","scale":0.05}]}`,
		`{"avatarDecorations":[{"emojiName":"party","scale":-1}]}`,
	} {
		h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
		rec := post(h.Update, body, user)
		assert.Equal(t, http.StatusBadRequest, rec.Code, body)
		assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
		assert.JSONEq(t, `[]`, string(repo.Users["user1"].AvatarDecorations))
	}
}

func TestUpdate_AvatarDecorations_AcceptsScaleBoundaries(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"avatarDecorations":[{"emojiName":"party","scale":0.1}]}`, `"scale":0.1`},
		{`{"avatarDecorations":[{"emojiName":"party","scale":1.0}]}`, ``},
	} {
		h, repo, _, user := emojiDecoFixture(t, defaultEmojiDecoPolicies())
		rec := post(h.Update, tc.body, user)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		stored := string(repo.Users["user1"].AvatarDecorations)
		if tc.want == "" {
			assert.NotContains(t, stored, "scale")
		} else {
			assert.Contains(t, stored, tc.want)
		}
	}
}
