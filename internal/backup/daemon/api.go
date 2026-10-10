package daemon

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/backup"
)

// ErrNoServiceToken is returned when the control API is configured without
// backup.server.serviceToken.
var ErrNoServiceToken = errors.New("backup.schedule.listen is set but backup.server.serviceToken is empty")

// ClientIPHeader carries the IP address of the person who asked the main
// server for the operation (#3462, #3463). It is trusted only on requests
// whose service token matched, and only logged.
const ClientIPHeader = "X-Elythia-Client-IP"

// maxRequestBody bounds the request bodies of the control API.
const maxRequestBody = 4 << 10

// Handler returns the control API (#3462).
//
//	POST /take    start a take job (take, verify when configured, prune)
//	POST /verify  start a verify job; body {"id": "<generation id>"}
//	GET  /status  the Status snapshot
//
// Every request must carry "Authorization: Bearer <token>". token must not
// be empty.
//
// 本体の管理画面が「今すぐ取る」「今すぐ確かめる」「状態」を頼む口。バックアップには
// 利用者の秘密鍵や token が入るので、token を知らない相手には状態も見せない。
func Handler(d *Daemon, token string) (http.Handler, error) {
	if token == "" {
		return nil, ErrNoServiceToken
	}
	want := sha256.Sum256([]byte(token))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /take", func(w http.ResponseWriter, r *http.Request) {
		job, err := d.Start(JobTake, "")
		writeStart(w, job, err)
	})
	mux.HandleFunc("POST /verify", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err := dec.Decode(&body); err != nil || !backup.ValidID(body.ID) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_id"})
			return
		}
		if _, err := d.opts.Storage.Stat(r.Context(), backup.Key(body.ID, backup.MetaFile)); err != nil {
			if errors.Is(err, backup.ErrNotFound) {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "no_such_generation"})
				return
			}
			d.log.Error("backup: cannot stat a generation", "generation", body.ID, "error", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": "storage_error"})
			return
		}
		job, err := d.Start(JobVerify, body.ID)
		writeStart(w, job, err)
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, d.Status())
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		// 長さの違いも漏らさないよう、hash にそろえてから定数時間で比べる。
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="elythia-backup"`)
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		// 接続元 IP のヘッダーは、token が一致した (本体から来た) リクエストのときだけ
		// 読む。一致しないリクエストはここまで来ないので、偽のヘッダーがログに残らない。
		// IP として読めない値は捨てる (ログへ任意の文字列を書かせない)。
		attrs := []any{"method", r.Method, "path", r.URL.Path}
		if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get(ClientIPHeader))); err == nil {
			attrs = append(attrs, "client_ip", ip.String())
		}
		d.log.Info("backup: control API request", attrs...)
		mux.ServeHTTP(w, r)
	}), nil
}

func writeStart(w http.ResponseWriter, job Job, err error) {
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
	case errors.Is(err, ErrBusy):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "busy", "running": job})
	default:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "not_running"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Serve serves h on ln until ctx is done.
func Serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}
