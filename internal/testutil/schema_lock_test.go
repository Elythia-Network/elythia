package testutil

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// 同じ DB で同じ schema を別のプロセスが使っているときは、開いた時点で原因を
// 名指しして落とす (#3369)。別のプロセスの代わりに、別の接続で先に鍵を取る。
func TestLockSchema_RefusesASchemaHeldElsewhere(t *testing.T) {
	schema := fmt.Sprintf("internal_testutil_lockprobe_%d", time.Now().UnixNano())

	held, err := lockSchema(schema)
	require.NoError(t, err, "test DB unavailable")

	_, err = lockSchema(schema)
	assert.ErrorIs(t, err, errSchemaInUse, "another holder must be refused")
	assert.Contains(t, err.Error(), "TEST_DB_NAME")

	// 別の schema の鍵は独立している。
	other, err := lockSchema(schema + "_other")
	require.NoError(t, err)
	require.NoError(t, other.Close())

	// 持っていた側が終われば (接続が閉じれば) 取れる。
	//
	// **すぐに取れるとは限らないので、短い間だけ繰り返す。** 接続を閉じても、
	// PostgreSQL がその session の advisory lock を外すのは backend が終わった後で、
	// 直後に取り直すと間に合わずに落ちた (CI で 1 回。#3369)。
	require.NoError(t, held.Close())
	// assert.Eventually は条件を別の goroutine で走らせ、期限が切れると待たずに返すので、
	// 外の変数へ代入すると失敗の経路で競合する。同じ goroutine で繰り返す。
	var again *lockSession
	var lastErr error
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if again, lastErr = lockSchema(schema); lastErr == nil || time.Now().After(deadline) {
			break
		}
	}
	require.NoError(t, lastErr, "the lock was not released after the holder closed")
	require.NoError(t, again.Close())
}

// 同じプロセスの中では何度開いても通る (パッケージの中で OpenTestDB を何度も
// 呼ぶのは普通の使い方)。
func TestClaimSchema_SameProcessIsIdempotent(t *testing.T) {
	schema := fmt.Sprintf("internal_testutil_claimprobe_%d", time.Now().UnixNano())
	require.NoError(t, claimSchema(schema), "test DB unavailable")
	t.Cleanup(func() { releaseClaim(t, schema) })
	require.NoError(t, claimSchema(schema))

	// このプロセスが持っているので、別の接続からは取れない。
	_, err := lockSchema(schema)
	assert.ErrorIs(t, err, errSchemaInUse)
}

// 鍵はプロセスで 1 本の接続に全部載る。schema ごとに接続を張ると、兄弟 schema を
// 多く使うパッケージで接続が増え続ける (#3369 のレビュー2周目)。
func TestClaimSchema_AllLocksShareOneSession(t *testing.T) {
	base := fmt.Sprintf("internal_testutil_sessionprobe_%d", time.Now().UnixNano())
	schemas := []string{base + "_a", base + "_b", base + "_c"}
	for _, s := range schemas {
		require.NoError(t, claimSchema(s), "test DB unavailable")
		t.Cleanup(func() { releaseClaim(t, s) })
	}

	db := observerDB(t)
	pids := map[int64]bool{}
	for _, s := range schemas {
		holders := lockHolders(t, db, s)
		require.Len(t, holders, 1, "schema %s must be held exactly once", s)
		pids[holders[0]] = true
	}
	assert.Len(t, pids, 1, "every schema lock must sit on the one lock session of the process")
}

// 鍵のセッションが切れたら (PostgreSQL の再起動、管理者の切断)、次に開くときに
// 張り直して、持っていた鍵を取り直す。張り直さないと、以後に開く schema は全部
// `driver: bad connection` で開けず、持っていたはずの鍵も黙って外れたまま
// (#3369 のレビュー3周目)。
func TestClaimSchema_RelocksAfterTheSessionIsCut(t *testing.T) {
	base := fmt.Sprintf("internal_testutil_cutprobe_%d", time.Now().UnixNano())
	held, next := base+"_held", base+"_next"
	require.NoError(t, claimSchema(held), "test DB unavailable")
	t.Cleanup(func() { releaseClaim(t, held) })

	db := observerDB(t)
	holders := lockHolders(t, db, held)
	require.Len(t, holders, 1)
	var cut bool
	require.NoError(t, db.Raw(`SELECT pg_terminate_backend(?)`, holders[0]).Scan(&cut).Error)
	require.True(t, cut)
	require.Eventually(t, func() bool { return len(lockHolders(t, db, held)) == 0 },
		5*time.Second, 20*time.Millisecond, "the lock must go away with the session")

	require.NoError(t, claimSchema(next), "opening another schema must survive a cut session")
	t.Cleanup(func() { releaseClaim(t, next) })
	_, err := lockSchema(held)
	assert.ErrorIs(t, err, errSchemaInUse, "the lock held before the cut must be taken again")
	_, err = lockSchema(next)
	assert.ErrorIs(t, err, errSchemaInUse)
}

