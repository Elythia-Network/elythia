package plugin

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// resetRegistry clears the global registry between tests.
func resetRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	registry = map[string]Definition{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = map[string]Definition{}
		registryMu.Unlock()
	})
}

// validDef builds a definition that passes Validate.
func validDef(name string) Definition {
	return Definition{
		Name:       name,
		Version:    "1.0.0",
		APIVersion: APIVersion,
		Routes:     func(Context, Router) error { return nil },
	}
}

type customPluginErrorCode struct {
	err  error
	code string
}

func (c customPluginErrorCode) Error() string { return c.err.Error() }
func (c customPluginErrorCode) Unwrap() error {
	return c.err
}
func (c customPluginErrorCode) PluginErrorCode() string { return c.code }

type customStatusErrorAs struct {
	statusErr *StatusError
}

func (e customStatusErrorAs) Error() string { return "custom status error" }
func (e customStatusErrorAs) As(target any) bool {
	statusErr, ok := target.(**StatusError)
	if ok {
		*statusErr = e.statusErr
	}
	return ok
}

func TestDefinition_Validate_Accepts(t *testing.T) {
	require.NoError(t, validDef("gameinfo").Validate())

	// Jobs だけでもよい (queue 専用のプラグイン)。
	jobsOnly := Definition{Name: "x", APIVersion: APIVersion, Jobs: func(Context, Jobs) error { return nil }}
	require.NoError(t, jobsOnly.Validate())

	// 通知の handler だけでもよい (#3469。bot の返事だけをするプラグイン)。
	notifOnly := Definition{Name: "x", APIVersion: APIVersion, Notifications: func(Context, Notifications) error { return nil }}
	require.NoError(t, notifOnly.Validate())
}

