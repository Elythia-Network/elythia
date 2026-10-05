// Command plugindev runs mk-go for plugin development (#2477).
//
// プラグインのソースを監視し、変わったら生成 → ビルド → 再起動する。
// フロントエンドは Vite dev server 側の HMR が担うので、こちらは backend の
// ループだけを回す。
//
//	make plugin-dev PLUGIN=plugins/status
//
// # なぜ専用のツールが要るのか
//
// 手でやると「生成を忘れて古いプラグインが入ったまま」「フロントだけ直して
// backend を再起動し忘れる」といった取り違えが起きる。開発ループの速さが
// プラグインを書く人の数を左右するので、1 コマンドで済ませる。
//
// # 監視対象を絞る理由
//
// mk-go 本体まで監視すると、本体を触っている間ずっと再起動し続ける。
// プラグインのディレクトリだけを見る。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"
)

// debounce collapses bursts of file events into one rebuild.
//
// エディタは保存 1 回で複数のイベントを出す (書き込み + rename + chmod)。
// そのたびにビルドすると無駄が多い。
const debounce = 300 * time.Millisecond

func main() {
	pluginDir := flag.String("plugin", "", "監視するプラグインのディレクトリ (既定: plugins/ 全体)")
	configPath := flag.String("config", ".config/default.yml", "mk-go の設定ファイル")
	flag.Parse()

	watch := *pluginDir
	if watch != "" && filepath.IsAbs(watch) {
		// pluginbuild の -include-disabled-dir はリポジトリ相対の完全一致で
		// 照合する。絶対パスのまま渡すと監視は動くのに disabled の包含だけが
		// 黙って外れ、「置いたのに読み込まれない」型の故障になるので直す。
		wd, err := os.Getwd()
		if err != nil {
			fmt.Fprintln(os.Stderr, "plugindev:", err)
			os.Exit(1)
		}
		rel, err := filepath.Rel(wd, watch)
		if err != nil || strings.HasPrefix(rel, "..") {
			fmt.Fprintf(os.Stderr, "plugindev: -plugin はリポジトリ内のディレクトリをリポジトリ相対で指定してください: %s\n", watch)
			os.Exit(1)
		}
		watch = rel
	}

	watchDir := watch
	if watchDir == "" {
		watchDir = "plugins"
	}
	if _, err := os.Stat(watchDir); err != nil {
		fmt.Fprintf(os.Stderr, "plugindev: %s がありません\n", watchDir)
		os.Exit(1)
	}

	d := &dev{watchDir: watchDir, pluginDir: watch, configPath: *configPath}
	if err := d.run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "plugindev:", err)
		os.Exit(1)
	}
}

type dev struct {
	watchDir string
	// pluginDir is the explicitly named plugin (-plugin), or "" when watching
	// all of plugins/.
	pluginDir  string
	configPath string

	mu      sync.Mutex
	running *exec.Cmd
}

func (d *dev) run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("監視を開始できません: %w", err)
	}
	defer w.Close() //nolint:errcheck // 終了時のみ

	if err := watchTree(w, d.watchDir); err != nil {
		return err
	}

	d.banner()
	if err := d.rebuildAndRestart(ctx); err != nil {
		// 初回の失敗では終わらない。直せば次の保存で立ち上がる。
		fmt.Fprintln(os.Stderr, "  ✗", err)
	}

	var timer *time.Timer
	for {
		select {
		case <-ctx.Done():
			d.stopServer()
			fmt.Println("\nplugindev: 終了しました")
			return nil

		case ev, ok := <-w.Events:
			if !ok {
				return nil
			}
			if !interesting(ev) {
				continue
			}
			// 新しいディレクトリが増えたら監視に足す (frontend/ を後から作る等)。
			if ev.Op&fsnotify.Create != 0 {
				if fi, err := os.Stat(ev.Name); err == nil && fi.IsDir() {
					_ = watchTree(w, ev.Name)
				}
			}
			if timer != nil {
				timer.Stop()
			}
			timer = time.AfterFunc(debounce, func() {
				fmt.Printf("\n==> 変更を検出: %s\n", filepath.Base(ev.Name))
				if err := d.rebuildAndRestart(ctx); err != nil {
					fmt.Fprintln(os.Stderr, "  ✗", err)
				}
			})

		case err, ok := <-w.Errors:
			if !ok {
				return nil
			}
			fmt.Fprintln(os.Stderr, "plugindev: 監視エラー:", err)
		}
	}
}

