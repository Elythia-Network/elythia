package plugintest

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/plugin"
)

// **未登録のキーは分かるエラーにする。** typo で「呼んだつもり」になるのを
// 防ぐため、登録済みの一覧も添える。
func TestHandlers_Lookup(t *testing.T) {
	hs := Handlers{
		"POST /b": func(plugin.Request) (any, error) { return nil, nil },
		"GET /a":  func(plugin.Request) (any, error) { return nil, nil },
	}

	got, err := hs.lookup("GET /a")
	require.NoError(t, err)
	assert.NotNil(t, got)

	_, err = hs.lookup("GET /missing")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "GET /missing")
	// 候補が並びで出る (毎回順序が変わると読みにくい)。
	assert.Contains(t, err.Error(), "[GET /a POST /b]")
}

func TestJobSet_Lookup(t *testing.T) {
	j := &JobSet{Handlers: map[string]plugin.JobHandler{"tick": nil}}

	_, err := j.lookup("tick")
	require.NoError(t, err)

	_, err = j.lookup("nope")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nope")
}

// WithSecrets の失敗は、どの名前で失敗したかが分かるエラーにする。
func TestHarness_StoreSecretsReportsTheName(t *testing.T) {
	h := New(t).WithName("p")
	err := h.storeSecrets(map[string]string{"ok": "v", "bad name": "v"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"bad name"`)
}

// 置き場所は本番の repository と同じく、プラグイン名で分けて名前順に返す。
func TestMemSecretRepo_ListByPlugin(t *testing.T) {
	h := New(t).WithName("p").WithSecrets(map[string]string{"b": "1", "a": "2"})
	h.WithName("q").WithSecrets(map[string]string{"c": "3"})
	rows, err := h.secretRepo.ListByPlugin(context.Background(), "p")
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "a", rows[0].Name)
	assert.Equal(t, "b", rows[1].Name)
	sts, err := h.secrets.Statuses(context.Background(), "q")
	require.NoError(t, err)
	require.Len(t, sts, 1)
	assert.True(t, sts[0].Readable)
}

// Notify の前処理。本番が届けない宛先・未登録・型の無い通知はテストを落とす
// (#3469)。Fatal を通る経路は外からテストできないので、判定だけを分けて見る。
func TestHarness_PrepareNotify(t *testing.T) {
	h := New(t).WithName("bot")
	mine := h.SeedAccount("bot", "mine")
	theirs := h.SeedAccount("other", "theirs")
	frozen := h.SeedAccount("bot", "frozen")
	h.SuspendAccount(frozen.ID)
	gone := h.SeedAccount("bot", "gone")
	require.NoError(t, h.Context().Accounts().Delete(context.Background(), gone.ID))

	_, _, err := h.prepareNotify(plugin.Notification{Type: plugin.NotificationMention, AccountID: mine.ID})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Handle が登録されていません")

	h.Notifications(plugin.Definition{
		Name: "bot", APIVersion: plugin.APIVersion,
		Notifications: func(_ plugin.Context, n plugin.Notifications) error {
			n.Handle(func(context.Context, plugin.Notification) error { return nil })
			return nil
		},
	})

	for name, n := range map[string]plugin.Notification{
		"他のプラグインのアカウント": {Type: plugin.NotificationMention, AccountID: theirs.ID},
		"存在しないアカウント":    {Type: plugin.NotificationMention, AccountID: "ghost"},
		"凍結中":           {Type: plugin.NotificationMention, AccountID: frozen.ID},
		"削除済み":          {Type: plugin.NotificationMention, AccountID: gone.ID},
		"型が空":           {AccountID: mine.ID},
	} {
		_, _, err := h.prepareNotify(n)
		assert.Error(t, err, name)
	}

	fn, got, err := h.prepareNotify(plugin.Notification{Type: plugin.NotificationFollow, AccountID: mine.ID})
	require.NoError(t, err)
	assert.NotNil(t, fn)
	assert.NotEmpty(t, got.ID, "ID を省いたら連番を入れる")
	assert.False(t, got.CreatedAt.IsZero(), "CreatedAt を省いたら現在時刻を入れる")
	_, again, err := h.prepareNotify(plugin.Notification{Type: plugin.NotificationFollow, AccountID: mine.ID})
	require.NoError(t, err)
	assert.NotEqual(t, got.ID, again.ID, "連番は呼ぶたびに変わる")

	// WithAccounts で差し替えているときは宛先を確かめない。
	h.WithAccounts(h.Context().Accounts())
	_, _, err = h.prepareNotify(plugin.Notification{Type: plugin.NotificationMention, AccountID: "anything"})
	assert.NoError(t, err)
}