// 同じパッケージの中で同時に開いても (init と並行のテスト、t.Parallel)、
// セッションは 1 本で、自分の鍵に当たらない。排他を外すとセッションが 2 本
// 張られ、自分が持つ鍵で errSchemaInUse になる。
func TestClaimSchema_ConcurrentClaims(t *testing.T) {
	base := fmt.Sprintf("internal_testutil_concprobe_%d", time.Now().UnixNano())
	var names []string
	for i := 0; i < 4; i++ {
		names = append(names, fmt.Sprintf("%s_%d", base, i))
	}
	t.Cleanup(func() {
		for _, n := range names {
			releaseClaim(t, n)
		}
	})
	errs := make(chan error, 64)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			errs <- claimSchema(n)
		}(names[i%len(names)])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	db := observerDB(t)
	pids := map[int64]bool{}
	for _, n := range names {
		holders := lockHolders(t, db, n)
		require.Len(t, holders, 1, "schema %s must be held exactly once", n)
		pids[holders[0]] = true
	}
	assert.Len(t, pids, 1)
}

// observerDB opens a connection outside every lock session.
func observerDB(t *testing.T) *gorm.DB {
	t.Helper()
	loadEnvFile()
	db, err := openDSN(testDSN(""))
	require.NoError(t, err, "test DB unavailable")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// lockHolders returns the backends that hold the lock of schema.
//
// 64 bit の鍵は pg_locks では classid (上位 32 bit) と objid (下位) に分かれて
// 載る (objsubid = 1 が 1 引数の形)。
func lockHolders(t *testing.T, db *gorm.DB, schema string) []int64 {
	t.Helper()
	var holders []int64
	require.NoError(t, db.Raw(`
		SELECT pid FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND objsubid = 1
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
			AND ((classid::bigint << 32) | objid::bigint) = hashtextextended(?, 0)`,
		schemaLockKey(schema)).Scan(&holders).Error)
	return holders
}

// releaseClaim drops a claim made by a test, so the probe schemas do not stay
// locked until the process exits.
func releaseClaim(t *testing.T, schema string) {
	t.Helper()
	processLocks.mu.Lock()
	defer processLocks.mu.Unlock()
	if !processLocks.held[schema] {
		return
	}
	var ok bool
	require.NoError(t, processLocks.session.conn.QueryRowContext(context.Background(),
		`SELECT pg_advisory_unlock(hashtextextended($1, 0))`, schemaLockKey(schema)).Scan(&ok))
	require.True(t, ok)
	delete(processLocks.held, schema)
}

// OpenTestDB を通った後は、このパッケージの schema の鍵を持っている。
func TestOpenTestDB_HoldsThePackageSchema(t *testing.T) {
	db, err := OpenTestDB()
	require.NoError(t, err, "test DB unavailable")
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })

	_, err = lockSchema("internal_testutil")
	assert.ErrorIs(t, err, errSchemaInUse, "OpenTestDB must keep the package schema locked")
}

// 開く入口では panic する。error で返すと、呼び出し側が「DB が無い」として
// skip し、2 本目のプロセスが黙って緑になる (#3369 で実測)。
func TestOpenTestDBForPackage_PanicsWhenTheSchemaIsHeldElsewhere(t *testing.T) {
	schema := fmt.Sprintf("internal_testutil_openprobe_%d", time.Now().UnixNano())
	held, err := lockSchema(schema)
	require.NoError(t, err, "test DB unavailable")
	t.Cleanup(func() { _ = held.Close() })
	t.Cleanup(func() {
		if db, err := openDSN(testDSN("")); err == nil {
			_ = db.Exec(`DROP SCHEMA IF EXISTS "` + schema + `" CASCADE`).Error
			if sqlDB, err := db.DB(); err == nil {
				_ = sqlDB.Close()
			}
		}
	})

	assert.PanicsWithError(t, (func() string {
		_, err := lockSchema(schema)
		return err.Error()
	})(), func() { _, _ = openTestDBForPackage(schema) })
}
