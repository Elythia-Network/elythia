package db

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	stdlog "log"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const secretToken = "CANARY-TOKEN-must-not-be-logged"

func loggerWithBuf(t *testing.T, paramLog bool) (logger.Interface, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	cfg := &config.Config{}
	if paramLog {
		cfg.Logging = &config.LoggingOptions{SQL: &config.SQLLoggingOptions{EnableQueryParamLog: true}}
	}
	return newGormLoggerTo(cfg, stdlog.New(&buf, "", 0)), &buf
}

// 既定では not-found のときに SQL を出さないこと。
//
// **認証経路がここを必ず通る。** `auth.Authenticate` はまず native token として
// 引くので、アプリ / OAuth / MiAuth のトークンでは毎回 not-found を経る。
func TestGormLogger_DoesNotLogSQLOnRecordNotFound(t *testing.T) {
	t.Parallel()

	lg, buf := loggerWithBuf(t, false)
	fcCalled := false
	lg.Trace(context.Background(), time.Now(), func() (string, int64) {
		fcCalled = true
		return `SELECT * FROM "user" WHERE token = '` + secretToken + `'`, 0
	}, gorm.ErrRecordNotFound)

	require.False(t, fcCalled, "not-found では SQL を組み立てさえしないこと")
	require.NotContains(t, buf.String(), secretToken)
	require.Empty(t, strings.TrimSpace(buf.String()), "出力そのものが無いこと")
}

// not-found 以外のエラーでは従来どおり記録すること (障害を握り潰さない)。
func TestGormLogger_StillLogsOtherErrors(t *testing.T) {
	t.Parallel()

	lg, buf := loggerWithBuf(t, false)
	lg.Trace(context.Background(), time.Now(), func() (string, int64) {
		return `SELECT 1`, 0
	}, context.DeadlineExceeded)

	require.Contains(t, buf.String(), "SELECT 1",
		"DB 障害は引き続き記録する — not-found だけを黙らせる変更である")
}

// 既定ではバインド値を SQL へ展開しないこと。
func TestGormLogger_DoesNotExpandBindVariablesByDefault(t *testing.T) {
	t.Parallel()

	lg, _ := loggerWithBuf(t, false)
	f, ok := lg.(gorm.ParamsFilter)
	require.True(t, ok, "logger は ParamsFilter を実装していること (これが無いと値が埋まる)")

	sql, params := f.ParamsFilter(context.Background(),
		`SELECT * FROM "user" WHERE token = ?`, secretToken)
	require.Equal(t, `SELECT * FROM "user" WHERE token = ?`, sql)
	require.Nil(t, params, "バインド値を渡さないこと (Explain が展開してしまう)")
}

// 運用者が明示的に有効にしたときだけ展開すること。
func TestGormLogger_ExpandsBindVariablesWhenExplicitlyEnabled(t *testing.T) {
	t.Parallel()

	lg, _ := loggerWithBuf(t, true)
	f, ok := lg.(gorm.ParamsFilter)
	require.True(t, ok)

	_, params := f.ParamsFilter(context.Background(),
		`SELECT * FROM "user" WHERE token = ?`, secretToken)
	require.Equal(t, []any{secretToken}, params,
		"enableQueryParamLogging を真にしたときは従来どおり展開する")
}

// gorm.Open へ渡す Logger が newGormLogger であること。
//
// **これが無いと差し戻しを検出できない。** `logger.Default` に戻しても
// newGormLogger 自身のテストは緑のまま通る (呼ばれなくなるだけなので)。
func TestGormOpenUsesHardenedLogger(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "db.go", nil, 0)
	require.NoError(t, err)

	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Config" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "gorm" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok || key.Name != "Logger" {
				continue
			}
			call, ok := kv.Value.(*ast.CallExpr)
			require.True(t, ok,
				"gorm.Config.Logger は newGormLogger(cfg) の呼び出しであること "+
					"(logger.Default を直接渡すと not-found とバインド値がログに出る)")
			fn, ok := call.Fun.(*ast.Ident)
			require.True(t, ok)
			require.Equal(t, "newGormLogger", fn.Name,
				"gorm.Config.Logger に渡すのは newGormLogger の結果であること")
			found = true
		}
		return true
	})
	require.True(t, found, "gorm.Config の Logger フィールドを見つけられなかった "+
		"(配線の形が変わったならこの検査を直すこと)")
}