func TestDefinition_Validate_Rejects(t *testing.T) {
	tests := []struct {
		name string
		def  Definition
		want string
	}{
		{
			name: "empty name",
			def:  Definition{APIVersion: APIVersion, Routes: func(Context, Router) error { return nil }},
			want: "Name が空",
		},
		{
			name: "no callbacks",
			def:  Definition{Name: "x", APIVersion: APIVersion},
			want: "Routes も Jobs も",
		},
		{
			name: "api version mismatch",
			def: Definition{
				Name: "x", APIVersion: APIVersion + 1,
				Routes: func(Context, Router) error { return nil },
			},
			want: "互換がありません",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.def.Validate()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

// **APIVersion が 0 のものを通さない。** 未設定の構造体をそのまま登録すると
// 互換性の宣言が無いまま動いてしまう。
func TestDefinition_Validate_RejectsZeroAPIVersion(t *testing.T) {
	def := Definition{Name: "x", Routes: func(Context, Router) error { return nil }}
	require.Error(t, def.Validate())
}

// 名前は URL path / queue task type / PostgreSQL schema の 3 箇所で使う。
// どれか 1 つでも通らない文字を最初から弾く。
func TestValidName(t *testing.T) {
	ok := []string{"a", "ab", "game-info", "x1", "a-b-c", strings.Repeat("a", 32)}
	for _, s := range ok {
		assert.Truef(t, validName(s), "%q は許可される", s)
	}

	ng := map[string]string{
		"":                      "空",
		"A":                     "大文字",
		"game_info":             "アンダースコア",
		"-lead":                 "先頭ハイフン",
		"trail-":                "末尾ハイフン",
		"ドライブ":                  "非 ASCII",
		"a/b":                   "スラッシュ (path を割る)",
		"a.b":                   "ドット (schema 修飾に見える)",
		strings.Repeat("a", 33): "長すぎる",
	}
	for s, why := range ng {
		assert.Falsef(t, validName(s), "%q は拒否される (%s)", s, why)
	}
}

func TestRegister_And_Registered(t *testing.T) {
	resetRegistry(t)

	Register(validDef("beta"))
	Register(validDef("alpha"))

	got := Registered()
	require.Len(t, got, 2)
	// 順序が固定されること (ルート登録順やログの並びをビルドごとに変えない)。
	assert.Equal(t, "alpha", got[0].Name)
	assert.Equal(t, "beta", got[1].Name)
}

func TestRegistered_EmptyByDefault(t *testing.T) {
	resetRegistry(t)
	assert.Empty(t, Registered())
}

// 生成コードの不具合なので、起動を許さず panic する。どちらが有効か分からない
// まま動く方が危険。
func TestRegister_PanicsOnDuplicate(t *testing.T) {
	resetRegistry(t)

	Register(validDef("dup"))
	assert.PanicsWithError(t, `plugin: "dup" が二重に登録されました`, func() {
		Register(validDef("dup"))
	})
}

func TestRegister_PanicsOnInvalid(t *testing.T) {
	resetRegistry(t)
	assert.Panics(t, func() { Register(Definition{}) })
}

func TestStatusError(t *testing.T) {
	err := Errorf(http.StatusBadRequest, "%s が不正です", "id")
	assert.Equal(t, http.StatusBadRequest, err.Status)
	assert.Equal(t, "id が不正です", err.Message)
	assert.Equal(t, "400: id が不正です", err.Error())

	// error として扱えること (errors.As で status を取り出す用途)。
	var se *StatusError
	require.True(t, errors.As(error(err), &se))
	assert.Equal(t, http.StatusBadRequest, se.Status)
}

func TestNewCodedStatusError(t *testing.T) {
	err := NewCodedStatusError(http.StatusConflict, "設定が更新されています", "REVISION_CONFLICT")

	var se *StatusError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, http.StatusConflict, se.Status)
	assert.Equal(t, "設定が更新されています", se.Message)
	assert.Equal(t, "409: 設定が更新されています", err.Error())
}

func TestNewCodedStatusError_EmptyCodeIsLegacyStatusError(t *testing.T) {
	err := NewCodedStatusError(http.StatusBadRequest, "id が不正です", "")

	var se *StatusError
	require.True(t, errors.As(err, &se))
	assert.Equal(t, http.StatusBadRequest, se.Status)
	assert.Equal(t, "id が不正です", se.Message)
}

func TestNewCodedStatusError_InvalidCodeFallsBackToLegacyStatusError(t *testing.T) {
	err := NewCodedStatusError(http.StatusBadRequest, "140 文字以内にしてください", "too-long")

	statusErr, code := ExtractStatusError(err)
	require.NotNil(t, statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.Status)
	assert.Equal(t, "140 文字以内にしてください", statusErr.Message)
	assert.Empty(t, code)
	assert.NotContains(t, err.Error(), "too-long")
}

func TestValidErrorCodeRejectsEmpty(t *testing.T) {
	assert.False(t, validErrorCode(""))
}

func TestExtractStatusError_PrefersOuterStatusError(t *testing.T) {
	err := errors.Join(
		fmt.Errorf("ctx: %w", Errorf(http.StatusBadRequest, "bad")),
		NewCodedStatusError(http.StatusConflict, "conflict", "REVISION_CONFLICT"),
	)

	statusErr, code := ExtractStatusError(err)
	require.NotNil(t, statusErr)
	assert.Equal(t, http.StatusBadRequest, statusErr.Status)
	assert.Equal(t, "bad", statusErr.Message)
	assert.Empty(t, code)
}

func TestExtractStatusError_PreservesJoinOrderWhenCodedIsFirst(t *testing.T) {
	err := errors.Join(
		NewCodedStatusError(http.StatusConflict, "conflict", "REVISION_CONFLICT"),
		Errorf(http.StatusBadRequest, "bad"),
	)

	statusErr, code := ExtractStatusError(err)
	require.NotNil(t, statusErr)
	assert.Equal(t, http.StatusConflict, statusErr.Status)
	assert.Equal(t, "conflict", statusErr.Message)
	assert.Equal(t, "REVISION_CONFLICT", code)
}

func TestExtractStatusError_CustomWrapperDoesNotInjectCode(t *testing.T) {
	err := errors.Join(
		Errorf(http.StatusForbidden, "denied"),
		customPluginErrorCode{
			err:  Errorf(http.StatusConflict, "conflict"),
			code: "CUSTOM_CODE_SHOULD_NOT_WIN",
		},
	)

	statusErr, code := ExtractStatusError(err)
	require.NotNil(t, statusErr)
	assert.Equal(t, http.StatusForbidden, statusErr.Status)
	assert.Equal(t, "denied", statusErr.Message)
	assert.Empty(t, code)
}

func TestExtractStatusError_PreservesCustomAsCompatibility(t *testing.T) {
	err := errors.Join(
		customStatusErrorAs{statusErr: Errorf(http.StatusTeapot, "legacy custom As")},
		NewCodedStatusError(http.StatusConflict, "conflict", "REVISION_CONFLICT"),
	)

	statusErr, code := ExtractStatusError(err)
	require.NotNil(t, statusErr)
	assert.Equal(t, http.StatusTeapot, statusErr.Status)
	assert.Equal(t, "legacy custom As", statusErr.Message)
	assert.Empty(t, code)
}

func TestErrNotFound(t *testing.T) {
	err := ErrNotFound("note %s", "abc")
	assert.Equal(t, http.StatusNotFound, err.Status)
	assert.Equal(t, "note abc", err.Message)
}

func TestAPIError(t *testing.T) {
	err := &APIError{Endpoint: "notes/show", Status: 404, Body: []byte(`{"error":{"code":"NO_SUCH_NOTE"}}`)}
	msg := err.Error()
	assert.Contains(t, msg, "notes/show")
	assert.Contains(t, msg, "404")
	// **本文をそのまま含める。** Misskey のエラーコードが読めないと、
	// プラグイン側で原因の切り分けができない。
	assert.Contains(t, msg, "NO_SUCH_NOTE")
}

// Peer は Peered とセットでないと効かない。宣言しても無視されると
// 「設定したつもり」で通ってしまう。
func TestDefinition_Validate_PeerRequiresPeered(t *testing.T) {
	base := Definition{Name: "demo", APIVersion: APIVersion}

	withPeer := base
	withPeer.Peer = func(Context, Peer) error { return nil }
	err := withPeer.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Peered")

	withPeer.Peered = true
	assert.NoError(t, withPeer.Validate(), "Peer だけでも成立する (受信専用のプラグイン)")

	// Routes / Jobs / EffectivePolicies / Peer のいずれも無ければエラー。
	assert.Error(t, base.Validate())
}

func TestNoRetry(t *testing.T) {
	assert.NoError(t, NoRetry(nil))
	base := errors.New("gone")
	err := NoRetry(base)
	assert.ErrorIs(t, err, ErrNoRetry)
	assert.ErrorIs(t, err, base, "元のエラーも残す")
}
