package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"

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
	Manager  *restoreop.Manager
	Verifier ReauthVerifier
}

// restoreBodyLimit caps a forwarded request body. パスキーの assertion を
// 含めても数 KB に収まる (本体の admin/backup/* と同じ上限)。
const restoreBodyLimit = 64 << 10

// Error IDs of the restore. Elythia-only.
const (
	idRestoreUnavailable = "515b8e48-8f90-439a-bd56-2bdd4d8870a5"
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
)

// restoreRequest is the body of the four endpoints.
type restoreRequest struct {
	// I is the administrator's session token (the main server moves it
	// from the Authorization header into the body).
	I            string          `json:"i"`
	GenerationID string          `json:"id"`
	OpID         string          `json:"opId"`
	Ticket       string          `json:"ticket"`
	Password     string          `json:"password"`
	Token        string          `json:"token"`
	Credential   json.RawMessage `json:"credential"`
}

// mountRestore adds the restore endpoints to mux.
//
//	POST /restore/prepare  {i, id, password, token|credential}  → {opId, ticket, status}
//	POST /restore/confirm  {i, opId, ticket, password, token|credential}
//	POST /restore/abort    {i, opId, ticket, password, token|credential}
//	POST /restore/status   {i, opId, ticket}  → status
//
// 本体は利用者のリクエストをそのまま転送し、応答をそのまま返す。そのため、
// 応答の形は本体の admin/* と同じ Misskey の形のエラーにする。
func mountRestore(mux *http.ServeMux, rs *Restore) {
	mux.HandleFunc("POST /restore/prepare", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readRestore(w, r, rs)
		if !ok {
			return
		}
		u, ok := reauth(w, r, rs, req)
		if !ok {
			return
		}
		rec, ticket, err := rs.Manager.Prepare(r.Context(), restoreop.PrepareRequest{GenerationID: req.GenerationID, UserID: u.ID, Token: req.I})
		if err != nil {
			restoreError(w, err)
			return
		}
		st, err := rs.Manager.Status(r.Context(), rec)
		if err != nil {
			restoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"opId": rec.OpID, "ticket": ticket, "status": st})
	})
	mux.HandleFunc("POST /restore/confirm", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readRestore(w, r, rs)
		if !ok {
			return
		}
		rec, ok := ticketRecord(w, r, rs, req)
		if !ok {
			return
		}
		// 準備ができていないものは、再認証の前に断る。名前を入れ替えている最中に
		// 照合の接続を DB へ張らないため。
		if rec.Step != restoreop.StepReady {
			restoreError(w, restoreop.ErrStep)
			return
		}
		if _, ok := reauth(w, r, rs, req); !ok {
			return
		}
		if err := rs.Manager.Confirm(r.Context(), rec.OpID); err != nil {
			restoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /restore/abort", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readRestore(w, r, rs)
		if !ok {
			return
		}
		rec, ok := ticketRecord(w, r, rs, req)
		if !ok {
			return
		}
		if rec.Step != restoreop.StepPreparing && rec.Step != restoreop.StepReady {
			restoreError(w, restoreop.ErrStep)
			return
		}
		if _, ok := reauth(w, r, rs, req); !ok {
			return
		}
		if err := rs.Manager.Abort(r.Context(), rec.OpID); err != nil {
			restoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /restore/status", func(w http.ResponseWriter, r *http.Request) {
		req, ok := readRestore(w, r, rs)
		if !ok {
			return
		}
		// 進み具合は ticket とセッションだけで返す (DB を引かない)。メンテナンス中にも
		// 読めるようにするため。
		rec, ok := ticketRecord(w, r, rs, req)
		if !ok {
			return
		}
		st, err := rs.Manager.Status(r.Context(), rec)
		if err != nil {
			restoreError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}

// readRestore reads a forwarded request body.
func readRestore(w http.ResponseWriter, r *http.Request, rs *Restore) (restoreRequest, bool) {
	var req restoreRequest
	if rs == nil || rs.Manager == nil || rs.Verifier == nil {
		writeJSON(w, http.StatusServiceUnavailable, apierr.ErrorWithKind("RESTORE_UNAVAILABLE",
			"Restoring from the admin page is not available on the backup service.", idRestoreUnavailable, apierr.KindServer))
		return req, false
	}
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
func reauth(w http.ResponseWriter, r *http.Request, rs *Restore, req restoreRequest) (*model.User, bool) {
	// パスキーの照合は Request の Origin と、Credential の assertion を読む
	// (strongauth が Credential を本文にした要求を作り直す)。
	u, err := rs.Verifier.Verify(r.Context(), req.I, clientIP(r), strongauth.Reauth{
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
func ticketRecord(w http.ResponseWriter, r *http.Request, rs *Restore, req restoreRequest) (*restoreop.Record, bool) {
	rec, err := rs.Manager.CheckTicket(r.Context(), req.OpID, req.Ticket, req.I)
	if err != nil {
		restoreError(w, err)
		return nil, false
	}
	return rec, true
}

// restoreError writes the response for an error of the restore.
func restoreError(w http.ResponseWriter, err error) {
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
	case errors.Is(err, restoreop.ErrNotRunning), errors.Is(err, ErrNotRunning):
		writeJSON(w, http.StatusServiceUnavailable, apierr.ErrorWithKind("RESTORE_UNAVAILABLE",
			"The backup service is stopping.", idRestoreUnavailable, apierr.KindServer))
	default:
		// 戻す前の検査 (権限、拡張、版など) の失敗。運営者が読めるよう、理由を載せる。
		writeJSON(w, http.StatusBadRequest, apierr.Error("RESTORE_PREPARE_FAILED", err.Error(), idRestorePrepareFail))
	}
}
