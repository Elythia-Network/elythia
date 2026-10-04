// Package main is a minimal HTTP black-hole receiver used by the queue
// bench (#563) to absorb deliver/inbox POST traffic without doing any
// work. Every request returns 204 No Content immediately so the sender's
// queue can drain at its native throughput.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	flag.Parse()

	var hits atomic.Uint64

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// body をドレインしないと keep-alive で次の req と混ざる場合があるため
		// io.Discard 相当の処理を入れる。Misskey TS / mk-go の deliver client
		// は keep-alive を有効にしてくる前提。
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(fmt.Appendf(nil, `{"hits":%d}`, hits.Load()))
	})
	mux.HandleFunc("/reset", func(w http.ResponseWriter, _ *http.Request) {
		hits.Store(0)
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: *addr, Handler: mux}
	log.Printf("blackhole listening on %s", *addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
