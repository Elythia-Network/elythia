package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/maintmode"
)

const apiKept = "app_before_restore_20261001000000"

// TestRollbackAPI_PrepareConfirm runs a rollback through the control API:
// the prepare takes kind and database, the rest is the restore's.
func TestRollbackAPI_PrepareConfirm(t *testing.T) {
	r := newAPIRig(t)
	r.db.dbs[apiKept] = true
	rec, out := r.post(t, "/restore/prepare", auth(map[string]any{"kind": "rollback", "database": apiKept}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	opID, _ := out["opId"].(string)
	ticket, _ := out["ticket"].(string)
	st, _ := out["status"].(map[string]any)
	recd, _ := st["record"].(map[string]any)
	assert.Equal(t, restoreop.KindRollback, recd["kind"])
	assert.Equal(t, apiKept, recd["target"])
	assert.Equal(t, 1, r.verify.count())
	require.Eventually(t, func() bool {
		_, out := r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "opId": opID, "ticket": ticket})
		recd, _ := out["record"].(map[string]any)
		return recd["step"] == restoreop.StepReady
	}, 5*time.Second, 5*time.Millisecond)
	rec, _ = r.post(t, "/restore/confirm", auth(map[string]any{"opId": opID, "ticket": ticket}))
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.Eventually(t, func() bool {
		_, out := r.post(t, "/restore/status", map[string]any{"session": "session-token-01", "opId": opID, "ticket": ticket})
		recd, _ := out["record"].(map[string]any)
		return recd["step"] == restoreop.StepDone
	}, 5*time.Second, 5*time.Millisecond)
	r.db.mu.Lock()
	defer r.db.mu.Unlock()
	assert.False(t, r.db.dbs[apiKept], "the kept database now has the server's name")
}

// TestRollbackAPI_RefusedBeforeTheReauthentication checks that an unknown
// kind or a name that is not a kept database is refused without spending
// the re-authentication.
func TestRollbackAPI_RefusedBeforeTheReauthentication(t *testing.T) {
	r := newAPIRig(t)
	for _, c := range []struct {
		body map[string]any
		code string
	}{
		{auth(map[string]any{"kind": "drop", "database": apiKept}), "INVALID_PARAM"},
		{auth(map[string]any{"kind": "rollback", "database": "app"}), "NO_SUCH_KEPT_DATABASE"},
		{auth(map[string]any{"kind": "rollback", "database": "postgres"}), "NO_SUCH_KEPT_DATABASE"},
		{auth(map[string]any{"kind": "rollback"}), "NO_SUCH_KEPT_DATABASE"},
	} {
		rec, out := r.post(t, "/restore/prepare", c.body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, c.body)
		assert.Equal(t, c.code, errCode(out), c.body)
	}
	assert.Equal(t, 0, r.verify.count())
}

func TestRollbackAPI_PrepareErrors(t *testing.T) {
	r := newAPIRig(t)
	body := auth(map[string]any{"kind": "rollback", "database": apiKept})
	rec, out := r.post(t, "/restore/prepare", body)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "NO_SUCH_KEPT_DATABASE", errCode(out), "a kept name that does not exist")

	r.db.dbs[apiKept] = true
	for _, c := range []struct {
		set    func()
		status int
		code   string
	}{
		{func() { r.db.rollbackErr = fmt.Errorf("x: %w", backup.ErrRestoreConnections) }, http.StatusConflict, "KEPT_DATABASE_IN_USE"},
		{func() { r.db.migrationsErr = fmt.Errorf("x: %w", backup.ErrRestoreMigrationTooNew) }, http.StatusBadRequest, "KEPT_DATABASE_TOO_NEW"},
		{func() { r.db.migrationsErr = fmt.Errorf("x: %w", backup.ErrRestoreMigrationDirty) }, http.StatusBadRequest, "KEPT_DATABASE_DIRTY"},
		{func() { r.db.migrationsErr = fmt.Errorf("x: %w", backup.ErrRestoreNotElythia) }, http.StatusBadRequest, "KEPT_DATABASE_NOT_ELYTHIA"},
		{func() { r.db.rollbackErr = fmt.Errorf("x: %w", backup.ErrRestoreNoPrivilege) }, http.StatusBadRequest, "RESTORE_PREPARE_FAILED"},
	} {
		r.db.rollbackErr, r.db.migrationsErr = nil, nil
		c.set()
		rec, out := r.post(t, "/restore/prepare", body)
		assert.Equal(t, c.status, rec.Code, c.code)
		assert.Equal(t, c.code, errCode(out))
	}
	r.db.rollbackErr, r.db.migrationsErr = nil, nil

	// 戻した後の DB に資格情報が無い (2FA を後から有効にした) ときは、照合で断られる。
	// 画面はこの理由から CLI を案内する。
	r.verify.err = &strongauth.Error{Reason: strongauth.ReasonTwoFactorRequired}
	rec, out = r.post(t, "/restore/prepare", body)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Equal(t, "TWO_FACTOR_REQUIRED", errCode(out))
}

