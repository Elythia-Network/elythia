package backup

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 保存・送信するエラーの文面には、戻している行の値を出さないこと。
//
// verify.json (dump を暗号化していても平文)、リストアの journal (Redis)、失敗の
// 通知 (Webhook) に入る。DETAIL / CONTEXT / "Command was:" (続く SQL の行も) と、
// ERROR の行の引用符の中に値が出る。
func TestRedactStderr(t *testing.T) {
	stderr := strings.Join([]string{
		`pg_restore: error: COPY failed for table "user_keypair": ERROR:  invalid input syntax for type integer: "SECRET-IN-ERROR"`,
		`CONTEXT:  COPY user_keypair, line 1, column privateKey: "SECRET-IN-CONTEXT"`,
		`DETAIL:  Key (token)=(SECRET-IN-DETAIL) already exists.`,
		`pg_restore: error: could not execute query: ERROR:  permission denied for function zq`,
		`Command was: CREATE FUNCTION zq() RETURNS void AS $$`,
		`  SELECT 'SECRET-IN-COMMAND';`,
		`$$ LANGUAGE sql;`,
		`HINT:  SECRET-IN-HINT`,
		`pg_restore: warning: errors ignored on restore: 2`,
	}, "\n")
	got := redactStderr(stderr)
	assert.NotContains(t, got, "SECRET", "行の値を出した: %s", got)
	// プログラムとサーバーの主な文言は残る (原因が分かるように)。
	assert.Contains(t, got, "invalid input syntax for type integer")
	assert.Contains(t, got, "permission denied for function zq")
	assert.Contains(t, got, "errors ignored on restore: 2")

	// ProgramError の文面も同じ形にする。全文は Stderr に残る。
	pe := &ProgramError{Program: "pg_restore", ExitCode: 1, Stderr: stderr}
	assert.NotContains(t, pe.Error(), "SECRET")
	assert.Contains(t, pe.Stderr, "SECRET-IN-CONTEXT")

	// 長い出力は上限で切る。
	long := strings.Repeat("ERROR:  "+strings.Repeat("x", 100)+"\n", 200)
	assert.LessOrEqual(t, len(redactStderr(long)), redactStderrMax+3)
}

// verify の段の失敗 (verify.json に残る) にも値を出さないこと。
func TestProgramVerdictStageMessageIsRedacted(t *testing.T) {
	pe := &ProgramError{Program: "pg_restore", ExitCode: 1, Stderr: `pg_restore: error: COPY failed for table "t": ERROR:  invalid input syntax for type uuid: "SECRET-VALUE"` + "\n" + `CONTEXT:  COPY t, line 3: "SECRET-ROW"`}
	err := programVerdict(context.Background(), "pg_restore", pe)
	var se *stageError
	require.True(t, errors.As(err, &se), "世代の欠陥として扱う: %v", err)
	assert.NotContains(t, se.msg, "SECRET")
	assert.Contains(t, se.msg, "invalid input syntax for type uuid")
}

// 長い標準エラーは先頭と末尾を残すこと (先頭に最初のエラーの行がある)。
func TestClipLogged(t *testing.T) {
	short := "pg_restore: error: x"
	assert.Equal(t, short, clipLogged(short))
	long := "pg_restore: error: could not execute query\n" + strings.Repeat("y", 40<<10) + "\nTAIL"
	got := clipLogged(long)
	assert.True(t, strings.HasPrefix(got, "pg_restore: error: could not execute query"), "先頭のエラーの行を落とした")
	assert.True(t, strings.HasSuffix(got, "TAIL"))
	assert.Less(t, len(got), 17<<10)
}

// 値の中に引用符があっても、値の中身を外へ出す文面に残さないこと
// (PostgreSQL は値の中の `"` を逃がさない)。識別子の後ろのエラーの本文は残す。
func TestRedactStderr_ValueWithQuotes(t *testing.T) {
	got := redactStderr(`pg_restore: error: COPY failed for table "user_keypair": ERROR:  invalid input syntax for type integer: "a"SECRET"b"`)
	assert.NotContains(t, got, "SECRET")
	assert.NotContains(t, got, "user_keypair")
	assert.Contains(t, got, "invalid input syntax for type integer")
}

// 失敗した SQL を載せる detail / hint の行は外へ出す文面に残さないこと
// (PG15 以降の pg_restore は `pg_restore: detail: Command was: ...` で出す)。
func TestRedactStderr_DropsDetailLines(t *testing.T) {
	got := redactStderr("pg_restore: error: could not execute query: ERROR:  permission denied\npg_restore: detail: Command was: CREATE FUNCTION public.zq() RETURNS int AS 'SECRETBODY'\npg_dump: hint: SECRETHINT")
	assert.NotContains(t, got, "SECRETBODY")
	assert.NotContains(t, got, "SECRETHINT")
	assert.Contains(t, got, "permission denied")
}
