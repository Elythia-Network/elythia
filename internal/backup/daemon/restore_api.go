package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/api/adminbackup"
	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/backup"
	"github.com/elythia-network/elythia/internal/backup/restoreop"
	"github.com/elythia-network/elythia/internal/core/strongauth"
	"github.com/elythia-network/elythia/internal/model"
)

// ReauthVerifier checks the administrator's re-authentication against the
// database. *strongauth.DBVerifier implements it.
type ReauthVerifier interface {
	Verify(ctx context.Context, token, clientIP string, r strongauth.Reauth) (*model.User, error)
}

// Restore is the restore from the admin page served by the control API
// (#3463 (6c)).
type Restore struct {
	// Manager and Verifier are nil until the restore is available; Set
	// fills them later (the backup service started before Redis answered).
	Manager  *restoreop.Manager
	Verifier ReauthVerifier

	mu sync.RWMutex
	// storeErrLog throttles the log of records that cannot be read.
	storeErrLog logThrottle
	// busy is held by the request that prepares, confirms or aborts.
	//
	// 段の確かめから操作までの間に、別の要求の再認証 (DB への接続) が割り込まない
	// よう、操作を変える要求は 1 つずつにする。待たずに断る。
	busy sync.Mutex
}

// Set makes the restore available.
func (rs *Restore) Set(m *restoreop.Manager, v ReauthVerifier) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.Manager, rs.Verifier = m, v
}

// parts returns the manager and the verifier, or answers RESTORE_UNAVAILABLE
// while they are not set.
func (rs *Restore) parts(w http.ResponseWriter) (*restoreop.Manager, ReauthVerifier, bool) {
	var m *restoreop.Manager
	var v ReauthVerifier
	if rs != nil {
		rs.mu.RLock()
		m, v = rs.Manager, rs.Verifier
		rs.mu.RUnlock()
	}
	if m == nil || v == nil {
		writeJSON(w, http.StatusServiceUnavailable, apierr.ErrorWithKind("RESTORE_UNAVAILABLE",
			"Restoring from the admin page is not available on the backup service.", idRestoreUnavailable, apierr.KindServer))
		return nil, nil, false
	}
	return m, v, true
}

// lock takes busy, or answers BACKUP_SERVICE_BUSY.
func (rs *Restore) lock(w http.ResponseWriter) (func(), bool) {
	if !rs.busy.TryLock() {
		restoreError(w, nil, restoreop.ErrBusy)
		return nil, false
	}
	return rs.busy.Unlock, true
}

// restoreBodyLimit caps a forwarded request body. パスキーの assertion を
// 含めても数 KB に収まる (本体の admin/backup/* と同じ上限)。
const restoreBodyLimit = 64 << 10

// Error IDs of the restore. Elythia-only.
const (
	idRestoreUnavailable = adminbackup.IDRestoreUnavailable
	idRestoreBusy        = "46984259-1ce4-4d04-99fe-e366432cdbed"
	idNotVerified        = "cccb6a55-05b4-4554-89c8-910abde07062"
	idMaintenanceActive  = "373d46b6-1629-48e5-8548-ecead3ab39b8"
	idReplicas           = "d728972c-e3bf-479e-948d-5bc90c467383"
	idVersionMismatch    = "5f1b0105-fc04-482f-abf0-8d2bdd130b6b"
	// idNoSuchGeneration is the same ID as admin/backup/* returns.
	idNoSuchGeneration   = "3c0b2940-b9a6-402a-ae46-d358cb796600"
	idNoSuchOperation    = "b18da30a-ddf8-4739-942f-118141d294fc"
	idTicket             = "9acc0efb-0525-480d-a140-3c8c43a03a14"
	idStep               = "ee0a73b0-bde4-4473-82e7-9fff7d4084ab"
	idRestorePrepareFail = "7b035de7-20e9-4a89-a2ea-70f71a89f0a3"
	// The rollback (#3463 (6d)).
	idNoSuchKept     = "8a3797f6-9ec1-4d07-acf0-c2cf26b756ff"
	idKeptInUse      = "244566b9-100b-4e69-a81f-9621afeff435"
	idKeptTooNew     = "89ae0244-69fd-4aae-abaa-d5dab882cdda"
	idKeptDirty      = "34815d4b-8ed6-41a4-9033-80813e5387bf"
	idKeptNotElythia = "1038631b-9339-4d70-89fc-fdef999a95e4"
	idKeptUnreadable = "a6d32479-3b5b-4937-953f-585af90128a4"
)

