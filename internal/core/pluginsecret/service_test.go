package pluginsecret_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/pluginsecret"
	"github.com/elythia-network/elythia/internal/core/pluginsecret/pgrepo"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/elythia-network/elythia/plugin"
)

// testKey is a fixed key for the DB tests (cipher_test.go の同名は内部の package)。
func testKey(b byte) []byte {
	return bytes.Repeat([]byte{b}, pluginsecret.KeySize)
}

func newOver(db *gorm.DB, key []byte) (*pluginsecret.Service, error) {
	return pluginsecret.New(pgrepo.New(repository.NewPluginSecretRepository(db)), key)
}

var (
	dbOnce sync.Once
	testDB *gorm.DB
)

// openDB returns this package's real PostgreSQL schema (CLAUDE.md Section 4).
// 暗号文が実際に DB に入る形を確かめるため、mock にしない。
func openDB(t *testing.T) *gorm.DB {
	t.Helper()
	dbOnce.Do(func() {
		testDB = testutil.MustOpenTestDB()
		testutil.ApplyMigrations(testDB)
	})
	return testDB
}

// newService builds a service over the real table and removes the plugin's
// rows when the test ends.
func newService(t *testing.T, key []byte, plugins ...string) (*pluginsecret.Service, *gorm.DB) {
	t.Helper()
	db := openDB(t)
	cleanup := func() {
		for _, p := range plugins {
			require.NoError(t, db.Exec(`DELETE FROM "plugin_secret" WHERE "pluginName" = ?`, p).Error)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	svc, err := newOver(db, key)
	require.NoError(t, err)
	return svc, db
}

func rawCiphertext(t *testing.T, db *gorm.DB, pluginName, name string) []byte {
	t.Helper()
	var rows []model.PluginSecret
	require.NoError(t, db.Where(`"pluginName" = ? AND "name" = ?`, pluginName, name).Find(&rows).Error)
	require.Len(t, rows, 1)
	return rows[0].SecretCiphertext
}

// **DB に置かれるのは暗号文で、鍵が無いと・別の鍵では読めない** (#3470 の完了条件)。
func TestService_DBHoldsCiphertextReadableOnlyWithTheKey(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t, testKey(1), "svc-at-rest")
	const value = "sk-live-0123456789abcdef"
	require.NoError(t, svc.Set(ctx, "svc-at-rest", "apiKey", value))

	stored := rawCiphertext(t, db, "svc-at-rest", "apiKey")
	assert.False(t, bytes.Contains(stored, []byte(value)), "DB に平文が入っている")
	assert.False(t, bytes.Contains(stored, []byte("0123456789abcdef")), "DB に値の一部が平文で入っている")

	got, err := svc.Get(ctx, "svc-at-rest", "apiKey")
	require.NoError(t, err)
	assert.Equal(t, value, got)

	other, err := newOver(db, testKey(2))
	require.NoError(t, err)
	_, err = other.Get(ctx, "svc-at-rest", "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretUnreadable, "別の鍵で読めてしまう")

	none, err := newOver(db, nil)
	require.NoError(t, err)
	_, err = none.Get(ctx, "svc-at-rest", "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretsUnavailable, "鍵が無いのに読めてしまう")
}

// 行の名前を書き換えても、別のプラグイン・別の名前の値としては読めない (AAD)。
func TestService_RowsAreBoundToPluginAndName(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t, testKey(1), "svc-aad-a", "svc-aad-b")
	require.NoError(t, svc.Set(ctx, "svc-aad-a", "apiKey", "value-of-a"))

	require.NoError(t, db.Exec(`UPDATE "plugin_secret" SET "pluginName" = 'svc-aad-b' WHERE "pluginName" = 'svc-aad-a'`).Error)
	_, err := svc.Get(ctx, "svc-aad-b", "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretUnreadable, "別のプラグインの値として読めてしまう")

	require.NoError(t, db.Exec(`UPDATE "plugin_secret" SET "pluginName" = 'svc-aad-a', "name" = 'token' WHERE "pluginName" = 'svc-aad-b'`).Error)
	_, err = svc.Get(ctx, "svc-aad-a", "token")
	assert.ErrorIs(t, err, plugin.ErrSecretUnreadable, "別の名前の値として読めてしまう")
}

// プラグインに渡す口は自分の値しか見えない。
func TestService_ForPluginIsScoped(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t, testKey(1), "svc-scope-a", "svc-scope-b")
	a := svc.ForPlugin("svc-scope-a")
	b := svc.ForPlugin("svc-scope-b")

	require.NoError(t, a.Set(ctx, "apiKey", "value-of-a"))
	_, err := b.Get(ctx, "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretNotSet, "B が A の値を読めてしまう")

	require.NoError(t, b.Set(ctx, "apiKey", "value-of-b"))
	got, err := a.Get(ctx, "apiKey")
	require.NoError(t, err)
	assert.Equal(t, "value-of-a", got, "B の書き込みが A の値を上書きした")

	require.NoError(t, b.Delete(ctx, "apiKey"))
	got, err = a.Get(ctx, "apiKey")
	require.NoError(t, err, "B の削除が A の値を消した")
	assert.Equal(t, "value-of-a", got)
	_, err = b.Get(ctx, "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretNotSet)
}

// **鍵が無ければ書き込みを断り、行も作らない。**
func TestService_WithoutKeyRefusesWrites(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t, nil, "svc-nokey")
	assert.False(t, svc.Available())

	assert.ErrorIs(t, svc.Set(ctx, "svc-nokey", "apiKey", "value"), plugin.ErrSecretsUnavailable)
	assert.ErrorIs(t, svc.Delete(ctx, "svc-nokey", "apiKey"), plugin.ErrSecretsUnavailable)
	_, err := svc.Get(ctx, "svc-nokey", "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretsUnavailable)

	var n int64
	require.NoError(t, db.Model(&model.PluginSecret{}).Where(`"pluginName" = ?`, "svc-nokey").Count(&n).Error)
	assert.Zero(t, n, "鍵が無いのに行ができている (平文で入った可能性)")
}

func TestService_SetReplacesAndDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t, testKey(1), "svc-replace")
	require.NoError(t, svc.Set(ctx, "svc-replace", "apiKey", "first"))
	require.NoError(t, svc.Set(ctx, "svc-replace", "apiKey", "second"))
	got, err := svc.Get(ctx, "svc-replace", "apiKey")
	require.NoError(t, err)
	assert.Equal(t, "second", got)

	require.NoError(t, svc.Delete(ctx, "svc-replace", "apiKey"))
	require.NoError(t, svc.Delete(ctx, "svc-replace", "apiKey"), "無いものを消して失敗した")
	_, err = svc.Get(ctx, "svc-replace", "apiKey")
	assert.ErrorIs(t, err, plugin.ErrSecretNotSet)
}

// 管理画面向けの状態には値を入れない。末尾は 20 文字以上の値の 4 文字だけ。
func TestService_StatusesNeverCarryTheValue(t *testing.T) {
	ctx := context.Background()
	svc, db := newService(t, testKey(1), "svc-status")
	before := time.Now()

	require.NoError(t, svc.Set(ctx, "svc-status", "long", "abcdefghijklmnop1234"))  // 20 文字
	require.NoError(t, svc.Set(ctx, "svc-status", "short", "abcdefghijklmnop123"))  // 19 文字
	require.NoError(t, svc.Set(ctx, "svc-status", "multi", "あいうえおかきくけこさしすせそたちつてと")) // 20 文字 (rune)

	after := time.Now()
	sts, err := svc.Statuses(ctx, "svc-status")
	require.NoError(t, err)
	require.Len(t, sts, 3)
	byName := map[string]pluginsecret.Status{}
	for _, st := range sts {
		byName[st.Name] = st
		assert.True(t, st.Configured)
		assert.True(t, st.Readable)
		assert.False(t, st.UpdatedAt.Before(before.Add(-time.Second)) || st.UpdatedAt.After(after.Add(time.Second)), "updatedAt: %v", st.UpdatedAt)
	}
	assert.Equal(t, []string{"long", "multi", "short"}, []string{sts[0].Name, sts[1].Name, sts[2].Name}, "名前順")
	assert.Equal(t, "1234", byName["long"].Hint)
	assert.Equal(t, "", byName["short"].Hint, "短い値の末尾を出している")
	assert.Equal(t, "ちつてと", byName["multi"].Hint, "バイトではなく文字で数える")

	// 別の鍵では「置いてあるが読めない」になり、末尾も出さない。
	other, err := newOver(db, testKey(2))
	require.NoError(t, err)
	sts, err = other.Statuses(ctx, "svc-status")
	require.NoError(t, err)
	require.Len(t, sts, 3)
	for _, st := range sts {
		assert.True(t, st.Configured)
		assert.False(t, st.Readable)
		assert.Empty(t, st.Hint)
	}

	// 鍵が無くても「置いてあるか」は分かる。
	none, err := newOver(db, nil)
	require.NoError(t, err)
	sts, err = none.Statuses(ctx, "svc-status")
	require.NoError(t, err)
	assert.Len(t, sts, 3)
	for _, st := range sts {
		assert.False(t, st.Readable)
		assert.Empty(t, st.Hint)
	}
}

func TestService_RejectsInvalidInput(t *testing.T) {
	ctx := context.Background()
	svc, _ := newService(t, testKey(1), "svc-invalid")

	for _, name := range []string{"", "1abc", "_abc", "has space", "ドット", "a.b", "a\x00b", strings.Repeat("a", 65)} {
		assert.ErrorIsf(t, svc.Set(ctx, "svc-invalid", name, "v"), pluginsecret.ErrInvalidName, "名前 %q を受け付けた", name)
		_, err := svc.Get(ctx, "svc-invalid", name)
		assert.ErrorIsf(t, err, pluginsecret.ErrInvalidName, "名前 %q で読めた", name)
		assert.ErrorIsf(t, svc.Delete(ctx, "svc-invalid", name), pluginsecret.ErrInvalidName, "名前 %q で消せた", name)
	}
	for _, v := range []string{"", strings.Repeat("x", pluginsecret.MaxValueBytes+1), "\xff\xfe"} {
		assert.ErrorIs(t, svc.Set(ctx, "svc-invalid", "apiKey", v), pluginsecret.ErrInvalidValue)
	}
	require.NoError(t, svc.Set(ctx, "svc-invalid", "apiKey", strings.Repeat("x", pluginsecret.MaxValueBytes)), "上限ちょうどは受ける")

	for _, p := range []string{"", "Bad", "a\x00b", strings.Repeat("a", 33)} {
		assert.Errorf(t, svc.Set(ctx, p, "apiKey", "v"), "プラグイン名 %q を受け付けた", p)
	}
}

// 宣言の検査 (plugin.Definition.Validate) と保存の検査が同じ規則であること。
// 食い違うと、宣言は通ったのに保存で弾かれる名前ができる。
func TestValidNameMatchesDefinition(t *testing.T) {
	jobs := func(plugin.Context, plugin.Jobs) error { return nil }
	for _, name := range []string{
		"apiKey", "api_key", "api-key", "A", "a1", "Z9_-", strings.Repeat("a", 64),
		"", "1a", "_a", "-a", "a b", "a.b", "あ", strings.Repeat("a", 65),
	} {
		def := plugin.Definition{
			Name: "x", APIVersion: plugin.APIVersion, Jobs: jobs,
			Secrets: []plugin.SecretSpec{{Name: name}},
		}
		assert.Equalf(t, def.Validate() == nil, pluginsecret.ValidName(name), "名前 %q の判定が食い違っている", name)
	}
}

type failingRepo struct{ err error }

func (r failingRepo) FindByName(context.Context, string, string) (*pluginsecret.Row, error) {
	return nil, r.err
}

func (r failingRepo) ListByPlugin(context.Context, string) ([]pluginsecret.Row, error) {
	return nil, r.err
}
func (r failingRepo) Upsert(context.Context, pluginsecret.Row) error { return r.err }
func (r failingRepo) Delete(context.Context, string, string) error   { return r.err }

// DB の障害を「未設定」に化けさせない。
func TestService_DBErrorsAreNotNotSet(t *testing.T) {
	boom := errors.New("boom")
	svc, err := pluginsecret.New(failingRepo{err: boom}, testKey(1))
	require.NoError(t, err)
	_, err = svc.Get(context.Background(), "bot", "apiKey")
	assert.ErrorIs(t, err, boom)
	assert.NotErrorIs(t, err, plugin.ErrSecretNotSet)
	_, err = svc.Statuses(context.Background(), "bot")
	assert.ErrorIs(t, err, boom)
}

func TestNew_RejectsBadKey(t *testing.T) {
	_, err := pluginsecret.New(nil, make([]byte, 16))
	assert.Error(t, err)
}
