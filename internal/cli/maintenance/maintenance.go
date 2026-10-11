// Package maintenance implements "elythia maintenance", which puts every
// Elythia process into maintenance mode and brings them back by writing the
// state in Redis (#3463).
//
// 管理画面の認証の部品 (#3462) に依存せずに使える手段として先に用意する。
// 設定ファイルを読んで Redis へ直に書くので、本体が DB を使えないときにも
// 抜けられる (設計 §3.5 / §4.5)。待つ間は、この CLI 自身も判定役の候補になる。
// 本体が 1 つも動いていない構成でも、CLI だけで完結させるため (設計第2版 §2.1)。
package maintenance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/core/cache"
	"github.com/elythia-network/elythia/internal/maintmode"
)

// env carries the process-level dependencies so tests can replace them.
type env struct {
	stdout, stderr io.Writer
	loadConfig     func(path string) (*config.Config, error)
	// openStore returns the store and the function that closes it.
	openStore func(cfg *config.Config) (*maintmode.Store, func(), error)
	// counter returns what the judge counts the sessions with.
	counter func(cfg *config.Config, judgeID string) maintmode.Counter
	now     func() time.Time
	// pollInterval is how often enter / exit / wait step the judge.
	pollInterval time.Duration
}

func defaultEnv() env {
	return env{
		stdout:     os.Stdout,
		stderr:     os.Stderr,
		loadConfig: config.Load,
		openStore: func(cfg *config.Config) (*maintmode.Store, func(), error) {
			clients, err := cache.NewRedisClients(cfg)
			if err != nil {
				return nil, nil, err
			}
			store := maintmode.NewStore(clients.Default, clients.Pubsub, cfg.Redis.KeyPrefix())
			return store, func() { _ = clients.Close() }, nil
		},
		counter: func(cfg *config.Config, judgeID string) maintmode.Counter {
			return &maintmode.PGCounter{Targets: maintmode.TargetsFromConfig(cfg, "elythia-judge/"+judgeID)}
		},
		now:          time.Now,
		pollInterval: 500 * time.Millisecond,
	}
}

// Enter implements "elythia maintenance enter".
func Enter(args []string) int { return enter(defaultEnv(), args) }

// Exit implements "elythia maintenance exit".
func Exit(args []string) int { return exit(defaultEnv(), args) }

// Wait implements "elythia maintenance wait".
func Wait(args []string) int { return wait(defaultEnv(), args) }

// Status implements "elythia maintenance status".
func Status(args []string) int { return status(defaultEnv(), args) }

// session is what one command works with.
type session struct {
	e     env
	cfg   *config.Config
	store *maintmode.Store
	judge *maintmode.Judge
}

// open loads the config and opens the store, reporting failures.
func (e env) open(configPath string) (*session, func(), bool) {
	cfg, err := e.loadConfig(configPath)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to load config: %v\n", err)
		return nil, nil, false
	}
	store, closeFn, err := e.openStore(cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to connect to Redis: %v\n", err)
		return nil, nil, false
	}
	id := "cli-" + newID()
	judge := &maintmode.Judge{
		Store: store, Counter: e.counter(cfg, id), ID: id,
		DrainTimeout: cfg.Maintenance.DrainTimeout, Now: e.now,
	}
	if judge.DrainTimeout <= 0 {
		judge.DrainTimeout = config.DefaultMaintenanceDrainTimeout
	}
	s := &session{e: e, cfg: cfg, store: store, judge: judge}
	return s, func() {
		// 待ちを終えたら判定役を降りる。ほかの判定役 (本体の各プロセス) が引き継ぐ。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = judge.Release(ctx)
		closeFn()
	}, true
}

