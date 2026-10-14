package backup

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pwnedMarker is the file a hostile dump tries to create in the throwaway
// server's host (the container).
const pwnedMarker = "/tmp/zq-verify-pwned"

// TestVerifyRestoresAsUnprivilegedRole: the dump is restored by a role that
// is not a superuser, so SQL in the dump cannot run programs on the host.
//
// 保存先に書ける者は、制約から呼ばれる関数に `COPY ... TO PROGRAM` を仕込んだ dump を
// 置ける。superuser で戻すとバックアップ用のサービスのコンテナでコードが動き、daemon の
// 起動時の自動の検証だけで届いた。関数は元の DB では何もしない (元の DB の名前で
// 分ける) ので、dump を作るときには動かない。
func TestVerifyRestoresAsUnprivilegedRole(t *testing.T) {
	e := getVerifyEnv(t)
	p := newDatabase(t)
	gdb, err := migrateDatabase(p.url, e.bundled)
	require.NoError(t, err)
	closeGorm(gdb)
	_, err = e.sandbox.sh(context.Background(), `rm -f "$1"`, pwnedMarker)
	require.NoError(t, err)
	p.exec(t,
		fmt.Sprintf(`CREATE FUNCTION zq_check(int) RETURNS bool LANGUAGE plpgsql AS $f$
BEGIN
	IF current_database() <> '%s' THEN
		EXECUTE 'COPY (SELECT 1) TO PROGRAM ''touch %s''';
	END IF;
	RETURN true;
END
$f$`, p.db, pwnedMarker),
		`CREATE TABLE zq_hostile (a int CHECK (zq_check(a)))`,
		`INSERT INTO zq_hostile VALUES (1)`,
	)

	ctx := context.Background()
	st := newMemStorage()
	meta, err := Take(ctx, p.takeOptions(st))
	require.NoError(t, err)

	res, err := Verify(ctx, st, meta.ID, e.opts())
	require.NoError(t, err, "権限の不足は世代の欠陥として判定する (環境の誤りではない)")
	assert.False(t, res.OK)
	require.Len(t, res.Stages, 3)
	assert.True(t, res.Stages[0].OK, "readable: %s", res.Stages[0].Error)
	assert.False(t, res.Stages[1].OK, "restorable を通した: dump の SQL がサーバー側でプログラムを動かせる")

	out, err := e.sandbox.sh(ctx, `if [ -e "$1" ]; then echo present; else echo absent; fi`, pwnedMarker)
	require.NoError(t, err)
	assert.Equal(t, "absent", strings.TrimSpace(out), "dump の SQL が使い捨てのサーバーのホストでプログラムを動かした")
	e.sandbox.assertNoLeftovers(t)
}

// TestVerifyPreinstallsExtensionsForTheRole: an extension the restore role may
// not create (not trusted) is installed beforehand and handed to the role, so
// the dump's CREATE EXTENSION and COMMENT ON EXTENSION pass.
func TestVerifyPreinstallsExtensionsForTheRole(t *testing.T) {
	e := getVerifyEnv(t)
	p := newDatabase(t)
	gdb, err := migrateDatabase(p.url, e.bundled)
	require.NoError(t, err)
	closeGorm(gdb)
	// pg_buffercache は trusted ではない (superuser でないと作れない) 拡張で、
	// postgres の image に入っている。
	p.exec(t, `CREATE EXTENSION pg_buffercache`)

	ctx := context.Background()
	st := newMemStorage()
	meta, err := Take(ctx, p.takeOptions(st))
	require.NoError(t, err)
	require.True(t, slices.ContainsFunc(meta.Extensions, func(x ExtensionInfo) bool {
		return x.Name == "pg_buffercache" && x.Schema == "public"
	}), "%+v", meta.Extensions)

	res, err := Verify(ctx, st, meta.ID, e.opts())
	require.NoError(t, err)
	for _, s := range res.Stages {
		assert.True(t, s.OK, "%s: %s", s.Stage, s.Error)
	}
	assert.True(t, res.OK)
	e.sandbox.assertNoLeftovers(t)
}