// Values of restoreRequest.Kind.
const (
	kindRestore  = "restore"
	kindRollback = "rollback"
)

// restoreRequest is the body of the endpoints.
type restoreRequest struct {
	// Kind is "restore" (or empty) to restore generation GenerationID, or
	// "rollback" to swap the kept database Database back in (prepare only).
	Kind     string `json:"kind"`
	Database string `json:"database"`
	// I is the administrator's session token (the main server moves it
	// from the Authorization header into the body).
	I            string          `json:"i"`
	GenerationID string          `json:"id"`
	OpID         string          `json:"opId"`
	Ticket       string          `json:"ticket"`
	Password     string          `json:"password"`
	Token        string          `json:"token"`
	Credential   json.RawMessage `json:"credential"`
	// Session is the session token status is answered for. status is not
	// authenticated by the main server (the restored database may not have
	// the session), so the admin page sends it here instead of in i.
	Session string `json:"session"`
}

// mountRestore adds the restore endpoints to mux.
//
//	POST /restore/prepare  {i, id, password, token|credential}  → {opId, ticket, status}
//	                       {i, kind: "rollback", database, password, token|credential}
//	POST /restore/confirm  {i, opId, ticket, password, token|credential}
//	POST /restore/abort    {i, opId, ticket, password, token|credential}
//	POST /restore/status   {session (or i), opId, ticket}  → status
//	POST /restore/candidates {}  → {database, databases: [...]}
//
// ロールバック (#3463 (6d)) は、準備だけ種類を受け、確定・中止・進み具合は
// リストアと同じ枠を使う。照合は今の DB で行う (v1 §7-6)。戻した後の DB に
// 資格情報が無ければ断られるので、画面は CLI (elythia backup restore -rollback)
// を案内する。
//
// 本体は利用者のリクエストをそのまま転送し、応答をそのまま返す。そのため、
// 応答の形は本体の admin/* と同じ Misskey の形のエラーにする。
func mountRestore(mux *http.ServeMux, rs *Restore, log *slog.Logger) {
	mux.HandleFunc("POST /restore/prepare", func(w http.ResponseWriter, r *http.Request) {
		m, v, ok := rs.parts(w)
		if !ok {
			return
		}
		req, ok := readRestore(w, r)
		if !ok {
			return
		}
		// 種類と名前の形は、再認証 (TOTP の消費) の前に断る。
		switch req.Kind {
		case "", kindRestore:
		case kindRollback:
			if err := m.ValidKept(req.Database); err != nil {
				restoreError(w, log, err)
				return
			}
		default:
			writeJSON(w, http.StatusBadRequest, apierr.InvalidParam())
			return
		}
		unlock, ok := rs.lock(w)
		if !ok {
			return
		}
		defer unlock()
		// 始められないもの (終わっていない操作、メンテナンス中、レプリカ) は、
		// 再認証の前に断る。再認証は DB へ接続を張るため。
		if err := m.CanStart(r.Context()); err != nil {
			restoreError(w, log, err)
			return
		}
		u, ok := reauth(w, r, v, req)
		if !ok {
			return
		}
		var rec *restoreop.Record
		var ticket string
		var err error
		if req.Kind == kindRollback {
			rec, ticket, err = m.PrepareRollback(r.Context(), restoreop.RollbackRequest{Database: req.Database, UserID: u.ID, Token: req.I})
		} else {
			rec, ticket, err = m.Prepare(r.Context(), restoreop.PrepareRequest{GenerationID: req.GenerationID, UserID: u.ID, Token: req.I})
		}
		if err != nil {
			restoreError(w, log, err)
			return
		}
		st, err := m.Status(r.Context(), rec)
		if err != nil {
			restoreError(w, log, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"opId": rec.OpID, "ticket": ticket, "status": st})
	})
	mux.HandleFunc("POST /restore/confirm", func(w http.ResponseWriter, r *http.Request) {
		m, v, ok := rs.parts(w)
		if !ok {
			return
		}
		req, ok := readRestore(w, r)
		if !ok {
			return
		}
		unlock, ok := rs.lock(w)
		if !ok {
			return
		}
		defer unlock()
		rec, ok := rs.ticketRecord(w, r, log, m, req)
		if !ok {
			return
		}
		// 準備ができていないもの、メンテナンス中は、再認証の前に断る。名前を入れ替え
		// ている最中に照合の接続を DB へ張らないため。
		if rec.Step != restoreop.StepReady {
			restoreError(w, log, restoreop.ErrStep)
			return
		}
		if err := m.NoMaintenance(r.Context()); err != nil {
			restoreError(w, log, err)
			return
		}
		if _, ok := reauth(w, r, v, req); !ok {
			return
		}
		if err := m.Confirm(r.Context(), rec.OpID); err != nil {
			restoreError(w, log, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /restore/abort", func(w http.ResponseWriter, r *http.Request) {
		m, v, ok := rs.parts(w)
		if !ok {
			return
		}
		req, ok := readRestore(w, r)
		if !ok {
			return
		}
		unlock, ok := rs.lock(w)
		if !ok {
			return
		}
		defer unlock()
		rec, ok := rs.ticketRecord(w, r, log, m, req)
		if !ok {
			return
		}
		if rec.Step != restoreop.StepPreparing && rec.Step != restoreop.StepReady {
			restoreError(w, log, restoreop.ErrStep)
			return
		}
		if err := m.NoMaintenance(r.Context()); err != nil {
			restoreError(w, log, err)
			return
		}
		if _, ok := reauth(w, r, v, req); !ok {
			return
		}
		if err := m.Abort(r.Context(), rec.OpID); err != nil {
			restoreError(w, log, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /restore/status", func(w http.ResponseWriter, r *http.Request) {
		m, _, ok := rs.parts(w)
		if !ok {
			return
		}
		req, ok := readRestore(w, r)
		if !ok {
			return
		}
		// 進み具合は ticket とセッションだけで返す (DB を引かない)。メンテナンス中や、
		// 戻した DB にセッションが無いときにも読めるようにするため。
		if req.Session != "" {
			req.I = req.Session
		}
		rec, ok := rs.ticketRecord(w, r, log, m, req)
		if !ok {
			return
		}
		st, err := m.Status(r.Context(), rec)
		if err != nil {
			restoreError(w, log, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}

// mountCandidates adds POST /restore/candidates: the databases kept by
// restores and rollbacks, which a rollback can swap back in.
//
// 本体の admin/backup/restore/candidates は、一覧 (admin/backup/list) と同じく
// 本体の再認証 (backupGuard) を通ってから転送する。ここでは照合しない (同じ TOTP の
// コードを本体と daemon で二重に消費しないため)。制御 API の serviceToken は
// 通っている。操作を変える要求と同じく 1 つずつにする (一覧は退避した DB へ短く
// 繋ぐので、準備の接続の確認と重ねない)。
func mountCandidates(mux *http.ServeMux, rs *Restore, log *slog.Logger) {
	mux.HandleFunc("POST /restore/candidates", func(w http.ResponseWriter, r *http.Request) {
		m, _, ok := rs.parts(w)
		if !ok {
			return
		}
		if _, ok := readRestore(w, r); !ok {
			return
		}
		unlock, ok := rs.lock(w)
		if !ok {
			return
		}
		defer unlock()
		list, err := m.Candidates(r.Context())
		switch {
		case errors.Is(err, restoreop.ErrBusy), errors.Is(err, restoreop.ErrMaintenanceActive):
			restoreError(w, log, err)
			return
		case err != nil:
			// 理由には DB の名前や接続先が入るので、応答には載せずログにだけ残す。
			if log != nil {
				log.Warn("backup: cannot list the kept databases", "err", err)
			}
			writeJSON(w, http.StatusInternalServerError, apierr.ErrorWithKind("KEPT_DATABASES_UNREADABLE",
				"The backup service cannot list the kept databases. See the log of the backup service for the reason.", idKeptUnreadable, apierr.KindServer))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"database": m.Database(), "databases": list})
	})
}

// readRestore reads a forwarded request body.
func readRestore(w http.ResponseWriter, r *http.Request) (restoreRequest, bool) {
	var req restoreRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, restoreBodyLimit+1))
	if err != nil || len(body) > restoreBodyLimit {
		writeJSON(w, http.StatusBadRequest, apierr.InvalidParam())
		return req, false
	}
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, apierr.InvalidParam())
		return req, false
	}
	return req, true
}

// clientIP is the administrator's address the main server forwarded. The
// control API only reads it after the service token matched.
func clientIP(r *http.Request) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(ClientIPHeader)))
	if err != nil {
		return ""
	}
	return ip.String()
}

