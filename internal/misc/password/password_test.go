package password

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// mustHasher returns the Hasher for c, failing the test when c is rejected.
func mustHasher(t *testing.T, c int) Hasher {
	t.Helper()
	h, err := NewHasher(c)
	if err != nil {
		t.Fatalf("NewHasher(%d): %v", c, err)
	}
	return h
}

func TestDefaultCostIsAboveUpstream(t *testing.T) {
	// upstream Misskey は全経路で cost 8。**既定でそこを下回らせない。**
	if DefaultCost <= 8 {
		t.Fatalf("DefaultCost=%d は upstream の 8 以下", DefaultCost)
	}
	// 零値は既定のcostを使う。配線していない部品(テスト)がcost 0で落ちないように。
	if got := (Hasher{}).Cost(); got != DefaultCost {
		t.Fatalf("零値のcostが%d, want %d", got, DefaultCost)
	}
}

func TestNewHasher_RejectsOutOfRange(t *testing.T) {
	for _, c := range []int{MinCost - 1, MaxCost + 1, -1, 0, 100} {
		if _, err := NewHasher(c); err == nil {
			t.Errorf("NewHasher(%d) がエラーにならない", c)
		}
	}
}

func TestNewHasher_AcceptsRange(t *testing.T) {
	for _, c := range []int{MinCost, DefaultCost, 12, MaxCost} {
		if got := mustHasher(t, c).Cost(); got != c {
			t.Fatalf("Cost()=%d, want %d", got, c)
		}
	}
}

// 設定から作るとき、0は未設定として既定に、範囲外は警告して既定に落とす。
// **範囲外で起動を失敗させない。** Configを構造体リテラルで組む呼び出し元が
// 全部壊れる。
func TestHasherFromConfig(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	tests := []struct {
		name     string
		cost     int
		want     int
		wantWarn bool
	}{
		{name: "unset selects the default", cost: 0, want: DefaultCost},
		{name: "in range is kept", cost: MinCost + 1, want: MinCost + 1},
		{name: "upper bound is kept", cost: MaxCost, want: MaxCost},
		{name: "below the range falls back with a warning", cost: MinCost - 1, want: DefaultCost, wantWarn: true},
		{name: "above the range falls back with a warning", cost: MaxCost + 1, want: DefaultCost, wantWarn: true},
		{name: "negative falls back with a warning", cost: -1, want: DefaultCost, wantWarn: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			buf.Reset()
			if got := HasherFromConfig(tt.cost).Cost(); got != tt.want {
				t.Fatalf("cost=%d, want %d", got, tt.want)
			}
			warned := strings.Contains(buf.String(), "bcrypt cost が不正なので既定値を使います")
			if warned != tt.wantWarn {
				t.Fatalf("警告の有無が%t, want %t (log: %q)", warned, tt.wantWarn, buf.String())
			}
		})
	}
}

// Hash が実際に設定した cost を使っていること。**ここが効いていないと、
// 設定を足しただけで何も変わらない。**
func TestHash_UsesConfiguredCost(t *testing.T) {
	for _, c := range []int{MinCost, MinCost + 2} {
		h, err := mustHasher(t, c).Hash("hunter2")
		if err != nil {
			t.Fatal(err)
		}
		got, err := bcrypt.Cost([]byte(h))
		if err != nil {
			t.Fatal(err)
		}
		if got != c {
			t.Errorf("hash の cost=%d, want %d", got, c)
		}
		if err := bcrypt.CompareHashAndPassword([]byte(h), []byte("hunter2")); err != nil {
			t.Errorf("自分で作ったハッシュを検証できない: %v", err)
		}
	}
}

// 零値はDefaultCostでハッシュを作る。
func TestHash_ZeroValueUsesDefaultCost(t *testing.T) {
	h, err := Hasher{}.Hash("hunter2")
	if err != nil {
		t.Fatal(err)
	}
	got, err := bcrypt.Cost([]byte(h))
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultCost {
		t.Fatalf("hash の cost=%d, want %d", got, DefaultCost)
	}
}

// 73 byte 以上は bcrypt のエラーをそのまま返す。呼び出し側が errors.Is で
// 拾って 400 に変換しているので、包んで別物にしない (#1075)。
func TestHash_TooLongPassthrough(t *testing.T) {
	_, err := mustHasher(t, MinCost).Hash(strings.Repeat("a", 73))
	if !errors.Is(err, bcrypt.ErrPasswordTooLong) {
		t.Fatalf("bcrypt.ErrPasswordTooLong のはずが %v", err)
	}
}

func TestNeedsRehash(t *testing.T) {
	hasher := mustHasher(t, MinCost+2)
	weak, err := bcrypt.GenerateFromPassword([]byte("hunter2"), MinCost)
	if err != nil {
		t.Fatal(err)
	}
	current, err := hasher.Hash("hunter2")
	if err != nil {
		t.Fatal(err)
	}

	if !hasher.NeedsRehash(string(weak)) {
		t.Error("設定より弱いハッシュが焼き直し対象にならない")
	}
	if hasher.NeedsRehash(current) {
		t.Error("設定どおりのハッシュが焼き直し対象になっている")
	}

	// 設定より強いものは触らない。運用者が cost を下げたときに、既存の
	// 強いハッシュをわざわざ弱くする必要は無い。
	strong, err := bcrypt.GenerateFromPassword([]byte("hunter2"), MinCost+4)
	if err != nil {
		t.Fatal(err)
	}
	if hasher.NeedsRehash(string(strong)) {
		t.Error("設定より強いハッシュを焼き直そうとしている")
	}

	// **読めないものは触らない。** 「弱い」と判断して上書きすると、検証できない
	// まま別形式へ潰しかねない。
	for _, bad := range []string{"", "not-a-hash", "$2a$", "plaintext"} {
		if hasher.NeedsRehash(bad) {
			t.Errorf("読めないハッシュ %q を焼き直し対象にしている", bad)
		}
	}
}

// 零値はDefaultCostと比べる。既定より弱いハッシュだけを焼き直す。
func TestNeedsRehash_ZeroValueComparesWithDefaultCost(t *testing.T) {
	below, err := bcrypt.GenerateFromPassword([]byte("hunter2"), DefaultCost-1)
	if err != nil {
		t.Fatal(err)
	}
	if !(Hasher{}).NeedsRehash(string(below)) {
		t.Error("既定より弱いハッシュが焼き直し対象にならない")
	}
	at, err := bcrypt.GenerateFromPassword([]byte("hunter2"), DefaultCost)
	if err != nil {
		t.Fatal(err)
	}
	if (Hasher{}).NeedsRehash(string(at)) {
		t.Error("既定どおりのハッシュが焼き直し対象になっている")
	}
}
