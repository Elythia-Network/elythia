package password

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

func cherryPickFixture(plain string) string {
	salt := []byte("0123456789abcdef")
	digest := argon2.IDKey([]byte(plain), salt, 3, 64*1024, 4, 32)
	return fmt.Sprintf("$argon2id$v=19$m=65536,t=3,p=4$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest))
}

func TestVerify_CherryPickArgon2id(t *testing.T) {
	hash := cherryPickFixture("correct horse battery staple")
	if scheme, out := Verify(context.Background(), hash, "correct horse battery staple"); out != OutcomeMatch || scheme != SchemeArgon2id {
		t.Fatalf("Verify(correct)=(%v,%v), want (%v,%v)", scheme, out, SchemeArgon2id, OutcomeMatch)
	}
	// **平文違いは Mismatch であって Unsupported ではない** (#2849)。ここを
	// 取り違えると、日常的な打ち間違いが警告ログを埋める。
	if scheme, out := Verify(context.Background(), hash, "wrong"); out != OutcomeMismatch || scheme != SchemeArgon2id {
		t.Fatalf("Verify(wrong)=(%v,%v), want (%v,%v)", scheme, out, SchemeArgon2id, OutcomeMismatch)
	}
}

func TestVerify_Bcrypt(t *testing.T) {
	hash, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if scheme, out := Verify(context.Background(), string(hash), "hunter2"); out != OutcomeMatch || scheme != SchemeBcrypt {
		t.Fatalf("Verify(bcrypt)=(%v,%v), want (%v,%v)", scheme, out, SchemeBcrypt, OutcomeMatch)
	}
	if scheme, out := Verify(context.Background(), string(hash), "wrong"); out != OutcomeMismatch || scheme != SchemeBcrypt {
		t.Fatalf("Verify(bcrypt wrong)=(%v,%v), want (%v,%v)", scheme, out, SchemeBcrypt, OutcomeMismatch)
	}
	// 壊れた bcrypt hash は「平文違い」ではなく**保存データの異常**なので
	// Unsupported に倒す (#2849)。
	if scheme, out := Verify(context.Background(), "$2a$broken", "hunter2"); out != OutcomeUnsupported || scheme != SchemeBcrypt {
		t.Fatalf("Verify(bcrypt broken)=(%v,%v), want (%v,%v)", scheme, out, SchemeBcrypt, OutcomeUnsupported)
	}
}

func TestVerify_RejectsMalformedOrUnsupportedHashes(t *testing.T) {
	valid := cherryPickFixture("hunter2")
	parts := strings.Split(valid, "$")
	shortSalt := []byte("short")
	shortSaltDigest := argon2.IDKey([]byte("hunter2"), shortSalt, 3, 64*1024, 4, 32)
	shortSaltHash := strings.Join([]string{
		"", parts[1], parts[2], parts[3],
		base64.RawStdEncoding.EncodeToString(shortSalt),
		base64.RawStdEncoding.EncodeToString(shortSaltDigest),
	}, "$")
	tests := []struct {
		name   string
		hash   string
		scheme Scheme
	}{
		{"unknown", "$scrypt$bad", SchemeUnknown},
		{"short", "$argon2id$v=19", SchemeArgon2id},
		{"argon2i", strings.Replace(valid, "$argon2id$", "$argon2i$", 1), SchemeUnknown},
		{"version", strings.Replace(valid, "v=19", "v=16", 1), SchemeArgon2id},
		{"memory", strings.Replace(valid, "m=65536", "m=4294967295", 1), SchemeArgon2id},
		{"iterations", strings.Replace(valid, "t=3", "t=4", 1), SchemeArgon2id},
		{"parallelism", strings.Replace(valid, "p=4", "p=255", 1), SchemeArgon2id},
		{"missing parameter", strings.Replace(valid, "m=65536,t=3,p=4", "m=65536,t=3", 1), SchemeArgon2id},
		{"duplicate parameter", strings.Replace(valid, "m=65536,t=3,p=4", "m=65536,t=3,p=4,p=4", 1), SchemeArgon2id},
		{"parameter order", strings.Replace(valid, "m=65536,t=3,p=4", "t=3,m=65536,p=4", 1), SchemeArgon2id},
		{"bad salt base64", strings.Join([]string{"", parts[1], parts[2], parts[3], "***", parts[5]}, "$"), SchemeArgon2id},
		{"short salt", shortSaltHash, SchemeArgon2id},
		{"bad digest base64", strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], "***"}, "$"), SchemeArgon2id},
		// **Unsupported と Mismatch の区別を固定している。** digest 長の検査を
		// 外すと Mismatch になり、呼び出し側の warn が消える (#2849 が足した
		// 唯一の手掛かり)。ConstantTimeCompare が長さ違いで 0 を返すので
		// 「一致しない」結果は同じだが、Outcome は同じではない (#2850)。
		{"short digest", strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], base64.RawStdEncoding.EncodeToString(make([]byte, 16))}, "$"), SchemeArgon2id},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// **Unsupported であることまで見る。** 単に「false」だと平文違いと
			// 区別が付かず、呼び出し側が warn を出す判断ができない (#2849)。
			scheme, out := Verify(context.Background(), tt.hash, "hunter2")
			if out != OutcomeUnsupported || scheme != tt.scheme {
				t.Fatalf("Verify=(%v,%v), want (%v,%v)", scheme, out, tt.scheme, OutcomeUnsupported)
			}
		})
	}
}