// reauth checks every strongauth condition for the request.
func reauth(w http.ResponseWriter, r *http.Request, v ReauthVerifier, req restoreRequest) (*model.User, bool) {
	// パスキーの照合は Request の Origin と、Credential の assertion を読む
	// (strongauth が Credential を本文にした要求を作り直す)。
	u, err := v.Verify(r.Context(), req.I, clientIP(r), strongauth.Reauth{
		Password: req.Password, Token: req.Token, Credential: req.Credential, Request: r,
	})
	if err != nil {
		res := adminbackup.Refusal(err, false)
		if res.RetryAfter != "" {
			w.Header().Set("Retry-After", res.RetryAfter)
		}
		writeJSON(w, res.Status, res.Body)
		return nil, false
	}
	return u, true
}

// ticketRecord checks the ticket and the session of a request.
//
// status は本体で認証しないので、誰の要求でもここまで届く。知らない操作や合わない
// ticket はログに書かず、Redis を読めないときだけ、間隔を空けて書く (匿名の要求で
// ログを溢れさせないため)。
func (rs *Restore) ticketRecord(w http.ResponseWriter, r *http.Request, log *slog.Logger, m *restoreop.Manager, req restoreRequest) (*restoreop.Record, bool) {
	rec, err := m.CheckTicket(r.Context(), req.OpID, req.Ticket, req.I)
	switch {
	case err == nil:
		return rec, true
	case errors.Is(err, restoreop.ErrNotFound), errors.Is(err, restoreop.ErrTicket):
		restoreError(w, nil, err)
	default:
		if log != nil && rs.storeErrLog.allow(time.Now()) {
			log.Warn("backup: cannot read a restore record", "err", err)
		}
		writeJSON(w, http.StatusServiceUnavailable, apierr.ErrorWithKind("RESTORE_UNAVAILABLE",
			"The backup service cannot read the restore now. Please try again later.", idRestoreUnavailable, apierr.KindServer))
	}
	return nil, false
}

