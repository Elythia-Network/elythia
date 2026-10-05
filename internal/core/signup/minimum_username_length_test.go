package signup_test

import (
	"errors"
	"testing"

	"github.com/elythia-network/elythia/internal/core/signup"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 新規登録の username に最小文字数を設けられるようにした (#3015)。
//
// **`preservedUsernames` では代用できない。** あれは名前を 1 つずつ列挙する
// 仕組みなので「2 文字以下を全部」は書けない (`[a-zA-Z0-9_]` の 1-2 文字だけで
// 63 + 63^2 = 4,032 通り)。

// EffectiveMinimumUsernameLength は列の生値ではなく**実際に効く値**を返す。
//
// **公開 meta に出る値なので、丸め方が登録側とずれると
// 「空いています」と案内した名前が登録で弾かれる。**
func TestEffectiveMinimumUsernameLength_Clamps(t *testing.T) {
	for name, tc := range map[string]struct {
		meta *model.Meta
		want int
	}{
		"meta が引けない":           {nil, 1},
		"列が未設定 (0)":            {&model.Meta{}, 1},
		"負値 (手で UPDATE された行)":  {&model.Meta{MinimumUsernameLength: -5}, 1},
		"1 (既定)":               {&model.Meta{MinimumUsernameLength: 1}, 1},
		"5":                    {&model.Meta{MinimumUsernameLength: 5}, 5},
		"20 (pattern の上限ちょうど)": {&model.Meta{MinimumUsernameLength: 20}, 20},
		"21 (これを通すと登録が全滅する)": {&model.Meta{MinimumUsernameLength: 21}, 20},
		"999": {&model.Meta{MinimumUsernameLength: 999}, 20},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, signup.EffectiveMinimumUsernameLength(tc.meta))
		})
	}
}

// 上限を丸めずに通すと、どの username も format 検証で落ちる。
//
// **「登録が全部 400 になるインスタンス」は設定ミスで作れてはいけない。**
// 21 を丸めていることを、実際に登録を通して確かめる。
func TestSignup_OutOfRangeMinimumStillAllowsTheLongestUsername(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 999

	longest := "abcdefghij0123456789" // 20 文字 = localUsernamePattern の上限
	require.Len(t, longest, 20)
	_, err := svc.Signup(longest, "pass1234", false, signup.UsernamePolicyPublic)
	require.NoError(t, err, "上限を丸めていないので、どの username も通らない")
	assert.Len(t, userRepo.Users, 1)
}

// 境界: n-1 は落ち、n は通る。
func TestSignup_RejectsUsernameShorterThanMinimum(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 5

	_, err := svc.Signup("abcd", "pass1234", false, signup.UsernamePolicyPublic)
	require.ErrorIs(t, err, signup.ErrUsernameTooShort)
	assert.Empty(t, userRepo.Users, "弾いたのにアカウントを作っている")

	// **`ErrInvalidUsername` に化けていないこと。** format 違反と混ぜると、
	// handler が INVALID_USERNAME を返して「使えない文字が入っています」に
	// なり、何文字必要かが利用者に伝わらない。
	assert.False(t, errors.Is(err, signup.ErrInvalidUsername))

	res, err := svc.Signup("abcde", "pass1234", false, signup.UsernamePolicyPublic)
	require.NoError(t, err, "ちょうど n 文字は通る")
	assert.Equal(t, "abcde", res.User.Username)
}

// 既定 (1) では挙動が変わらない。**既存インスタンスの登録を壊さない。**
func TestSignup_DefaultMinimumAllowsSingleCharUsername(t *testing.T) {
	for name, v := range map[string]int{
		"列が未設定 (0)": 0,
		"既定値 (1)":   1,
	} {
		t.Run(name, func(t *testing.T) {
			svc, _, metaRepo := newTestService(t)
			metaRepo.Meta.MinimumUsernameLength = v
			_, err := svc.Signup("a", "pass1234", false, signup.UsernamePolicyPublic)
			require.NoError(t, err)
		})
	}
}

// admin 経路 (`admin/accounts/create`) は最小文字数を受けない。
//
// **位置では区別が付かない** — admin も `isInitialSetup == false` で
// `Signup` を呼ぶので、呼び出し側が policy を選ぶ。
func TestSignup_OperatorPolicySkipsMinimumButKeepsPreserved(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 10

	res, err := svc.Signup("ops", "pass1234", false, signup.UsernamePolicyOperator)
	require.NoError(t, err, "運営が公式アカウントに短い ID を配れない")
	assert.Equal(t, "ops", res.User.Username)
	assert.Len(t, userRepo.Users, 1)

	// **予約は引き続き効く。** 除外したのは最小長だけ。
	metaRepo.Meta.PreservedUsernames = model.StringArray{"root"}
	_, err = svc.Signup("root", "pass1234", false, signup.UsernamePolicyOperator)
	assert.ErrorIs(t, err, signup.ErrUsernameReserved)
}

