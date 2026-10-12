package restoreop

import (
	"context"

	"github.com/elythia-network/elythia/internal/backup"
)

// RollbackRequest asks to swap a kept database back in (#3463 (6d)).
type RollbackRequest struct {
	// Database is the kept database: <db>_before_restore_<t> or
	// <db>_rolled_back_<t>.
	Database string
	UserID   string
	// Token is the administrator's session token (only its hash is kept).
	Token string
}

// ValidKept returns ErrInvalidKept unless name has the form of a database
// kept by a restore or a rollback of the server's database. It reads
// nothing, so the control API calls it before the re-authentication.
//
// CLI の -rollback と同じく、退避した DB の名前の形だけを受ける。打ち間違えた名前や
// 細工した名前で、無関係な DB を本体の DB として入れ替えないため。
func (m *Manager) ValidKept(name string) error {
	if _, _, ok := backup.ParseKeptName(m.o.Database, name); !ok {
		return ErrInvalidKept
	}
	return nil
}

// PrepareRollback checks the request and migrates the kept database when
// it is behind the bundled migrations, while the server keeps running. The
// confirmation, the switch and the recovery are the ones of a restore; the
// kept database is never dropped (dropTarget).
func (m *Manager) PrepareRollback(ctx context.Context, req RollbackRequest) (*Record, string, error) {
	m.mu.Lock()
	base := m.base
	m.mu.Unlock()
	if base == nil || base.Err() != nil {
		return nil, "", ErrNotRunning
	}
	if err := m.ValidKept(req.Database); err != nil {
		return nil, "", err
	}
	release, err := m.o.Acquire()
	if err != nil {
		return nil, "", err
	}
	rec, ticket, migrations, err := m.rollbackChecks(ctx, req)
	if err != nil {
		release()
		return nil, "", err
	}
	pctx, cancel := context.WithCancel(base)
	live := &liveOp{cancelPrepare: cancel, release: release}
	m.mu.Lock()
	m.ops[rec.OpID] = live
	m.mu.Unlock()
	m.journal(ctx, rec.OpID, StepPreparing, "rolling back to %s; the current database will be kept as %s", rec.Target, rec.Before)
	m.modlog(ctx, m.o.Database, rec, "prepare")
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.prepare(pctx, rec, func(ctx context.Context) error { return m.migrateKept(ctx, rec, migrations) }, live)
	}()
	return rec, ticket, nil
}

// rollbackChecks runs the stop conditions of a rollback and creates its
// record.
//
// 接続の確認を、版を読む (戻す先へ繋ぐ) より先に行う。自分が閉じた接続が
// pg_stat_activity に少しの間残り、それを数えて断らないようにするため。
func (m *Manager) rollbackChecks(ctx context.Context, req RollbackRequest) (*Record, string, *backup.DatabaseMigrations, error) {
	if err := m.canStart(ctx); err != nil {
		return nil, "", nil, err
	}
	// 戻す先の版が同梱より古ければ、このサービスの同梱の migration を当てる。再開する
	// プロセスがそれと同じ版でなければ、戻した DB で起動できない (リストアと同じ)。
	if err := m.checkVersions(ctx); err != nil {
		return nil, "", nil, err
	}
	before, err := backup.RollbackName(m.o.Database, m.now())
	if err != nil {
		return nil, "", nil, err
	}
	has, err := m.o.DB.ExistingDatabases(ctx, m.o.MaintenanceDB, []string{req.Database})
	if err != nil {
		return nil, "", nil, err
	}
	if !has[req.Database] {
		return nil, "", nil, ErrNoSuchKept
	}
	if err := m.o.DB.CheckRollback(ctx, m.o.MaintenanceDB, m.o.Database, req.Database, before); err != nil {
		return nil, "", nil, err
	}
	migrations, err := m.o.DB.CheckDatabaseMigrations(ctx, req.Database)
	if err != nil {
		return nil, "", nil, err
	}
	ticket, ticketHash := newTicket()
	now := m.now()
	rec := &Record{
		OpID: newOpID(), Kind: KindRollback, Step: StepPreparing, Database: m.o.Database,
		Target: req.Database, Before: before, UserID: req.UserID, CreatedAt: now, UpdatedAt: now,
		ticketHash: ticketHash, tokenHash: hashString(req.Token),
	}
	if err := m.o.Store.create(ctx, rec); err != nil {
		return nil, "", nil, err
	}
	return rec, ticket, migrations, nil
}

// migrateKept applies the bundled migrations to the kept database when it is
// behind them.
//
// 再開するプロセスは migrate を流さないので、入れ替える前に当てる (リストアの準備と
// 同じ)。当てた後で中止しても、戻す先は新しい版のまま残る (中身は失わない)。
func (m *Manager) migrateKept(ctx context.Context, rec *Record, migrations *backup.DatabaseMigrations) error {
	if migrations == nil || !migrations.NeedsMigration {
		m.journal(ctx, rec.OpID, StepPreparing, "%s is at the bundled migrations", rec.Target)
		return ctx.Err()
	}
	m.journal(ctx, rec.OpID, StepPreparing, "applying the bundled migrations to %s", rec.Target)
	if _, err := m.o.DB.Migrate(rec.Target); err != nil {
		return err
	}
	m.journal(ctx, rec.OpID, StepPreparing, "migrations applied")
	return ctx.Err()
}

// Candidates lists the databases kept by restores and rollbacks, which a
// rollback can swap back in. It refuses while an operation is unfinished
// or a maintenance is on (ErrBusy, ErrMaintenanceActive): listing connects
// briefly to each kept database, which must not get in the way of a switch.
func (m *Manager) Candidates(ctx context.Context) ([]backup.KeptDatabase, error) {
	if cur, err := m.o.Store.Current(ctx); err != nil {
		return nil, err
	} else if cur != nil {
		return nil, ErrBusy
	}
	if err := m.NoMaintenance(ctx); err != nil {
		return nil, err
	}
	list, err := m.o.DB.KeptDatabases(ctx, m.o.MaintenanceDB, m.o.Database)
	if err != nil {
		return nil, err
	}
	if list == nil {
		list = []backup.KeptDatabase{}
	}
	return list, nil
}
