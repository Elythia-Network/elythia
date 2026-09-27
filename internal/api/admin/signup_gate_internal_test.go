package admin

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 登録ゲートの補助関数を直接固定する (#2803)。
//
// **HTTP 経由では届かない形を試すためのファイル。** normalizeSignupGateBools が
// 先に null を落とし残りを 400 で弾くので、handler 経由のテストでは
// metaBoolExplicit に bool 以外が渡ることはない。そのぶん「presence だけの判定に
// 緩める」変更を handler のテストが検出できず、**登録が開いたまま残る側へ倒れる
// 変更が無検証で通る**。ここで直接押さえる。

func TestMetaBoolExplicit(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]any
		want   bool
	}{
		{name: "absent", fields: map[string]any{}, want: false},
		{name: "bool true", fields: map[string]any{"disableRegistration": true}, want: true},
		{name: "bool false", fields: map[string]any{"disableRegistration": false}, want: true},
		// bool 以外を明示扱いすると、閉じる既定を飛ばして開いたまま残す側に倒れる。
		{name: "string", fields: map[string]any{"disableRegistration": "true"}, want: false},
		{name: "json null", fields: map[string]any{"disableRegistration": nil}, want: false},
		{name: "number", fields: map[string]any{"disableRegistration": float64(1)}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, metaBoolExplicit(tt.fields, "disableRegistration"))
		})
	}
}

// normalizeSignupConditions は meta が引けないと補正を掛けられない。
//
// **fail-open なので、条件と一緒に固定しておく。** handler 経由では
// maybeAutoGenerateVAPID が同じ欠損で 500 にするため列は書かれないが、この関数
// 単体の挙動はそれとは独立に決まる。
func TestNormalizeSignupConditions_NilCurrent(t *testing.T) {
	t.Run("承認制を切る更新は補正できない", func(t *testing.T) {
		fields := map[string]any{"approvalRequiredForSignup": false}
		normalizeSignupConditions(fields, nil)
		_, ok := fields["disableRegistration"]
		assert.False(t, ok, "遷移を判定できないので触らない")
	})
	t.Run("承認制を入れる更新は矛盾する明示だけを正す", func(t *testing.T) {
		fields := map[string]any{"approvalRequiredForSignup": true, "disableRegistration": true}
		normalizeSignupConditions(fields, nil)
		assert.Equal(t, false, fields["disableRegistration"])
	})
}

// null は落とし、bool 以外は弾く (#2803)。
func TestNormalizeSignupGateBools(t *testing.T) {
	t.Run("null は無指定に落とす", func(t *testing.T) {
		fields := map[string]any{"disableRegistration": nil, "approvalRequiredForSignup": nil}
		require.NoError(t, normalizeSignupGateBools(fields))
		assert.Empty(t, fields)
	})
	t.Run("bool はそのまま残す", func(t *testing.T) {
		fields := map[string]any{"disableRegistration": true, "approvalRequiredForSignup": false}
		require.NoError(t, normalizeSignupGateBools(fields))
		assert.Equal(t, map[string]any{"disableRegistration": true, "approvalRequiredForSignup": false}, fields)
	})
	t.Run("対象外の列は型を見ない", func(t *testing.T) {
		fields := map[string]any{"emailRequiredForSignup": "true"}
		require.NoError(t, normalizeSignupGateBools(fields))
		assert.Equal(t, "true", fields["emailRequiredForSignup"])
	})
	for _, key := range signupGateBoolFields {
		t.Run(key+" が bool 以外なら error", func(t *testing.T) {
			fields := map[string]any{key: "false"}
			err := normalizeSignupGateBools(fields)
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
}

// meta が引けないときは、この更新で送られた値だけで判定する。
func TestNormalizeRegistrationClosed_NilCurrent(t *testing.T) {
	t.Run("閉じる更新は招待制の値も立てる", func(t *testing.T) {
		fields := map[string]any{"registrationClosed": true, "disableRegistration": false}
		normalizeRegistrationClosed(fields, nil)
		assert.Equal(t, true, fields["disableRegistration"])
	})
	t.Run("解除する更新は遷移を判定できないので触らない", func(t *testing.T) {
		fields := map[string]any{"registrationClosed": false, "approvalRequiredForSignup": true}
		normalizeRegistrationClosed(fields, nil)
		_, ok := fields["disableRegistration"]
		assert.False(t, ok)
	})
}