func TestRollbackAPI_Candidates(t *testing.T) {
	r := newAPIRig(t)
	size := int64(1234)
	r.db.kept = []backup.KeptDatabase{{Name: apiKept, Kind: backup.KeptBeforeRestore, At: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Size: &size}}
	rec, out := r.post(t, "/restore/candidates", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "app", out["database"])
	list, _ := out["databases"].([]any)
	require.Len(t, list, 1)
	first, _ := list[0].(map[string]any)
	assert.Equal(t, apiKept, first["name"])
	assert.Equal(t, backup.KeptBeforeRestore, first["kind"])
	assert.InDelta(t, 1234, first["size"], 0)
	assert.Equal(t, 0, r.verify.count(), "the main server re-authenticates the listing")

	r.db.kept = nil
	rec, out = r.post(t, "/restore/candidates", map[string]any{})
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, []any{}, out["databases"])

	r.db.keptErr = errors.New("connection refused")
	rec, out = r.post(t, "/restore/candidates", map[string]any{})
	assert.Equal(t, http.StatusInternalServerError, rec.Code)
	assert.Equal(t, "KEPT_DATABASES_UNREADABLE", errCode(out))
	assert.NotContains(t, rec.Body.String(), "connection refused", "the reason stays in the log")
	assert.Contains(t, r.log.String(), "connection refused")
	r.db.keptErr = nil

	require.NoError(t, r.maint.Enter(context.Background(), maintmode.State{OpID: "m1", Kind: maintmode.KindManual}))
	rec, out = r.post(t, "/restore/candidates", map[string]any{})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "MAINTENANCE_ACTIVE", errCode(out))

	// 1 つずつ: 準備の要求が照合している間は、待たずに断る。
	r2 := newAPIRig(t)
	r2.verify.entered, r2.verify.block = make(chan struct{}), make(chan struct{})
	entered := r2.verify.entered
	done := make(chan struct{})
	go func() {
		defer close(done)
		r2.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
	}()
	<-entered
	rec, out = r2.post(t, "/restore/candidates", map[string]any{})
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, "BACKUP_SERVICE_BUSY", errCode(out))
	close(r2.verify.block)
	<-done

	// 本文が読めない。
	req := httptest.NewRequest(http.MethodPost, "/restore/candidates", bytes.NewReader([]byte("{")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	// 組み立てる前は 503。
	h, err := Handler(New(Options{Storage: newMemStorage(), Logger: discardLogger()}), testToken, nil)
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, "/restore/candidates", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+testToken)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, req)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
}

func TestRollbackErrorMapping(t *testing.T) {
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{restoreop.ErrInvalidKept, http.StatusBadRequest, "NO_SUCH_KEPT_DATABASE"},
		{restoreop.ErrNoSuchKept, http.StatusBadRequest, "NO_SUCH_KEPT_DATABASE"},
		{fmt.Errorf("%w: %w", restoreop.ErrKept, backup.ErrRestoreConnections), http.StatusConflict, "KEPT_DATABASE_IN_USE"},
		{fmt.Errorf("%w: %w", restoreop.ErrKept, backup.ErrRestoreMigrationTooNew), http.StatusBadRequest, "KEPT_DATABASE_TOO_NEW"},
		{fmt.Errorf("%w: %w", restoreop.ErrKept, backup.ErrRestoreMigrationDirty), http.StatusBadRequest, "KEPT_DATABASE_DIRTY"},
		{fmt.Errorf("%w: %w", restoreop.ErrKept, backup.ErrRestoreNotElythia), http.StatusBadRequest, "KEPT_DATABASE_NOT_ELYTHIA"},
	} {
		w := httptest.NewRecorder()
		restoreError(w, discardLogger(), c.err)
		assert.Equal(t, c.status, w.Code, c.code)
		assert.Contains(t, w.Body.String(), `"code":"`+c.code+`"`)
	}
	// dirty の直し方は、今の DB ではなく戻す先を名指しする。
	w := httptest.NewRecorder()
	restoreError(w, discardLogger(), fmt.Errorf("%w: %w", restoreop.ErrKept, backup.ErrRestoreMigrationDirty))
	assert.Contains(t, w.Body.String(), "MK_DB_DB")
}

// TestRestoreAPI_MigrationRefusalsStayRestoreErrors checks that a restore of
// a generation whose migrations are too new or dirty still answers
// RESTORE_PREPARE_FAILED: the same sentinels mean the kept database only for
// a rollback.
func TestRestoreAPI_MigrationRefusalsStayRestoreErrors(t *testing.T) {
	r := newAPIRig(t)
	for _, sentinel := range []error{backup.ErrRestoreMigrationTooNew, backup.ErrRestoreMigrationDirty, backup.ErrRestoreNotElythia, backup.ErrRestoreConnections} {
		r.db.loadErr = fmt.Errorf("backup: %w: schema_migrations", sentinel)
		rec, out := r.post(t, "/restore/prepare", auth(map[string]any{"id": "20261010T120000Z"}))
		assert.Equal(t, http.StatusBadRequest, rec.Code, sentinel)
		assert.Equal(t, "RESTORE_PREPARE_FAILED", errCode(out), sentinel)
	}
}
