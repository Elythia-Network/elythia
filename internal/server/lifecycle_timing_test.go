package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// TestSupervisor_StopReportsStepDurations checks that stopping a generation
// reports how long each step took (#3802). 段ごとに違う長さで止め、後の段ほど
// 短くする。測る区間を取り違えると下限を割り、前の段が区間に混ざると上限を超える。
func TestSupervisor_StopReportsStepDurations(t *testing.T) {
	const (
		drain     = 240 * time.Millisecond
		stopParts = 120 * time.Millisecond
		closePool = 40 * time.Millisecond
	)
	f := newFakeGenerations(t)
	f.hook = func(context.Context) { time.Sleep(stopParts) }
	f.closeDB = func(*gorm.DB) error { time.Sleep(closePool); return nil }
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(5*time.Second))
	require.NoError(t, err)

	go func() { _, _ = getBody(t, sup, "/slow") }()
	<-f.entered
	time.AfterFunc(drain, func() { close(f.release) })

	rep := sup.stopGeneration(http.HandlerFunc(unavailable))
	require.NoError(t, rep.Err)
	assert.Zero(t, rep.InFlight)
	// AfterFunc は stopGeneration が began を取るより前に始まるので、その隙間
	// (genMu・setHandler・WithTimeout) の分だけ短く出うる。余裕として 20ms 引く。
	assert.GreaterOrEqual(t, rep.Drain, drain-20*time.Millisecond, "処理中のリクエストを待った時間")
	assert.GreaterOrEqual(t, rep.StopBackground, stopParts, "背景処理を止めた時間")
	assert.GreaterOrEqual(t, rep.CloseDB, closePool, "DB のプールを閉じた時間")
	assert.Less(t, rep.Drain, drain+stopParts, "待ちの区間に背景処理の停止が入っている")
	assert.Less(t, rep.StopBackground, drain, "背景処理の停止の区間に待ちが入っている")
	assert.Less(t, rep.CloseDB, stopParts, "DB を閉じる区間に背景処理の停止が入っている")
}

// TestSupervisor_StartReportsStepDurations checks the same for building and
// starting a generation, and that a failed build still reports the steps
// that ran.
func TestSupervisor_StartReportsStepDurations(t *testing.T) {
	const (
		openDB    = 240 * time.Millisecond
		construct = 120 * time.Millisecond
		startup   = 40 * time.Millisecond
	)
	f := newFakeGenerations(t)
	sup, err := NewSupervisor(&config.Config{}, nil, f.deps(time.Second))
	require.NoError(t, err)
	sup.stopGeneration(http.HandlerFunc(unavailable))

	f.openDelay, f.buildDelay = openDB, construct
	f.start = func() { time.Sleep(startup) }
	rep, err := sup.startGeneration()
	require.NoError(t, err)
	assert.GreaterOrEqual(t, rep.OpenDB, openDB, "DB を開いた時間")
	assert.GreaterOrEqual(t, rep.Construct, construct, "組み立てた時間")
	assert.GreaterOrEqual(t, rep.StartBackground, startup, "背景処理を起動した時間")
	assert.Less(t, rep.OpenDB, openDB+construct, "DB を開く区間に組み立てが入っている")
	assert.Less(t, rep.Construct, openDB, "組み立ての区間に DB を開く時間が入っている")
	assert.Less(t, rep.StartBackground, construct, "起動の区間に組み立てが入っている")

	// 動いている世代があるときは何もしないので、時間も 0。
	rep, err = sup.startGeneration()
	require.NoError(t, err)
	assert.Equal(t, StartReport{}, rep)

	// 組み立てに失敗しても、走った段の時間は返す。
	sup.stopGeneration(http.HandlerFunc(unavailable))
	f.buildErr = errors.New("bad plugin")
	rep, err = sup.startGeneration()
	require.ErrorContains(t, err, "bad plugin")
	assert.GreaterOrEqual(t, rep.OpenDB, openDB)
	assert.GreaterOrEqual(t, rep.Construct, construct)
	assert.Zero(t, rep.StartBackground)
}

// TestMaintenance_LogsStepDurations checks that entering and leaving a
// maintenance logs the step durations, so they can be read from a running
// instance (#3802).
func TestMaintenance_LogsStepDurations(t *testing.T) {
	r := newMaintenanceRig(t)
	buf := captureDefaultLog(t)

	sup := r.start(t, "proc-a", r.deps("proc-a"))
	waitProc(t, sup, maintmode.ProcRunning)
	enterMaintenance(t, r.store)
	waitProc(t, sup, maintmode.ProcDrained)
	require.NoError(t, r.store.Clear(context.Background(), "op-1"))
	waitProc(t, sup, maintmode.ProcRunning)

	logs := buf.String()
	built := logLine(t, logs, `"msg":"server generation built"`)
	for _, key := range []string{`"openDB":`, `"construct":`} {
		assert.Contains(t, built, key)
	}
	drained := logLine(t, logs, `"msg":"maintenance: drained"`)
	for _, key := range []string{`"drain":`, `"stopBackground":`, `"closeDB":`} {
		assert.Contains(t, drained, key)
	}
	serving := logLine(t, logs, `"msg":"maintenance: serving again"`)
	for _, key := range []string{`"openDB":`, `"construct":`, `"startBackground":`} {
		assert.Contains(t, serving, key)
	}
}

// logLine returns the single log line containing msg.
func logLine(t *testing.T, logs, msg string) string {
	t.Helper()
	found := logLines(logs, msg)
	require.Len(t, found, 1, "log lines with %s:\n%s", msg, logs)
	return found[0]
}

// logLines returns the log lines containing msg.
func logLines(logs, msg string) []string {
	var found []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, msg) {
			found = append(found, line)
		}
	}
	return found
}
