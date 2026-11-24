package migrate

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/testutil"
)

// withCherryPick makes e report r (or err) for the CherryPick check and
// counts the calls.
func withCherryPick(e *env, r maintenance.CherryPickReport, err error) *int {
	calls := 0
	e.cherrypick = func(*config.Config) (maintenance.CherryPickReport, error) {
		calls++
		return r, err
	}
	return &calls
}

var pendingCherryPick = maintenance.CherryPickReport{
	Status:       maintenance.CherryPickSupported,
	PendingUsers: []maintenance.CherryPickPendingUser{{ID: "u1", Username: "carol"}},
}

func TestRun_CherryPickRefusesUp(t *testing.T) {
	m := &fakeMigrator{}
	e, out, opened := testEnv(t, m, nil)
	calls := withCherryPick(&e, pendingCherryPick, nil)

	code := run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)})
	assert.Equal(t, 1, code)
	assert.Equal(t, 1, *calls)
	assert.Empty(t, *opened, "止めたときは migrator を開かない")
	assert.Empty(t, m.calls)
	assert.Contains(t, out.String(), "this database came from CherryPick and needs action")
	assert.Contains(t, out.String(), "then `elythia migrate -allow-cherrypick` and `elythia cherrypick convert`")
	assert.Contains(t, out.String(), "kindsNeedingAction=1")
}

// 対応していない版は -allow-cherrypick でも通さない。
// Elythia が流した後に止まったときは、convert を案内する。
func TestRun_CherryPickAfterMigratedPointsToConvert(t *testing.T) {
	m := &fakeMigrator{}
	e, out, _ := testEnv(t, m, nil)
	r := pendingCherryPick
	r.Migrated = true
	withCherryPick(&e, r, nil)

	assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
	assert.Contains(t, out.String(), "run `elythia cherrypick convert`")
	assert.NotContains(t, out.String(), "-allow-cherrypick`")
}

func TestRun_CherryPickUnsupportedVersion(t *testing.T) {
	for _, args := range [][]string{nil, {"-allow-cherrypick"}} {
		m := &fakeMigrator{}
		e, out, _ := testEnv(t, m, nil)
		withCherryPick(&e, maintenance.CherryPickReport{Status: maintenance.CherryPickUnsupported, Reason: "too old"}, nil)

		assert.Equal(t, 1, run(e, &bytes.Buffer{}, append([]string{"-config", writeConfig(t)}, args...)), args)
		assert.Empty(t, m.calls)
		assert.Contains(t, out.String(), "a CherryPick version Elythia does not support")
		assert.Contains(t, out.String(), `reason="too old"`)
	}
}

func TestRun_CherryPickAllowFlag(t *testing.T) {
	m := &fakeMigrator{}
	e, out, _ := testEnv(t, m, nil)
	withCherryPick(&e, pendingCherryPick, nil)

	assert.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-allow-cherrypick"}))
	assert.Equal(t, []string{"up"}, m.calls)
	assert.Contains(t, out.String(), "level=WARN")
	assert.Contains(t, out.String(), "migrating because of -allow-cherrypick")
}

func TestRun_CherryPickNothingToDo(t *testing.T) {
	for name, r := range map[string]maintenance.CherryPickReport{
		"not CherryPick": {Status: maintenance.NotCherryPick},
		// Elythia が流した後で要対応が無ければ、確認が印を付けて Converted を返す。
		"marked by the check": {Status: maintenance.CherryPickSupported, Migrated: true, Converted: true, NoteHistories: 3},
		// convert が済んでいれば、他の欄に何があっても止めない。
		"converted": {Converted: true, Status: maintenance.CherryPickSupported, PendingUsers: pendingCherryPick.PendingUsers},
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, out, _ := testEnv(t, m, nil)
			withCherryPick(&e, r, nil)
			assert.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
			assert.Equal(t, []string{"up"}, m.calls)
			assert.NotContains(t, out.String(), "came from CherryPick")
		})
	}
}

