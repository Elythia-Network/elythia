package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	apiadmin "github.com/shiroha-a/mk/internal/api/admin"
	"github.com/shiroha-a/mk/internal/core/emojimeta"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRemoteMetaFetcher records what it was asked for and returns a canned result.
type fakeRemoteMetaFetcher struct {
	gotHost     string
	gotName     string
	gotSoftware string
	meta        *emojimeta.Meta
	err         error
	calls       int
}

func (f *fakeRemoteMetaFetcher) Fetch(_ context.Context, host, name, software string) (*emojimeta.Meta, error) {
	f.calls++
	f.gotHost, f.gotName, f.gotSoftware = host, name, software
	return f.meta, f.err
}

func strp(s string) *string { return &s }
func boolp(b bool) *bool    { return &b }

// 取得できた項目だけを返す (#2698)。
func TestEmojiFetchRemoteMeta_Success(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "kawaii", Host: &host})
	f := &fakeRemoteMetaFetcher{meta: &emojimeta.Meta{
		Category:    strp("cat"),
		Aliases:     []string{"a", "b"},
		License:     strp("CC0"),
		IsSensitive: boolp(true),
	}}
	h.SetRemoteEmojiMetaFetcher(f)

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["fetched"])
	assert.Equal(t, "cat", body["category"])
	assert.Equal(t, "CC0", body["license"])
	assert.Equal(t, true, body["isSensitive"])
	assert.Equal(t, []any{"a", "b"}, body["aliases"])

	// 取得先は絵文字の host / name から決まる。
	assert.Equal(t, "remote.example", f.gotHost)
	assert.Equal(t, "kawaii", f.gotName)
}

// **取れなかった項目はキーごと出さない。** 空文字を返すと frontend が
// 「相手が空を返した」と解釈して既存値を消す。
func TestEmojiFetchRemoteMeta_OmitsMissingFields(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{meta: &emojimeta.Meta{Category: strp("only")}})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "only", body["category"])
	assert.NotContains(t, body, "license", "取れなかった項目のキーは出さない")
	assert.NotContains(t, body, "aliases")
	assert.NotContains(t, body, "isSensitive")
}

// **取得失敗を 5xx にしない。** 相手が per-name endpoint を持たないのは正常な
// 結果で、UI は手入力に倒す。
func TestEmojiFetchRemoteMeta_UnsupportedIsNotError(t *testing.T) {
	host := "mastodon.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{err: emojimeta.ErrUnsupported})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["fetched"])
	assert.Equal(t, "unsupported", body["reason"])
}

func TestEmojiFetchRemoteMeta_NotFoundOnOrigin(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{err: emojimeta.ErrNotFound})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["fetched"])
	assert.Equal(t, "notFound", body["reason"])
}

// 相手が落ちている類の失敗も 200 で返すが、理由は "error" にする。
func TestEmojiFetchRemoteMeta_TransportErrorIsNotError(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{err: errors.New("dial tcp: refused")})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["fetched"])
	assert.Equal(t, "error", body["reason"])
}

// ローカル絵文字には取ってくる相手がいない。**fetcher を呼ばない**こと。
func TestEmojiFetchRemoteMeta_LocalEmojiIsRejected(t *testing.T) {
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: nil})
	f := &fakeRemoteMetaFetcher{meta: &emojimeta.Meta{}}
	h.SetRemoteEmojiMetaFetcher(f)

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NOT_REMOTE_EMOJI")
	assert.Zero(t, f.calls, "ローカル絵文字で外向き通信を起こしてはいけない")
}

func TestEmojiFetchRemoteMeta_NoSuchEmoji(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"missing"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "NO_SUCH_EMOJI")
}

