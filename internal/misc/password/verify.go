package password

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/sync/semaphore"
)

// Scheme identifies the password hash format selected by Verify.
type Scheme uint8

const (
	SchemeUnknown Scheme = iota
	SchemeBcrypt
	SchemeArgon2id
)

const (
	cherryPickArgon2Params = "m=65536,t=3,p=4"
	argon2MaxConcurrency   = 4
)

var argon2VerifySlots = semaphore.NewWeighted(argon2MaxConcurrency)

// argon2AcquireTimeout は枠が空くのを待つ上限 (#2849)。
//
// **バーストを吸収できる長さにする。** 輻輳下では 1 回の検証に実測 105 ms 前後
// かかり (直列だと 42-64 ms)、枠は 4 つなのでスループットの上限は実測 約 40 req/s
// (同時 100 を 2.63 秒で捌いた測定から)。切替直後は全ログインが Argon2id になる
// ため、朝のログイン集中がそのままここに来る。短すぎると、セマフォが守ろうと
// しているまさにその負荷で正当な利用者が弾かれる。
//
// 実測 (同時数 → 失敗数): 1 秒だと 50 → 14 / 100 → 58、3 秒なら 50 → 0 /
// 100 → 0 で、200 → 83 と過負荷では意図どおり shed する。goroutine を 3 秒
// 保持するコストより、正当なログインを落とすほうが高い。
//
// **const ではなく var。** in-package のテストが本物の枠枯渇を短い待ちで
// 踏めるようにするため (export はしないので公開面は変わらない)。値そのものは
// TestArgon2AcquireTimeout が固定する。
var argon2AcquireTimeout = 3 * time.Second

// String implements fmt.Stringer so log output is readable.
//
// **無いと slog が uint8 をそのまま出す** (`"scheme":2`)。operator に手掛かりを
// 残すのがこの仕組みの目的なので、数字だけでは意味が無い (#2849)。
func (s Scheme) String() string {
	switch s {
	case SchemeBcrypt:
		return "bcrypt"
	case SchemeArgon2id:
		return "argon2id"
	default:
		return "unknown"
	}
}

// Outcome reports why Verify accepted or rejected the password.
//
// **3 つの失敗を区別するために足した** (#2849)。以前は全て `false` を返しており、
// 「パスワードが違う」「未対応の hash」「検証枠を取れなかった」が呼び出し側から
// 同じに見えていた。移行元が想定と違う argon2 実装だと**全ユーザーが一斉に
// ログインできなくなるのに、operator に手掛かりが 1 つも残らない**。
type Outcome uint8

const (
	// OutcomeMismatch は hash を読めたが平文が一致しないもの (= パスワード間違い)。
	// **これだけはログに出さない** — 日常的に起きるうえ、出すと本物の異常が埋もれる。
	OutcomeMismatch Outcome = iota
	// OutcomeMatch は一致。
	OutcomeMatch
	// OutcomeUnsupported は hash が壊れているか、受理する profile ではないもの。
	// 呼び出し側は 403 のまま warn を出す (データの異常なので気付ける必要がある)。
	OutcomeUnsupported
	// OutcomeUnavailable は検証枠を取れなかったもの (負荷、または ctx のキャンセル)。
	// 呼び出し側は原則 503 + Retry-After を返す。**403 にしないこと** — 正しい
	// パスワードかもしれないので、利用者に不要な reset をさせてしまう。
	//
	// **例外は「password 不一致でも他の要素で認証できる」経路だけ** (#2853)。
	// そこは 503 で止めずに検証失敗として先へ進めてよい (`signin` の
	// `verifyPasswordTolerant` が唯一の実装)。**ただし「検証済み」に倒さないこと** —
	// 未検証の平文が移行やハッシュ更新の入力になる。
	OutcomeUnavailable
)

// OK reports whether the password matched.
func (o Outcome) OK() bool { return o == OutcomeMatch }