// 要対応の無い CherryPick の DB は、初めての migrate up の後に確かめ直して印を
// 付けさせる。付けないと、serve が次の migrate up まで起動しない。
func TestRun_CherryPickMarksAfterFirstUp(t *testing.T) {
	before := maintenance.CherryPickReport{Status: maintenance.CherryPickSupported, NoteHistories: 3}
	for _, tt := range []struct {
		name     string
		after    maintenance.CherryPickReport
		afterErr error
		wantCode int
		wantLog  string
	}{
		{name: "marked", after: maintenance.CherryPickReport{Status: maintenance.CherryPickSupported, Migrated: true, Converted: true}},
		{name: "not marked", after: maintenance.CherryPickReport{Status: maintenance.CherryPickSupported, Migrated: true},
			wantLog: "was not marked as converted"},
		// 流し終えた後の数え直しの失敗は、警告だけで 0 を返す (次の migrate で付く)。
		{name: "check failed", afterErr: errors.New("connection refused"),
			wantLog: "could not be marked as converted"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, out, _ := testEnv(t, m, nil)
			calls := 0
			e.cherrypick = func(*config.Config) (maintenance.CherryPickReport, error) {
				calls++
				if calls == 1 {
					return before, nil
				}
				// 確かめ直すのは流し終えてから。
				assert.Equal(t, []string{"up"}, m.calls)
				return tt.after, tt.afterErr
			}
			assert.Equal(t, tt.wantCode, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-track", "core"}))
			assert.Equal(t, 2, calls, "流した後に確かめ直す")
			if tt.wantLog == "" {
				assert.NotContains(t, out.String(), "came from CherryPick")
			} else {
				assert.Contains(t, out.String(), tt.wantLog)
			}
		})
	}
	// 既に流した DB、要対応がある DB、CherryPick ではない DB では確かめ直さない。
	for name, r := range map[string]maintenance.CherryPickReport{
		"migrated":       {Status: maintenance.CherryPickSupported, Migrated: true},
		"needs action":   pendingCherryPick,
		"not CherryPick": {Status: maintenance.NotCherryPick},
	} {
		t.Run(name, func(t *testing.T) {
			m := &fakeMigrator{}
			e, _, _ := testEnv(t, m, nil)
			calls := withCherryPick(&e, r, nil)
			args := []string{"-config", writeConfig(t), "-track", "core"}
			if name == "needs action" {
				args = append(args, "-allow-cherrypick")
			}
			assert.Equal(t, 0, run(e, &bytes.Buffer{}, args))
			assert.Equal(t, 1, *calls)
		})
	}
}

func TestRun_CherryPickCheckFails(t *testing.T) {
	m := &fakeMigrator{}
	e, out, _ := testEnv(t, m, nil)
	withCherryPick(&e, maintenance.CherryPickReport{}, errors.New("connection refused"))

	assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t)}))
	assert.Empty(t, m.calls)
	assert.Contains(t, out.String(), "failed to check whether the database came from CherryPick")
}

