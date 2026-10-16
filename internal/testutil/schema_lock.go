package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// errSchemaInUse means another process holds the schema's advisory lock.
var errSchemaInUse = errors.New("is in use by another test process")

// processLocks is this process's lock session and the schemas it holds. The
// session is never closed: the locks last until the process exits, which
// releases them.
//
// **鍵は 1 本の接続に全部載せる。** advisory lock はセッション単位で、1 つの
// セッションが複数の鍵を持てる。schema ごとに接続を張ると、兄弟 schema
// (OpenTestDBSchema) を多く使うパッケージでは接続がその数だけ増え続ける
// (internal/repository で 13 本、#3369 のレビュー2周目)。
var processLocks struct {
	mu      sync.Mutex
	session *lockSession
	held    map[string]bool
}

// lockSession is a pinned connection that holds advisory locks, and its own
// pool.
type lockSession struct {
	conn *sql.Conn
	pool *sql.DB
}

// openLockSession opens a dedicated connection for advisory locks.
func openLockSession() (*lockSession, error) {
	// .env.test の接続先に張る。testutil 自身のテストは openTestDBForPackage を
	// 通らずにここへ来るので、ここでも読む。
	loadEnvFile()
	db, err := openDSN(testDSN(""))
	if err != nil {
		return nil, fmt.Errorf("open test schema lock session: %w", err)
	}
	pool, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("open test schema lock session: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("open test schema lock session: %w", err)
	}
	return &lockSession{conn: conn, pool: pool}, nil
}

// tryLock takes the advisory lock of schema on this session. It wraps
// errSchemaInUse when another session holds it.
func (l *lockSession) tryLock(schema string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var got bool
	if err := l.conn.QueryRowContext(ctx,
		`SELECT pg_try_advisory_lock(hashtextextended($1, 0))`, schemaLockKey(schema)).Scan(&got); err != nil {
		return fmt.Errorf("lock test schema %s: %w", schema, err)
	}
	if !got {
		return fmt.Errorf("test schema %q on database %q %w. "+
			"Is a test of the same package running against the same database (another terminal, worktree or agent; "+
			"`go test -run` of tests that never touch the database counts too, since TestMain / init opens it)? "+
			"Run them against different databases (TEST_DB_NAME, or another PostgreSQL via TEST_DB_PORT)",
			schema, EnvOrDefault("TEST_DB_NAME", "misskey_test"), errSchemaInUse)
	}
	return nil
}

// Close ends the session, which releases every lock it holds (returning a
// *sql.Conn to the pool would keep the session, and the locks, alive).
func (l *lockSession) Close() error {
	_ = l.conn.Close()
	return l.pool.Close()
}

// claimSchema makes sure no other process uses schema on the same database
// while this one runs, by holding a session-level advisory lock keyed by the
// schema name on this process's lock session. It returns an error wrapping
// errSchemaInUse when another process holds it.
//
// schema はパッケージごとで、1 つのテストプロセスだけが使う前提で作っている
// (後片付けは自分の作った行しか知らない)。同じ DB に同じパッケージのテストを
// 2 本同時に流すと、互いの行が見えて一覧の比較などが無関係な形で落ちる
// (#3369 の TestPluginAccounts_ScopedPerPlugin_RealDB で実測)。そうなったときに
// 原因が分かるよう、最初に開いた時点で落とす。CI は shard ごとに DB を持ち、
// 1 つの shard の `go test` に各パッケージは 1 回しか入らないので、ここで落ちない。
//
// 鍵は hashtextextended の 64 bit。advisory lock は database ごとなので、
// TEST_DB_NAME を分ければ同じ schema 名でもぶつからない。
func claimSchema(schema string) error {
	processLocks.mu.Lock()
	defer processLocks.mu.Unlock()
	// **接続が切れたら鍵は外れている。** PostgreSQL の再起動や管理者の切断
	// (idle_session_timeout など) でセッションが終わると、鍵は黙って外れ、
	// *sql.Conn は張り直さないので以後のクエリは全部失敗する。捨てて張り直し、
	// 持っていたはずの鍵を取り直す (#3369 のレビュー3周目)。
	if s := processLocks.session; s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.conn.PingContext(ctx)
		cancel()
		if err != nil {
			if err := resetLockSession(); err != nil {
				return err
			}
		}
	}
	if processLocks.held[schema] {
		return nil
	}
	if processLocks.session == nil {
		s, err := openLockSession()
		if err != nil {
			return err
		}
		processLocks.session = s
		processLocks.held = map[string]bool{}
	}
	err := processLocks.session.tryLock(schema)
	if err != nil && !errors.Is(err, errSchemaInUse) {
		// Ping の後に切れた。もう一度だけ張り直して試す。
		if rerr := resetLockSession(); rerr != nil {
			return rerr
		}
		err = processLocks.session.tryLock(schema)
	}
	if err != nil {
		return err
	}
	processLocks.held[schema] = true
	return nil
}

// resetLockSession replaces the lock session with a new one and takes again
// every lock this process held. processLocks.mu must be held.
func resetLockSession() error {
	if processLocks.session != nil {
		_ = processLocks.session.Close()
		processLocks.session = nil
	}
	s, err := openLockSession()
	if err != nil {
		return err
	}
	processLocks.session = s
	var firstErr error
	for held := range processLocks.held {
		if err := s.tryLock(held); err != nil {
			// 外れていた間に別のプロセスが取った (か、また切れた)。持っていない
			// 鍵を持っていることにしない。
			delete(processLocks.held, held)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// lockSchema opens a session of its own and takes the lock of schema on it,
// standing in for another process in tests. Closing the session releases it.
func lockSchema(schema string) (*lockSession, error) {
	loadEnvFile()
	s, err := openLockSession()
	if err != nil {
		return nil, err
	}
	if err := s.tryLock(schema); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// schemaLockKey is the text hashed into the advisory lock key. 接頭辞を付け、
// 他の用途の advisory lock と鍵が重ならないようにする。
func schemaLockKey(schema string) string {
	return "elythia-testutil-schema:" + schema
}
