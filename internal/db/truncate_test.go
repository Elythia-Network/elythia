package db

import (
	"bytes"
	stdlog "log"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/stretchr/testify/require"
)

// **長い SQL を切り詰めること (既定)。**
//
// `logging.sql.disableQueryTruncation` は宣言と env バインドだけがあって
// どこからも読まれておらず、設定しても何も起きなかった (dead config)。
// upstream は既定 100 文字に切り詰め、このフラグで無効化できる。
func TestGormLogger_TruncatesLongSQLByDefault(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	lg := newGormLoggerTo(&config.Config{}, stdlog.New(&buf, "", 0))

	long := "SELECT " + strings.Repeat("x", 500)
	lg.Warn(t.Context(), "%s", long)

	out := buf.String()
	require.Contains(t, out, "...", "切り詰めた印が付くこと")
	require.NotContains(t, out, strings.Repeat("x", 200), "全文は出さないこと")
}

// フラグを立てたら従来どおり全文を出すこと。
func TestGormLogger_KeepsFullSQLWhenTruncationDisabled(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	cfg := &config.Config{Logging: &config.LoggingOptions{
		SQL: &config.SQLLoggingOptions{DisableQueryTruncation: true},
	}}
	lg := newGormLoggerTo(cfg, stdlog.New(&buf, "", 0))

	long := "SELECT " + strings.Repeat("x", 500)
	lg.Warn(t.Context(), "%s", long)

	require.Contains(t, buf.String(), strings.Repeat("x", 400),
		"disableQueryTruncation を立てたら全文を出すこと")
}