// Verify compares plain with stored using the hash format declared by stored.
// Malformed and unsupported hashes fail closed.
func Verify(ctx context.Context, stored, plain string) (Scheme, Outcome) {
	if strings.HasPrefix(stored, "$2") {
		switch err := bcrypt.CompareHashAndPassword([]byte(stored), []byte(plain)); {
		case err == nil:
			return SchemeBcrypt, OutcomeMatch
		case errors.Is(err, bcrypt.ErrMismatchedHashAndPassword):
			return SchemeBcrypt, OutcomeMismatch
		default:
			// ErrHashTooShort / ErrHashVersionTooNew など。**平文の問題ではなく
			// 保存されている hash の問題**なので mismatch と混ぜない。
			return SchemeBcrypt, OutcomeUnsupported
		}
	}
	if !strings.HasPrefix(stored, "$argon2id$") {
		return SchemeUnknown, OutcomeUnsupported
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != cherryPickArgon2Params {
		return SchemeArgon2id, OutcomeUnsupported
	}
	salt, err := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return SchemeArgon2id, OutcomeUnsupported
	}
	// **digest 長の検査は冗長ではない** (#2850)。ConstantTimeCompare は長さ違いで
	// 0 を返すので「一致しない」結果自体は同じだが、返る Outcome が
	// Unsupported ではなく Mismatch になる。呼び出し側は Unsupported のときだけ
	// warn を出すので、外すと**移行元が想定と違う argon2 実装だと気付く唯一の
	// 手掛かりが消える** (#2849)。node-argon2 の hashLength だけ既定から変えた
	// 移行元は、m/t/p を保ったままここへ来る。
	want, err := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if err != nil || len(want) != 32 {
		return SchemeArgon2id, OutcomeUnsupported
	}
	acquireCtx, cancel := context.WithTimeout(ctx, argon2AcquireTimeout)
	defer cancel()
	if err := argon2VerifySlots.Acquire(acquireCtx, 1); err != nil {
		return SchemeArgon2id, OutcomeUnavailable
	}
	defer argon2VerifySlots.Release(1)
	got := argon2.IDKey([]byte(plain), salt, 3, 64*1024, 4, 32)
	if subtle.ConstantTimeCompare(got, want) == 1 {
		return SchemeArgon2id, OutcomeMatch
	}
	return SchemeArgon2id, OutcomeMismatch
}

// ProfileForLog returns the version and parameter fields of stored for
// diagnostics.
//
// **salt と digest は返さない。** 未対応 profile を診断するのに要るのは `v=` と
// パラメータだけで、hash 本体をログへ出す理由が無い。
func ProfileForLog(stored string) string {
	if strings.HasPrefix(stored, "$2") {
		return "bcrypt"
	}
	parts := strings.Split(stored, "$")
	if len(parts) < 4 || parts[1] != "argon2id" {
		return "unrecognized"
	}
	// **想定の形でなければ値を返さない。** PHC の field 数が 6 でないとき
	// parts[2] / parts[3] には salt や digest が来る。argon2 のリファレンス実装は
	// version 0x10 で `$v=` を省くので、これは「移行元が想定と違う実装」という
	// **まさに診断したい場面で踏む**形 (#2849)。
	// field の数が合っていても、中身が version / パラメータの形でなければ返さない。
	// 区切りが1つずれた形では、parts[3] に salt が来る。
	if len(parts) != 6 || !isPHCVersion(parts[2]) || !isPHCParams(parts[3]) {
		return fmt.Sprintf("argon2id (unexpected field layout, parts=%d)", len(parts))
	}
	return "argon2id " + parts[2] + " " + parts[3]
}

// isPHCVersion reports whether s is a PHC version field such as "v=19".
func isPHCVersion(s string) bool {
	v, ok := strings.CutPrefix(s, "v=")
	return ok && isDecimal(v)
}

// isPHCParams reports whether s is a PHC parameter list such as
// "m=65536,t=3,p=4": comma-separated lowercase keys with decimal values.
func isPHCParams(s string) bool {
	for kv := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" || !isDecimal(v) {
			return false
		}
		for _, r := range k {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

func isDecimal(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