func TestVerify_Argon2idHonorsContextWhileWaitingForSlot(t *testing.T) {
	hash := cherryPickFixture("hunter2")
	const expectedMaxConcurrency = 4
	if err := argon2VerifySlots.Acquire(context.Background(), expectedMaxConcurrency); err != nil {
		t.Fatal(err)
	}
	defer argon2VerifySlots.Release(expectedMaxConcurrency)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	// **Unavailable であることまで見る。** Mismatch に倒すと呼び出し側が 403 を
	// 返してしまい、正しいパスワードの利用者に不要な reset をさせる (#2849)。
	if scheme, out := Verify(ctx, hash, "hunter2"); out != OutcomeUnavailable || scheme != SchemeArgon2id {
		t.Fatalf("Verify(canceled)=(%v,%v), want (%v,%v)", scheme, out, SchemeArgon2id, OutcomeUnavailable)
	}
}

// ProfileForLog は診断に要る version / parameter だけを返し、**salt と digest を
// 返さない** (#2849)。ここが漏れると、未対応 profile の警告を出すたびに hash 本体が
// ログへ流れる。
func TestProfileForLog_OmitsSaltAndDigest(t *testing.T) {
	hash := cherryPickFixture("hunter2")
	parts := strings.Split(hash, "$")
	got := ProfileForLog(hash)

	if got != "argon2id v=19 m=65536,t=3,p=4" {
		t.Fatalf("ProfileForLog(argon2id)=%q", got)
	}
	if strings.Contains(got, parts[4]) {
		t.Fatal("salt がログ用文字列に含まれている")
	}
	if strings.Contains(got, parts[5]) {
		t.Fatal("digest がログ用文字列に含まれている")
	}

	bc, err := bcrypt.GenerateFromPassword([]byte("hunter2"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if got := ProfileForLog(string(bc)); got != "bcrypt" {
		t.Fatalf("ProfileForLog(bcrypt)=%q", got)
	}
	if strings.Contains(ProfileForLog(string(bc)), string(bc)[7:]) {
		t.Fatal("bcrypt hash がログ用文字列に含まれている")
	}
	if got := ProfileForLog("$scrypt$bad"); got != "unrecognized" {
		t.Fatalf("ProfileForLog(unknown)=%q", got)
	}
}

// OK は OutcomeMatch だけを真にする (#2849)。
//
// **ここが緩むと認証が破れる。** 例えば OutcomeUnavailable を OK に含めると、
// 検証枠を取れなかっただけのリクエストがログイン成功になる。呼び出し側は
// `outcome.OK()` を素通しで passwordOK に使うので、この 1 関数が最後の砦。
func TestOutcome_OKOnlyForMatch(t *testing.T) {
	for _, tc := range []struct {
		out  Outcome
		want bool
	}{
		{OutcomeMatch, true},
		{OutcomeMismatch, false},
		{OutcomeUnsupported, false},
		{OutcomeUnavailable, false},
	} {
		if got := tc.out.OK(); got != tc.want {
			t.Fatalf("Outcome(%v).OK()=%v, want %v", tc.out, got, tc.want)
		}
	}
}

// **本物の枠枯渇**で Unavailable になること (#2849)。
//
// これまでは caller ctx を殺す枝しかテストしておらず、「枠が枯れたが ctx は
// 生きている」= 本番の過負荷そのものの枝が無検証だった。`Acquire` 失敗時に
// ctx が生きていれば Mismatch を返す変異 (= #2849 のバグを復活させる) が
// 素通りしていた。
//
// timeout は package private な var なので、in-package のこのテストだけが
// 短縮できる (export はしない)。
func TestVerify_ExhaustedSlotsReturnUnavailable(t *testing.T) {
	hash := cherryPickFixture("hunter2")
	if err := argon2VerifySlots.Acquire(context.Background(), argon2MaxConcurrency); err != nil {
		t.Fatal(err)
	}
	defer argon2VerifySlots.Release(argon2MaxConcurrency)

	prev := argon2AcquireTimeout
	argon2AcquireTimeout = 20 * time.Millisecond
	defer func() { argon2AcquireTimeout = prev }()

	// **ctx は生きている。** 死んでいると別の枝を踏んでしまう。
	scheme, out := Verify(context.Background(), hash, "hunter2")
	if out != OutcomeUnavailable || scheme != SchemeArgon2id {
		t.Fatalf("Verify(exhausted)=(%v,%v), want (%v,%v)", scheme, out, SchemeArgon2id, OutcomeUnavailable)
	}
}

// acquire timeout の値そのものを固定する (#2849)。
//
// 1 秒 → 3 秒がこの PR の主要変更の片方なのに、巻き戻す変異が全テストを
// 素通りしていた。バーストを吸収できる長さであることが要点で、実測では
// 1 秒だと同時 100 で 58 件が弾かれ、3 秒なら 0 件になる。
func TestArgon2AcquireTimeout(t *testing.T) {
	if argon2AcquireTimeout != 3*time.Second {
		t.Fatalf("argon2AcquireTimeout=%v, want 3s", argon2AcquireTimeout)
	}
}

// ProfileForLog は**想定外の field 構成でも値を返さない** (#2849)。
//
// argon2 のリファレンス実装は version 0x10 で `$v=` を省くので、
// `$argon2id$m=...$salt$digest` という 5 分割になる。この形は
// 「移行元が想定と違う実装」= まさに診断したい場面で踏むのに、初版は
// parts[2] / parts[3] を無条件で返しており salt (場合により digest) が
// ログへ出ていた。
func TestProfileForLog_UnexpectedLayoutOmitsValues(t *testing.T) {
	for _, in := range []string{
		"$argon2id$m=65536,t=3,p=4$c2FsdHNhbHRzYWx0c2Ex$ZGlnZXN0ZGlnZXN0",
		"$argon2id$c2FsdA$ZGlnZXN0",
		"$argon2id$v=19$SALTSALTSALTSALT$DIGESTDIGESTDIGEST",
		// field の数は6でも、区切りが1つずれて parts[3] に salt が来る形
		"$argon2id$v=19$c2FsdHNhbHRzYWx0c2Ex$ZGlnZXN0ZGlnZXN0$extra",
		// version の欄に値以外が入る形
		"$argon2id$v=c2FsdHNhbHRzYWx0$m=65536,t=3,p=4$c2FsdA$ZGlnZXN0",
		// パラメータの欄の値に数字以外が入る形
		"$argon2id$v=19$m=c2FsdHNhbHRzYWx0$c2FsdA$ZGlnZXN0",
		// パラメータの鍵が英小文字でない形、鍵や値が空の形、version の値が空の形
		"$argon2id$v=19$M=65536$c2FsdA$ZGlnZXN0",
		"$argon2id$v=19$=1$c2FsdA$ZGlnZXN0",
		"$argon2id$v=19$m=$c2FsdA$ZGlnZXN0",
		"$argon2id$v=$m=1$c2FsdA$ZGlnZXN0",
		"$argon2id$v=19$$c2FsdA$ZGlnZXN0",
	} {
		got := ProfileForLog(in)
		if !strings.Contains(got, "unexpected field layout") {
			t.Fatalf("ProfileForLog(%q)=%q, want unexpected-layout marker", in, got)
		}
		for _, f := range strings.Split(in, "$")[2:] {
			if f != "" && strings.Contains(got, f) {
				t.Fatalf("ProfileForLog(%q)=%q に field %q が漏れている", in, got, f)
			}
		}
	}
}

// Scheme.String は全ての値を読める形にする (#2849)。
//
// **ログのためだけに存在する。** slog の JSONHandler は fmt.Stringer を使わない
// ので呼び出し側が明示的に通す必要があり、ここが壊れると `"scheme":2` のような
// 数字がログに出て診断の役に立たなくなる。
func TestScheme_String(t *testing.T) {
	for _, tc := range []struct {
		in   Scheme
		want string
	}{
		{SchemeBcrypt, "bcrypt"},
		{SchemeArgon2id, "argon2id"},
		{SchemeUnknown, "unknown"},
		{Scheme(99), "unknown"},
	} {
		if got := tc.in.String(); got != tc.want {
			t.Fatalf("Scheme(%d).String()=%q, want %q", tc.in, got, tc.want)
		}
	}
}

// base64 は **Strict** で読む (#2850)。
//
// 非 canonical な末尾 bit を含む base64 を通すと、同じ digest を指す表現が
// 複数できる。実害は小さい (stored は DB 由来) が、明示的に書いてある制約なので
// 変異で落ちる形にしておく。
func TestVerify_RejectsNonCanonicalBase64(t *testing.T) {
	valid := cherryPickFixture("hunter2")
	parts := strings.Split(valid, "$")

	// 末尾に非 canonical な bit を立てた salt / digest を作る。
	nonCanonical := func(std string) string {
		raw, err := base64.RawStdEncoding.DecodeString(std)
		if err != nil {
			t.Fatal(err)
		}
		enc := []byte(base64.RawStdEncoding.EncodeToString(raw))
		// 最終文字を、同じ byte 列へ復号されるが canonical でない文字へ差し替える。
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
		for _, c := range alphabet {
			cand := string(append(append([]byte{}, enc[:len(enc)-1]...), byte(c)))
			if cand == std {
				continue
			}
			if got, err := base64.RawStdEncoding.DecodeString(cand); err == nil && string(got) == string(raw) {
				return cand
			}
		}
		// **Skip にしない。** skip は go test を ok のまま通すので、
		// .Strict() を見る唯一のテストが黙って消える。
		t.Fatalf("非 canonical 表現を作れない (len=%d)", len(raw))
		return ""
	}

	// **salt と digest の両方を見る。** 片方だけだと、もう片方の .Strict() を
	// 外す変異が素通りする (実測)。
	badSalt := nonCanonical(parts[4])
	saltHash := strings.Join([]string{"", parts[1], parts[2], parts[3], badSalt, parts[5]}, "$")
	if _, out := Verify(context.Background(), saltHash, "hunter2"); out != OutcomeUnsupported {
		t.Fatalf("Verify(non-canonical salt)=%v, want %v", out, OutcomeUnsupported)
	}

	badDigest := nonCanonical(parts[5])
	digestHash := strings.Join([]string{"", parts[1], parts[2], parts[3], parts[4], badDigest}, "$")
	if _, out := Verify(context.Background(), digestHash, "hunter2"); out != OutcomeUnsupported {
		t.Fatalf("Verify(non-canonical digest)=%v, want %v", out, OutcomeUnsupported)
	}
}