func (d *dev) banner() {
	fmt.Printf(`plugindev: %s を監視します

  frontend の HMR は Vite dev server が担います。別の端末で起動してください:
    cd frontend/packages/frontend && pnpm watch

  backend はここで自動的に再ビルド・再起動します。
  Ctrl-C で終了します。

`, d.watchDir)
}

// rebuildAndRestart regenerates, builds, and restarts mk-go.
func (d *dev) rebuildAndRestart(ctx context.Context) error {
	// **生成を毎回走らせる。** プラグインを足した / 消したときに、生成物が
	// 古いままだと「置いたのに読み込まれない」で悩むことになる。
	//
	// GOWORK=off にするのは、消したプラグインを参照する go.work が残っていると
	// 生成ツール自体が起動できなくなるため。
	args := []string{"run", "./tools/pluginbuild"}
	if d.pluginDir != "" {
		// -plugin で名指しした監視対象は elythia-plugin.yml で disabled でも含める。
		// 既定無効の同梱サンプルを、tracked ファイルの編集 (= dirty tree) を
		// 強いずに開発できるようにするため。
		//
		// **名指しした 1 つに限る** (-include-disabled にしない)。disabled で
		// 退避中の壊れたプラグインまで巻き込むと、無関係な開発の生成・ビルドが
		// 止まる。
		args = append(args, "-include-disabled-dir", d.pluginDir)
	}
	if err := d.exec(ctx, "go", args, "GOWORK=off"); err != nil {
		return fmt.Errorf("生成に失敗しました: %w", err)
	}

	bin := filepath.Join(os.TempDir(), "mk-plugindev")
	// **GOWORK を明示的に戻す。** 生成物が import するプラグインのモジュールは go.mod に
	// 無く go.work 経由でしか解決できないので、呼び出し側から GOWORK=off を
	// 継承すると `no required module provides package` で落ちる。空文字列は Go では
	// auto 扱いで、exec.Cmd の env は後勝ちなので上の GOWORK=off を打ち消せる。
	if err := d.exec(ctx, "go", []string{"build", "-o", bin, "./cmd/elythia"}, "GOWORK="); err != nil {
		return fmt.Errorf("ビルドに失敗しました: %w", err)
	}

	d.stopServer()
	return d.startServer(bin)
}

// exec runs a command, forwarding its output.
func (d *dev) exec(ctx context.Context, name string, args []string, env ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// startServer launches mk-go in dev mode.
func (d *dev) startServer(bin string) error {
	cmd := exec.Command(bin, "serve", "-config", d.configPath)
	// **MK_DEV=1 を必ず立てる。** ビルド済みのフロントが残っていても、
	// dev では Vite dev server を見に行くようにする (#2477)。
	cmd.Env = append(os.Environ(), "MK_DEV=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// プロセスグループを分けて、子ごと確実に止められるようにする。
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("起動に失敗しました: %w", err)
	}
	d.mu.Lock()
	d.running = cmd
	d.mu.Unlock()

	go func() {
		_ = cmd.Wait()
	}()
	return nil
}

// stopServer terminates the running mk-go, if any.
func (d *dev) stopServer() {
	d.mu.Lock()
	cmd := d.running
	d.running = nil
	d.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}
	// プロセスグループごと止める。mk-go が子を持っていても取り残さない。
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		// graceful shutdown が終わらないときは落とす。開発中に前の
		// プロセスが port を掴んだままだと次が起動できない。
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// watchTree adds dir and its subdirectories to the watcher.
func watchTree(w *fsnotify.Watcher, dir string) error {
	return filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		// 生成物や依存は監視しない。触るたびに再ビルドが走る。
		switch entry.Name() {
		case "node_modules", ".git", "dist", "built":
			return filepath.SkipDir
		}
		return w.Add(path)
	})
}

// interesting reports whether an event should trigger a rebuild.
func interesting(ev fsnotify.Event) bool {
	// chmod だけの変更は中身が変わっていない。
	if ev.Op == fsnotify.Chmod {
		return false
	}
	name := filepath.Base(ev.Name)
	// エディタの一時ファイル。保存のたびに作られては消える。
	if strings.HasPrefix(name, ".") || strings.HasSuffix(name, "~") {
		return false
	}
	switch filepath.Ext(name) {
	case ".go", ".ts", ".vue", ".yml", ".yaml", ".mod", ".sum":
		return true
	}
	return false
}