func enter(e env, args []string) int {
	fs := cliflag.New("maintenance enter", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	reason := fs.String("reason", "", "reason shown on the public maintenance page")
	// 既定は 2 分。1 つのプロセスが世代を止めるのに、処理中のリクエスト・背景処理・
	// DB を閉じるの 3 段でそれぞれ最大 30 秒 (計 90 秒) 掛かりうるので、それより長くする。
	waitFor := fs.Duration("wait", 2*time.Minute, "how long to wait until the maintenance is held (0 = do not wait)")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	s, closeFn, ok := e.open(*configPath)
	if !ok {
		return 1
	}
	defer closeFn()
	ctx := context.Background()

	if policy, err := s.store.EvictionPolicy(ctx); err == nil && maintmode.EvictsState(policy) {
		fmt.Fprintf(e.stderr, "warning: Redis has maxmemory-policy %s and may evict the maintenance state, "+
			"which would end the maintenance by itself; use noeviction or volatile-*\n", policy)
	}
	opID := "manual-" + newID()
	err := s.store.Enter(ctx, maintmode.State{
		OpID: opID, Kind: maintmode.KindManual, Reason: *reason, StartedAt: e.now(),
		RequestedBy: "cli",
	})
	if errors.Is(err, maintmode.ErrActive) {
		fmt.Fprintln(e.stderr, "a maintenance is already active; see `elythia maintenance status`")
		return 1
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to enter maintenance: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "entering maintenance (opId %s)\n", opID)
	if *waitFor <= 0 {
		return 0
	}
	return s.waitHeld(ctx, opID, *waitFor)
}

// waitHeld steps the judge until opID is held or the wait runs out.
func (s *session) waitHeld(ctx context.Context, opID string, waitFor time.Duration) int {
	e := s.e
	deadline := e.now().Add(waitFor)
	for {
		_, stepErr := s.judge.Step(ctx)
		st, err := s.store.Get(ctx)
		if err != nil {
			fmt.Fprintf(e.stderr, "failed to read the maintenance state: %v\n", err)
			return 1
		}
		if st == nil || st.OpID != opID {
			fmt.Fprintln(e.stderr, "the maintenance was ended or replaced while waiting")
			return 1
		}
		if st.Phase == maintmode.PhaseHeld {
			printTerminated(e.stdout, st.Terminated)
			fmt.Fprintln(e.stdout, "no session is left on the database; the maintenance is held")
			return 0
		}
		if st.Phase == maintmode.PhaseResuming {
			fmt.Fprintln(e.stderr, "the maintenance is resuming; it was ended while waiting")
			return 1
		}
		if !e.now().Before(deadline) {
			printWaiting(e.stderr, st, stepErr)
			fmt.Fprintln(e.stderr, "the maintenance is not held yet and stays at entering; the processes keep judging it. "+
				"Check with `elythia maintenance status`, or wait again with `elythia maintenance wait`")
			return 1
		}
		time.Sleep(e.pollInterval)
	}
}

func exit(e env, args []string) int {
	fs := cliflag.New("maintenance exit", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	waitFor := fs.Duration("wait", 2*time.Minute, "how long to wait for every process to serve again before clearing the state")
	force := fs.Bool("force", false, "clear the state at once without waiting for the processes")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	s, closeFn, ok := e.open(*configPath)
	if !ok {
		return 1
	}
	defer closeFn()
	ctx := context.Background()

	st, err := s.store.Get(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to read the maintenance state: %v\n", err)
		return 1
	}
	if st == nil {
		fmt.Fprintln(e.stdout, "no maintenance is active")
		return 0
	}
	if *force {
		if err := s.store.Clear(ctx, st.OpID); err != nil {
			fmt.Fprintf(e.stderr, "failed to clear the maintenance: %v\n", err)
			return 1
		}
		fmt.Fprintln(e.stdout, "the maintenance is over")
		return 0
	}
	// resuming で各プロセスに戻ってもらい、生きているプロセスが全部 running に
	// なったら判定役が消す。消すのを先にすると、戻れなかったプロセスに気付けない。
	if err := s.store.SetPhase(ctx, st.OpID, maintmode.PhaseResuming); err != nil {
		fmt.Fprintf(e.stderr, "failed to start resuming: %v\n", err)
		return 1
	}
	fmt.Fprintf(e.stdout, "resuming (opId %s)\n", st.OpID)
	if *waitFor <= 0 {
		return 0
	}
	return s.waitCleared(ctx, st.OpID, *waitFor)
}

// waitCleared steps the judge until opID is gone or the wait runs out.
func (s *session) waitCleared(ctx context.Context, opID string, waitFor time.Duration) int {
	e := s.e
	deadline := e.now().Add(waitFor)
	for {
		_, _ = s.judge.Step(ctx)
		st, err := s.store.Get(ctx)
		if err != nil {
			fmt.Fprintf(e.stderr, "failed to read the maintenance state: %v\n", err)
			return 1
		}
		if st == nil || st.OpID != opID {
			fmt.Fprintln(e.stdout, "every process serves again; the maintenance is over")
			return 0
		}
		if !e.now().Before(deadline) {
			reports, _ := s.store.Reports(ctx)
			printReports(e.stderr, reports)
			// resuming のままにしておく。プロセスは作り直しを繰り返すので、DB が
			// 戻れば自分で戻る。
			fmt.Fprintln(e.stderr, "not every process serves again; the state stays at resuming and they keep retrying. "+
				"Check with `elythia maintenance status`, or clear it with -force")
			return 1
		}
		time.Sleep(e.pollInterval)
	}
}

func wait(e env, args []string) int {
	fs := cliflag.New("maintenance wait", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	waitFor := fs.Duration("wait", 2*time.Minute, "how long to wait")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	s, closeFn, ok := e.open(*configPath)
	if !ok {
		return 1
	}
	defer closeFn()
	ctx := context.Background()
	st, err := s.store.Get(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to read the maintenance state: %v\n", err)
		return 1
	}
	switch {
	case st == nil:
		fmt.Fprintln(e.stdout, "no maintenance is active")
		return 0
	case st.Phase == maintmode.PhaseResuming:
		return s.waitCleared(ctx, st.OpID, *waitFor)
	default:
		return s.waitHeld(ctx, st.OpID, *waitFor)
	}
}

func status(e env, args []string) int {
	fs := cliflag.New("maintenance status", e.stderr)
	configPath := fs.String("config", ".config/default.yml", "path to configuration file")
	asJSON := fs.Bool("json", false, "print as JSON")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	s, closeFn, ok := e.open(*configPath)
	if !ok {
		return 1
	}
	defer closeFn()
	ctx := context.Background()
	st, err := s.store.Get(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to read the maintenance state: %v\n", err)
		return 1
	}
	reports, err := s.store.Reports(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to read the process reports: %v\n", err)
		return 1
	}
	judge, err := s.store.Judge(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "failed to read the judge lock: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(e.stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(struct {
			Maintenance *maintmode.State       `json:"maintenance"`
			Judge       string                 `json:"judge"`
			Processes   []maintmode.ProcReport `json:"processes"`
		}{st, judge, reports})
		return 0
	}
	if st == nil {
		fmt.Fprintln(e.stdout, "maintenance: none")
	} else {
		fmt.Fprintf(e.stdout, "maintenance: %s (opId %s, kind %s, since %s)\n",
			st.Phase, st.OpID, st.Kind, st.StartedAt.Format(time.RFC3339))
		if st.Reason != "" {
			fmt.Fprintf(e.stdout, "reason: %s\n", st.Reason)
		}
		if judge == "" {
			// 判定役がいないと entering から進まない (安全側)。本体が 1 つも
			// 動いていないなら、wait で CLI が判定する。
			fmt.Fprintln(e.stdout, "judge: none (run `elythia maintenance wait` to judge)")
		} else {
			fmt.Fprintf(e.stdout, "judge: %s (last looked %s)\n", judge, formatTime(st.JudgedAt))
		}
		if st.Phase == maintmode.PhaseHeld {
			fmt.Fprintf(e.stdout, "held since %s\n", formatTime(st.HeldAt))
			if len(st.Blockers) > 0 {
				fmt.Fprintln(e.stdout, "warning: these sessions connected after the maintenance was held:")
			}
		}
		printSessions(e.stdout, st.Blockers)
		if len(st.WaitingFor) > 0 {
			fmt.Fprintf(e.stdout, "waiting for: %s\n", strings.Join(st.WaitingFor, ", "))
		}
		printTerminated(e.stdout, st.Terminated)
		if st.JudgeError != "" {
			fmt.Fprintf(e.stdout, "judge error: %s\n", st.JudgeError)
		}
	}
	printReports(e.stdout, reports)
	return 0
}

func printWaiting(w io.Writer, st *maintmode.State, stepErr error) {
	if len(st.WaitingFor) > 0 {
		fmt.Fprintf(w, "processes not drained yet: %s\n", strings.Join(st.WaitingFor, ", "))
	}
	if len(st.Blockers) > 0 {
		fmt.Fprintln(w, "sessions still on the database:")
		printSessions(w, st.Blockers)
	}
	if stepErr != nil {
		fmt.Fprintf(w, "judge error: %v\n", stepErr)
	} else if st.JudgeError != "" {
		fmt.Fprintf(w, "judge error: %s\n", st.JudgeError)
	}
}

func printSessions(w io.Writer, sessions []maintmode.Blocker) {
	for _, b := range sessions {
		fmt.Fprintf(w, "  session %s\n", b.String())
	}
}

func printTerminated(w io.Writer, sessions []maintmode.Blocker) {
	if len(sessions) == 0 {
		return
	}
	fmt.Fprintln(w, "terminated Elythia sessions that outlived the drain timeout:")
	printSessions(w, sessions)
}

func printReports(w io.Writer, reports []maintmode.ProcReport) {
	if len(reports) == 0 {
		fmt.Fprintln(w, "processes: none reporting")
		return
	}
	fmt.Fprintln(w, "processes:")
	for _, r := range reports {
		line := fmt.Sprintf("  %s  role=%s  state=%s  openConns=%d  inFlight=%d  version=%s",
			r.ID, r.Role, r.State, r.OpenConns, r.InFlight, r.Version)
		if r.Commit != "" {
			line += " (" + r.Commit + ")"
		}
		if r.Error != "" {
			line += "  error=" + strings.ReplaceAll(r.Error, "\n", " ")
		}
		fmt.Fprintln(w, line)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Format(time.RFC3339)
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