// logThrottle lets one log line through per interval.
type logThrottle struct {
	mu   sync.Mutex
	last time.Time
}

// storeErrLogInterval is how often a failure to read a record is logged.
const storeErrLogInterval = time.Minute

func (t *logThrottle) allow(now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.last.IsZero() && now.Sub(t.last) < storeErrLogInterval {
		return false
	}
	t.last = now
	return true
}

// restoreError writes the response for an error of the restore.
func restoreError(w http.ResponseWriter, log *slog.Logger, err error) {
	switch {
	case errors.Is(err, restoreop.ErrInvalidID):
		writeJSON(w, http.StatusBadRequest, apierr.InvalidParam())
	case errors.Is(err, backup.ErrNotFound):
		writeJSON(w, http.StatusBadRequest, apierr.Error("NO_SUCH_GENERATION", "No such backup generation.", idNoSuchGeneration))
	case errors.Is(err, restoreop.ErrNotVerified):
		writeJSON(w, http.StatusBadRequest, apierr.Error("GENERATION_NOT_VERIFIED",
			"The backup generation has not passed verification. Verify it first.", idNotVerified))
	case errors.Is(err, restoreop.ErrReplicas):
		writeJSON(w, http.StatusBadRequest, apierr.Error("RESTORE_NOT_SUPPORTED",
			"Restoring from the admin page is not supported with database replicas. Use elythia backup restore.", idReplicas))
	case errors.Is(err, restoreop.ErrBusy), errors.Is(err, ErrBusy):
		writeJSON(w, http.StatusConflict, apierr.Error("BACKUP_SERVICE_BUSY",
			"The backup service is busy. Please try again later.", idRestoreBusy))
	case errors.Is(err, restoreop.ErrMaintenanceActive):
		writeJSON(w, http.StatusConflict, apierr.Error("MAINTENANCE_ACTIVE",
			"A maintenance is already active.", idMaintenanceActive))
	case errors.Is(err, restoreop.ErrVersionMismatch):
		writeJSON(w, http.StatusConflict, apierr.Error("VERSION_MISMATCH",
			"A server process runs another version of Elythia than the backup service.", idVersionMismatch))
	case errors.Is(err, restoreop.ErrNotFound):
		writeJSON(w, http.StatusNotFound, apierr.Error("NO_SUCH_RESTORE", "No such restore.", idNoSuchOperation))
	case errors.Is(err, restoreop.ErrTicket):
		writeJSON(w, http.StatusForbidden, apierr.ErrorWithKind("RESTORE_TICKET_INVALID",
			"The restore ticket does not match this session.", idTicket, apierr.KindPermission))
	case errors.Is(err, restoreop.ErrStep):
		writeJSON(w, http.StatusConflict, apierr.Error("RESTORE_STEP",
			"The restore is not at a step that allows this.", idStep))
	case errors.Is(err, restoreop.ErrInvalidKept), errors.Is(err, restoreop.ErrNoSuchKept):
		writeJSON(w, http.StatusBadRequest, apierr.Error("NO_SUCH_KEPT_DATABASE",
			"No such database kept by a restore or a rollback.", idNoSuchKept))
	case errors.Is(err, restoreop.ErrKept) && errors.Is(err, backup.ErrRestoreConnections):
		writeJSON(w, http.StatusConflict, apierr.Error("KEPT_DATABASE_IN_USE",
			"Someone is connected to the database to roll back to. Disconnect them first.", idKeptInUse))
	case errors.Is(err, restoreop.ErrKept) && errors.Is(err, backup.ErrRestoreMigrationTooNew):
		writeJSON(w, http.StatusBadRequest, apierr.Error("KEPT_DATABASE_TOO_NEW",
			"The database to roll back to has migrations this version of Elythia does not know. Use a version at least as new as that database.", idKeptTooNew))
	case errors.Is(err, restoreop.ErrKept) && errors.Is(err, backup.ErrRestoreMigrationDirty):
		writeJSON(w, http.StatusBadRequest, apierr.Error("KEPT_DATABASE_DIRTY",
			"A migration of the database to roll back to stopped in the middle. Repair its migration table first (elythia migrate -force with MK_DB_DB set to that database).", idKeptDirty))
	case errors.Is(err, restoreop.ErrKept) && errors.Is(err, backup.ErrRestoreNotElythia):
		writeJSON(w, http.StatusBadRequest, apierr.Error("KEPT_DATABASE_NOT_ELYTHIA",
			"The database to roll back to is not an Elythia database.", idKeptNotElythia))
	case errors.Is(err, restoreop.ErrNotRunning), errors.Is(err, ErrNotRunning):
		writeJSON(w, http.StatusServiceUnavailable, apierr.ErrorWithKind("RESTORE_UNAVAILABLE",
			"The backup service is stopping.", idRestoreUnavailable, apierr.KindServer))
	default:
		// 戻す前の検査 (権限、拡張、版など) の失敗。理由には DB やファイルの名前が入る
		// ので、応答には載せず daemon のログにだけ残す。
		if log != nil {
			log.Warn("backup: a restore from the admin page could not be prepared", "err", err)
		}
		writeJSON(w, http.StatusBadRequest, apierr.Error("RESTORE_PREPARE_FAILED",
			"The restore could not be prepared. See the log of the backup service for the reason.", idRestorePrepareFail))
	}
}
