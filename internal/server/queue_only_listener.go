package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/shiroha-a/mk/internal/config"
)

// queueOnlyMux builds the tiny HTTP surface a queue-only node exposes.
//
// **`s.echo` は使わない。** あちらは setupRoutes で API / frontend の全ルートを
// 登録済みなので、流用すると配送ノードに API 面がそのまま生えてしまう。別の
// ServeMux を立てることで、生えるのはここに書いた分だけになる。
//
// # upstream との意図的な差分 (#2459)
//
// upstream の `onlyQueue` は一切 listen しない。そのまま真似ると
// `elythia healthcheck` (Dockerfile が使う) が必ず失敗し、**コンテナの
// ヘルスチェックを外さないと運用できないノード**ができる。死活監視を捨てる方が
// upstream との一致より高くつくので、`/healthz` だけは出す。
//
// `enableMetrics` が真なら `/metrics` も出す。worker の観測手段がここしか無い
// ため (admin API は server ノード側にしか無い)。
func (s *Server) queueOnlyMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		// server ノードの /healthz と同じ shape。role を足しているのは、
		// 監視先を取り違えたときに応答だけで気付けるようにするため。
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"role":   string(config.RoleQueue),
		})
	})
	if s.config.EnableMetrics {
		handler, err := buildMetricsHandler(s.queueMetrics)
		if err != nil {
			// server ノード側と同じく、metrics が出せなくても起動は続ける。
			slog.Error("server: failed to register queue metrics", "err", err)
		} else {
			mux.Handle("/metrics", handler)
		}
	}
	return mux
}

// serveQueueOnly listens with the minimal mux and blocks until shutdown.
//
// listener の張り方 (unix socket / TCP) は server ノードと揃える。同じ
// `elythia healthcheck` が両方の role に効くようにするため。
func (s *Server) serveQueueOnly() error {
	srv := &http.Server{Handler: s.queueOnlyMux()}
	applyServerTimeouts(srv)
	s.queueOnlyServer = srv

	var ln net.Listener
	var err error
	if s.config.Socket != "" {
		ln, err = config.ListenUnixSocket(s.config.Socket, s.config.ChmodSocket)
		if err != nil {
			return err
		}
		slog.Info("starting Misskey queue worker", "socket", s.config.Socket, "url", s.config.URL)
	} else {
		addr := fmt.Sprintf(":%d", s.config.Port)
		ln, err = net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		slog.Info("starting Misskey queue worker", "addr", addr, "url", s.config.URL)
	}

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
