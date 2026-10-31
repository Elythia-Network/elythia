package id

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestAidxCutoffPrefix_ClampsUpperTimestamp(t *testing.T) {
	max := time.UnixMilli(time2000 + testAIDMaxOffsetMillis)
	assert.Equal(t, "zzzzzzzz00000000", AidxCutoffPrefix(max))
	assert.Equal(t, AidxCutoffPrefix(max), AidxCutoffPrefix(max.Add(time.Millisecond)))
}

func TestNormalizeCursor_BothEmptyReturnsEmpty(t *testing.T) {
	s, u, ok := NormalizeCursor("", "", nil, nil)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.Empty(t, s)
	assert.Empty(t, u)
}

func TestNormalizeCursor_SinceIDPassesThrough(t *testing.T) {
	s, u, ok := NormalizeCursor("abc123def456", "", nil, nil)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.Equal(t, "abc123def456", s)
	assert.Empty(t, u)
}

func TestNormalizeCursor_UntilIDPassesThrough(t *testing.T) {
	s, u, ok := NormalizeCursor("", "xyz789", nil, nil)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.Empty(t, s)
	assert.Equal(t, "xyz789", u)
}

func TestNormalizeCursor_SinceDateConvertedToAidxPrefix(t *testing.T) {
	// 2026-05-22T00:00:00Z (= 既知の Unix ms)。AidxCutoffPrefix と同じ結果を期待。
	dateMs := int64(1779744000000)
	s, u, ok := NormalizeCursor("", "", &dateMs, nil)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.NotEmpty(t, s, "sinceDate を渡したら sinceID が aidx prefix に変換される")
	assert.Len(t, s, 16, "aidx prefix は 16 文字")
	assert.Empty(t, u)
}

func TestNormalizeCursor_UntilDateConvertedToAidxPrefix(t *testing.T) {
	dateMs := int64(1779744000000)
	s, u, ok := NormalizeCursor("", "", nil, &dateMs)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.Empty(t, s)
	assert.NotEmpty(t, u)
	assert.Len(t, u, 16)
}

func TestNormalizeCursor_SinceIDOverridesSinceDate(t *testing.T) {
	// upstream 仕様: sinceID が指定されていれば sinceDate は無視。
	dateMs := int64(1779744000000)
	s, _, ok := NormalizeCursor("explicit-id", "", &dateMs, nil)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.Equal(t, "explicit-id", s, "sinceID 優先、sinceDate は ignore")
}

func TestNormalizeCursor_BothDatesProduceDifferentPrefixes(t *testing.T) {
	since := int64(1779744000000) // 2026-05-22T00:00:00Z
	until := int64(1779830400000) // 2026-05-23T00:00:00Z (+24h)
	s, u, ok := NormalizeCursor("", "", &since, &until)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.NotEmpty(t, s)
	assert.NotEmpty(t, u)
	assert.NotEqual(t, s, u, "異なる timestamp は異なる prefix を生成")
}

func TestNormalizeCursor_AidxPrefixSuffixIsZero(t *testing.T) {
	// AidxCutoffPrefix の仕様: 後半 8 文字は "00000000" で揃う (= 最小 ID
	// prefix で `id > prefix` の SQL で同 msec の全 ID を含む)。
	dateMs := int64(1779744000000)
	s, _, ok := NormalizeCursor("", "", &dateMs, nil)
	assert.True(t, ok, "列に入るカーソルを弾いている")
	assert.Equal(t, "00000000", s[8:], "aidx prefix 後半 8 文字は counter 最小値")
}

// **NUL を含むカーソルは ok=false (#3025)。** そのまま `id < ?` の bind
// parameter に載せると PostgreSQL がその時点で落とす (本番の pgx extended
// protocol で SQLSTATE 22021) ので、認証済みの一般利用者がパラメータ 1 文字で
// 500 を起こせていた。
func TestNormalizeCursor_RejectsUnstorableCursor(t *testing.T) {
	for _, tt := range []struct {
		name    string
		sinceID string
		untilID string
	}{
		{"sinceId に NUL", "a\x00b", ""},
		{"untilId に NUL", "", "a\x00b"},
		{"両方に NUL", "\x00", "\x00"},
		{"NUL だけ", "\x00", ""},
		{"末尾の NUL", "abc\x00", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, u, ok := NormalizeCursor(tt.sinceID, tt.untilID, nil, nil)
			assert.False(t, ok, "列に入らないカーソルを通している (SELECT がそこで落ちる)")
			// **返す値も空にする。** ok を無視した呼び出し側が NUL を DB へ
			// 渡さないようにするための保険で、「先頭から返してよい」ではない。
			assert.Empty(t, s)
			assert.Empty(t, u)
		})
	}
}

// date から作った prefix は NUL を含みようがないので、date 経路は落とさない。
func TestNormalizeCursor_DateCursorStaysStorable(t *testing.T) {
	dateMs := int64(1779744000000)
	s, u, ok := NormalizeCursor("", "", &dateMs, &dateMs)
	assert.True(t, ok)
	assert.NotEmpty(t, s)
	assert.NotEmpty(t, u)
}

// 本家は sinceDate / untilDate を真偽値として見るので、0 (JSON の -0 も int64 では
// 0) は指定なしと同じ。時刻 0 のカーソルにすると untilDate: 0 が空の一覧になる。
func TestNormalizeCursor_ZeroDateIsAbsent(t *testing.T) {
	zero := int64(0)
	s, u, ok := NormalizeCursor("", "", &zero, nil)
	assert.True(t, ok)
	assert.Empty(t, s, "sinceDate: 0 must not become a cursor")
	assert.Empty(t, u)

	s, u, ok = NormalizeCursor("", "", nil, &zero)
	assert.True(t, ok)
	assert.Empty(t, s)
	assert.Empty(t, u, "untilDate: 0 must not become a cursor")

	// 負の日付は真なので、本家と同じくカーソルにする
	neg := int64(-1)
	s, u, ok = NormalizeCursor("", "", &neg, &neg)
	assert.True(t, ok)
	assert.NotEmpty(t, s)
	assert.NotEmpty(t, u)
}