// 版を確かめられなかったときは、-allow-cherrypick でも進まない。
func TestRun_CherryPickStatusFailsWithAllow(t *testing.T) {
	m := &fakeMigrator{}
	e, out, _ := testEnv(t, m, nil)
	withCherryPick(&e, maintenance.CherryPickReport{}, &maintenance.CherryPickStatusError{Err: errors.New("timeout")})

	assert.Equal(t, 1, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-allow-cherrypick"}))
	assert.Empty(t, m.calls)
	assert.Contains(t, out.String(), "failed to check whether the database came from CherryPick")
}

// 件数を数えられなかったときは、-allow-cherrypick なら警告だけで進む (大きな DB の
// 時間切れで起動できなくならないように)。
func TestRun_CherryPickCheckFailsWithAllow(t *testing.T) {
	m := &fakeMigrator{}
	e, out, _ := testEnv(t, m, nil)
	withCherryPick(&e, maintenance.CherryPickReport{}, errors.New("timeout"))

	assert.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-allow-cherrypick"}))
	assert.Equal(t, []string{"up"}, m.calls)
	assert.Contains(t, out.String(), "level=WARN")
	assert.Contains(t, out.String(), "could not check whether the database came from CherryPick")
}

// 戻す操作は止めない。
func TestRun_CherryPickDoesNotBlockDownOrForce(t *testing.T) {
	t.Run("down", func(t *testing.T) {
		m := &fakeMigrator{}
		e, _, _ := testEnv(t, m, nil)
		calls := withCherryPick(&e, pendingCherryPick, nil)
		assert.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-direction", "down", "-track", "core"}))
		assert.Equal(t, []string{"down"}, m.calls)
		assert.Zero(t, *calls)
	})
	t.Run("force", func(t *testing.T) {
		m := &fakeMigrator{dirty: true, version: 2}
		e, _, _ := testEnv(t, m, nil)
		calls := withCherryPick(&e, pendingCherryPick, nil)
		e.coreDir = t.TempDir()
		writeMigration(t, e.coreDir, "000001_a", "SELECT 1;", "SELECT 1;")
		assert.Equal(t, 0, run(e, &bytes.Buffer{}, []string{"-config", writeConfig(t), "-force", "1", "-track", "core"}))
		assert.Equal(t, []string{"force"}, m.calls)
		assert.Zero(t, *calls)
	})
}

func TestDefaultEnv_ChecksCherryPick(t *testing.T) {
	assert.Equal(t, reflect.ValueOf(inspectCherryPick).Pointer(), reflect.ValueOf(defaultEnv().cherrypick).Pointer())
}

// CherryPick 4.18.0-beta.1 の DB に、本物の migration を流す。承認待ちが残っている間は
// 止まり、-allow-cherrypick なら最後まで当たる。流した後も、承認待ちは数えられる。
func TestMigrateDB_CherryPickDatabase(t *testing.T) {
	f := newDBFixture(t, "cherrypick")
	require.NoError(t, testutil.LoadCherryPickSchema(f.db))
	require.NoError(t, f.db.Exec(`INSERT INTO "user" (id, username, "usernameLower", approved, "signupReason") VALUES ('u1', 'carol', 'carol', false, 'hi')`).Error)
	require.NoError(t, f.db.Exec(`INSERT INTO meta (id, "approvalRequiredForSignup") VALUES ('x', true)`).Error)
	f.coreDir = filepath.Join("..", "..", "..", "migration")
	f.localDir = filepath.Join(t.TempDir(), "absent-local")
	inspect := func(*config.Config) (maintenance.CherryPickReport, error) {
		return maintenance.InspectCherryPickBeforeMigrate(context.Background(), f.db, time.Now())
	}

	code, out := f.migrateWith(t, inspect)
	require.Equal(t, 1, code, out)
	assert.False(t, f.tableExists(t, "schema_migrations"), "止めたときは何も流さない")

	code, out = f.migrateWith(t, inspect, "-allow-cherrypick")
	require.Equal(t, 0, code, out)
	latest, _, err := LatestVersion(f.coreDir)
	require.NoError(t, err)
	assert.Equal(t, []int64{int64(latest)}, f.versions(t, "schema_migrations"))

	r, err := maintenance.InspectCherryPick(context.Background(), f.db, time.Now())
	require.NoError(t, err)
	assert.Equal(t, maintenance.CherryPickSupported, r.Status, r.Reason)
	assert.Len(t, r.PendingUsers, 1, "流した後も、check では数えられる")

	// convert が済むまでは、起動のたびの migrate up も止める (#4248)。
	code, out = f.migrateWith(t, inspect)
	assert.Equal(t, 1, code, out)

	// 済んだら止めない。
	require.NoError(t, maintenance.MarkCherryPickConverted(context.Background(), f.db))
	code, out = f.migrateWith(t, inspect)
	assert.Equal(t, 0, code, out)
}

// 要対応の無い CherryPick の DB は、初めての migrate up で変換済みの印が付く。
// serve はこの印が無いと起動しない。
func TestMigrateDB_CherryPickNothingToDoIsMarked(t *testing.T) {
	f := newDBFixture(t, "cherrypick")
	require.NoError(t, testutil.LoadCherryPickSchema(f.db))
	f.coreDir = filepath.Join("..", "..", "..", "migration")
	f.localDir = filepath.Join(t.TempDir(), "absent-local")
	inspect := func(*config.Config) (maintenance.CherryPickReport, error) {
		return maintenance.InspectCherryPickBeforeMigrate(context.Background(), f.db, time.Now())
	}
	code, out := f.migrateWith(t, inspect)
	require.Equal(t, 0, code, out)
	needs, err := maintenance.CherryPickNeedsConvert(context.Background(), f.db)
	require.NoError(t, err)
	assert.False(t, needs, "初めての migrate up の後に印が付いている")
}