func TestEmojiFetchRemoteMeta_RequiresEmojiID(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	rec := doPost(h.EmojiFetchRemoteMeta, `{}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// fetcher 未配線でも 5xx にしない (graceful degradation)。
func TestEmojiFetchRemoteMeta_UnwiredFetcher(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["fetched"])
	// **id は返す。** 無いと frontend がモーダルを描けず取り込めない。
	assert.Equal(t, "e1", body["emojiId"])
	assert.Equal(t, "remote.example", body["host"])
}

var _ apiadmin.RemoteEmojiMetaFetcher = (*fakeRemoteMetaFetcher)(nil)

// 相手の API から取れなかったときは、連合で入っているライセンスを返す (#3246)。
// 返さないと、ダイアログが空で初期化されて copy が元のライセンスを空で上書きする。
func TestEmojiFetchRemoteMeta_FallsBackToStoredLicense(t *testing.T) {
	cases := []struct {
		name    string
		fetcher *fakeRemoteMetaFetcher
	}{
		{"origin has no license", &fakeRemoteMetaFetcher{meta: &emojimeta.Meta{Category: strp("c")}}},
		{"unsupported", &fakeRemoteMetaFetcher{err: emojimeta.ErrUnsupported}},
		{"not found", &fakeRemoteMetaFetcher{err: emojimeta.ErrNotFound}},
		{"transport error", &fakeRemoteMetaFetcher{err: errors.New("dial tcp: refused")}},
		{"unwired", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host := "remote.example"
			h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host, License: strp("COGNOSPHERE")})
			if tc.fetcher != nil {
				h.SetRemoteEmojiMetaFetcher(tc.fetcher)
			}
			rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
			require.Equal(t, http.StatusOK, rec.Code)
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			assert.Equal(t, "COGNOSPHERE", body["license"])
		})
	}
}

// 相手の API の値があればそちらを優先する。保存値が空ならキーを出さない。
func TestEmojiFetchRemoteMeta_OriginLicenseWins(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host, License: strp("old")})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{meta: &emojimeta.Meta{License: strp("new")}})
	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "new", body["license"])

	h, _ = setupEmojiHandler(t, &model.Emoji{ID: "e2", Name: "y", Host: &host, License: strp("")})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{err: emojimeta.ErrUnsupported})
	rec = doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e2"}`, adminUser)
	body = map[string]any{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotContains(t, body, "license")
}

// admin/emoji/copy の上書きパラメータ (#2698)。取得・編集した値でコピーする。
func TestEmojiCopy_OverridesFromRemoteMeta(t *testing.T) {
	host := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "x", Host: &host,
		Category: strp("old"), License: strp("oldlic"),
	})

	rec := doPost(h.EmojiCopy,
		`{"emojiId":"e1","category":"new","aliases":["a1","a2"],"license":"CC0","isSensitive":true}`,
		adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))

	require.NotNil(t, copied.Category)
	assert.Equal(t, "new", *copied.Category)
	assert.Equal(t, []string{"a1", "a2"}, []string(copied.Aliases))
	require.NotNil(t, copied.License)
	assert.Equal(t, "CC0", *copied.License)
	assert.True(t, copied.IsSensitive)
}

// **指定しなかった項目は src の値を保つ。** upstream の呼び出し (emojiId のみ) が
// そのまま通り続けることの担保でもある。
func TestEmojiCopy_WithoutOverridesKeepsSource(t *testing.T) {
	host := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "x", Host: &host,
		Category: strp("keep"), License: strp("keeplic"),
		Aliases: model.StringArray{"orig"}, IsSensitive: true,
	})

	rec := doPost(h.EmojiCopy, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))

	require.NotNil(t, copied.Category)
	assert.Equal(t, "keep", *copied.Category)
	require.NotNil(t, copied.License)
	assert.Equal(t, "keeplic", *copied.License)
	assert.Equal(t, []string{"orig"}, []string(copied.Aliases))
	assert.True(t, copied.IsSensitive)
}

// **「指定なし」と「空を指定」を区別する。** 空配列や空文字を渡したら消す。
func TestEmojiCopy_ExplicitEmptyClearsValue(t *testing.T) {
	host := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{
		ID: "e1", Name: "x", Host: &host,
		Category: strp("old"), Aliases: model.StringArray{"orig"}, IsSensitive: true,
	})

	rec := doPost(h.EmojiCopy, `{"emojiId":"e1","category":"","aliases":[],"isSensitive":false}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	copied := findEmojiByID(t, repo, body["id"].(string))

	require.NotNil(t, copied.Category)
	assert.Equal(t, "", *copied.Category, "空文字の指定は空にする")
	assert.Empty(t, copied.Aliases)
	assert.False(t, copied.IsSensitive)
}

// **name+host でも引ける** (#2698)。右クリックから呼ぶとき frontend は
// `name@host` しか持っておらず、id を知らない。
func TestEmojiFetchRemoteMeta_ByNameAndHost(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "kawaii", Host: &host, OriginalURL: "https://x/e.png"})
	f := &fakeRemoteMetaFetcher{meta: &emojimeta.Meta{Category: strp("c")}}
	h.SetRemoteEmojiMetaFetcher(f)

	rec := doPost(h.EmojiFetchRemoteMeta, `{"name":"kawaii","host":"remote.example"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, true, body["fetched"])
	// copy に渡す id と、モーダルが描画に使う情報が揃っていること。
	assert.Equal(t, "e1", body["emojiId"])
	assert.Equal(t, "kawaii", body["name"])
	assert.Equal(t, "remote.example", body["host"])
	assert.Equal(t, "https://x/e.png", body["originalUrl"])
}