// 初回セットアップは policy によらず最小文字数を受けない。
//
// **これが無いと、n を大きくした状態で構築し直すと最初のアカウントが
// 作れなくなる。**
func TestSignup_InitialSetupSkipsMinimum(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 10

	_, err := svc.Signup("root", "pass1234", true, signup.UsernamePolicyPublic)
	require.NoError(t, err)
}

// メール確認経路 (`CreatePending` → `CreatePendingForApplication`) も同じ制限。
//
// **ここを抜くと「確認メールを踏んだ瞬間に弾かれる」形になる。**
func TestCreatePending_RejectsUsernameShorterThanMinimum(t *testing.T) {
	svc, _, metaRepo := newTestService(t)
	pendingRepo := testutil.NewMockUserPendingRepository()
	svc.SetUserPendingRepo(pendingRepo)
	metaRepo.Meta.MinimumUsernameLength = 5

	_, err := svc.CreatePending("abcd", "a@example.com", "pass1234", nil)
	require.ErrorIs(t, err, signup.ErrUsernameTooShort)
	assert.Empty(t, pendingRepo.Rows, "弾いたのに pending を作っている")

	_, err = svc.CreatePending("abcde", "b@example.com", "pass1234", nil)
	require.NoError(t, err)
	assert.Len(t, pendingRepo.Rows, 1)
}

// 承認制の申請経由 (`SignupForApplication`) も同じ制限。
//
// ここで踏むのは db / appRepo が未配線のときの fallback (`Signup` へ委譲する
// 枝)。tx 経路は実 PostgreSQL が要るので integration test 側で見る。
func TestSignupForApplication_UnwiredFallbackAppliesMinimum(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 5

	_, err := svc.SignupForApplication("abcd", "pass1234", "app1", "")
	require.ErrorIs(t, err, signup.ErrUsernameTooShort)
	assert.Empty(t, userRepo.Users)

	_, err = svc.SignupForApplication("abcde", "pass1234", "app1", "")
	require.NoError(t, err)
}

// meta が引けないときは通す。
//
// **`preservedUsernames` と同じ扱いに揃える** (同じ `if meta, err := Fetch()`
// ブロックの中にいる)。最小文字数は運営者の好みであってセキュリティ境界では
// ないので、DB 障害で登録全体を止める側には倒さない。承認制 (#2804) は
// fail-closed だが、あちらは「承認されていない人を通さない」ゲートで性質が違う。
func TestSignup_MinimumIsSkippedWhenMetaUnavailable(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 10
	metaRepo.Meta = nil // Fetch が error を返す

	_, err := svc.Signup("abcd", "pass1234", false, signup.UsernamePolicyPublic)
	require.NoError(t, err)
	assert.Len(t, userRepo.Users, 1)
}

// `username/available` と登録が同じ判定を通ること。
//
// **`ViolatesMinimumUsernameLength` を export しているのはこのため。**
// available 側に判定を書き写すと、値の丸め方が 2 箇所に分かれる。
func TestViolatesMinimumUsernameLength_MatchesSignup(t *testing.T) {
	for name, tc := range map[string]struct {
		min      int
		username string
		policy   signup.UsernamePolicy
		want     bool
	}{
		"n-1 は違反":           {5, "abcd", signup.UsernamePolicyPublic, true},
		"n は違反しない":          {5, "abcde", signup.UsernamePolicyPublic, false},
		"n+1 は違反しない":        {5, "abcdef", signup.UsernamePolicyPublic, false},
		"既定 (1) では 1 文字も通る": {1, "a", signup.UsernamePolicyPublic, false},
		"範囲外の値は 20 に丸める":    {999, "abcdefghij0123456789", signup.UsernamePolicyPublic, false},
		"operator は常に違反しない": {20, "a", signup.UsernamePolicyOperator, false},
	} {
		t.Run(name, func(t *testing.T) {
			meta := &model.Meta{MinimumUsernameLength: tc.min}
			assert.Equal(t, tc.want,
				signup.ViolatesMinimumUsernameLength(tc.username, meta, tc.policy))
		})
	}
}

// multibyte は最小長の判定に届かない。
//
// **`violatesMinimumUsernameLength` は byte で数える**ので、format 検証より先に
// 呼ぶと `"あ"` (3 byte) が min=2 を通る。登録経路は
// `normalizeAndValidateUsername` を先に通すので届かないことを、順序が崩れたら
// 落ちる形で固定しておく。
func TestSignup_MultibyteIsRejectedBeforeTheMinimumCheck(t *testing.T) {
	svc, userRepo, metaRepo := newTestService(t)
	metaRepo.Meta.MinimumUsernameLength = 2

	_, err := svc.Signup("あ", "pass1234", false, signup.UsernamePolicyPublic)
	require.ErrorIs(t, err, signup.ErrInvalidUsername,
		"format 検証より先に最小長を見ている (byte 長なので multibyte が素通りする)")
	require.NotErrorIs(t, err, signup.ErrUsernameTooShort)
	assert.Empty(t, userRepo.Users)
}