// 取得に失敗しても id は返す。**手で埋めて取り込む導線を残す**ため。
func TestEmojiFetchRemoteMeta_FailureStillReturnsID(t *testing.T) {
	host := "mastodon.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e9", Name: "x", Host: &host, OriginalURL: "https://y/e.png"})
	h.SetRemoteEmojiMetaFetcher(&fakeRemoteMetaFetcher{err: emojimeta.ErrUnsupported})

	rec := doPost(h.EmojiFetchRemoteMeta, `{"name":"x","host":"mastodon.example"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, false, body["fetched"])
	assert.Equal(t, "e9", body["emojiId"], "取得に失敗しても取り込みは続けられること")
	assert.Equal(t, "https://y/e.png", body["originalUrl"])
}

// name だけ / host だけでは引けない。
func TestEmojiFetchRemoteMeta_RequiresBothNameAndHost(t *testing.T) {
	h, _ := setupEmojiHandler(t)
	for _, body := range []string{`{"name":"x"}`, `{"host":"h"}`, `{}`} {
		rec := doPost(h.EmojiFetchRemoteMeta, body, adminUser)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", body)
	}
}

// **列に収まらない値で 500 にしない** (#2698)。値の出どころは相手サーバーの
// `/api/emoji` なので、長さは相手が決める。AP 経路が同じ 3 列に対して持っている
// 規則 (#2726) と揃える。
func TestEmojiCopy_ClampsOversizedFields(t *testing.T) {
	host := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})

	long129 := strings.Repeat("あ", 129)   // category / alias は varchar(128)
	long1025 := strings.Repeat("い", 1025) // license は varchar(1024)
	body, err := json.Marshal(map[string]any{
		"emojiId":  "e1",
		"category": long129,
		"aliases":  []string{"ok", long129, ""},
		"license":  long1025,
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiCopy, string(body), adminUser)
	require.Equal(t, http.StatusOK, rec.Code, "列超過で 500 になってはいけない")

	var res map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	copied := findEmojiByID(t, repo, res["id"].(string))

	// 本文は切る。
	require.NotNil(t, copied.Category)
	assert.Equal(t, 128, len([]rune(*copied.Category)))
	require.NotNil(t, copied.License)
	assert.Equal(t, 1024, len([]rune(*copied.License)))

	// **alias は切らずに落とす。** 切ると別の名前になり、リアクションの照合に
	// 使えないものが混ざる。空文字も落とす。
	assert.Equal(t, []string{"ok"}, []string(copied.Aliases))
}

// NUL は長さに関わらず SQLSTATE 22021 で弾かれるので落とす。
func TestEmojiCopy_StripsNUL(t *testing.T) {
	host := "remote.example"
	h, repo := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})

	body, err := json.Marshal(map[string]any{
		"emojiId":  "e1",
		"category": "a\x00b",
		"aliases":  []string{"c\x00d"},
		"license":  "e\x00f",
	})
	require.NoError(t, err)

	rec := doPost(h.EmojiCopy, string(body), adminUser)
	require.Equal(t, http.StatusOK, rec.Code)

	var res map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
	copied := findEmojiByID(t, repo, res["id"].(string))

	require.NotNil(t, copied.Category)
	assert.Equal(t, "ab", *copied.Category)
	require.NotNil(t, copied.License)
	assert.Equal(t, "ef", *copied.License)
	assert.Equal(t, []string{"cd"}, []string(copied.Aliases))
}

// **software の解決経路を固定する** (#2698)。`instanceRepo` を引かないと
// software が常に空になり、fetcher が常に ErrUnsupported を返す = 機能が
// 丸ごと止まる。
func TestEmojiFetchRemoteMeta_PassesSoftwareName(t *testing.T) {
	host := "remote.example"
	h, _ := setupEmojiHandler(t, &model.Emoji{ID: "e1", Name: "x", Host: &host})
	f := &fakeRemoteMetaFetcher{meta: &emojimeta.Meta{}}
	h.SetRemoteEmojiMetaFetcher(f)

	sw := "misskey"
	instRepo := testutil.NewMockInstanceRepository()
	require.NoError(t, instRepo.Create(&model.Instance{Host: host, SoftwareName: &sw}))
	h.SetInstanceRepo(instRepo)

	rec := doPost(h.EmojiFetchRemoteMeta, `{"emojiId":"e1"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "misskey", f.gotSoftware, "instance.softwareName を fetcher に渡していない")
}
