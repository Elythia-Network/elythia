package backfill

import (
	"fmt"
	"io"

	"github.com/elythia-network/elythia/internal/cli/cliflag"
	coredrive "github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/maintenance"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/repository"
)

// **承認経路と同じ複製を使っていることを型で固定する。** ここが満たされなくなるのは
// `drive.Service` 側の複製が消えたか形が変わったときで、そうなると別実装を書き足す
// 誘惑が生まれる (= 片方だけ直る形)。
var _ maintenance.SystemFileCopier = (*coredrive.Service)(nil)

// EmojiSystemFile implements "elythia backfill emoji-system-file": it
// duplicates the applicant-owned drive file behind every already-approved
// `kind = own` custom emoji application into a system-owned drive file, and
// repoints the emoji at that copy (#2990).
//
// #2966 は**承認経路だけ**を直した。それ以前に承認された絵文字は申請者所有の drive
// ファイルの URL を参照したままなので、申請者が drive から元ファイルを消すか、
// アカウントを削除した時点で表示できなくなる。`emoji` テーブルは drive ファイル ID を
// 持たず `originalUrl` / `publicUrl` の文字列しか持たないため、参照元をたどって保護する
// 処理は既存に無い。放置すると「いつ壊れるかは申請者の操作次第」という状態が残り続ける。
//
// **申請者所有の元ファイルは読むだけで、変更も移動も削除もしない。**
// **`emoji_application.fileId` も書き換えない** (申請時に利用者が提出したファイル、
// という意味を保つ)。
//
// 冪等。途中で落ちても、作れた複製の分だけ進んだ状態から再実行できる。
//
// **既定は dry-run。** 書き込むときだけ -apply を明示する。
//
// **元画像を復元できない対象は自動修復しない** (元ファイルが削除済み / 実体が
// ストレージから消えている / 読み出しに失敗した)。該当したものは申請 ID・絵文字名・
// 理由を一覧に出し、exit code を非ゼロにして見落とさないようにする。
//
// **サーバーと同じ作業ディレクトリで実行すること。** `storedInternal = true` の行は
// ローカル FS にあり、既定の探索先は相対パスの ./drive-files (mk-go 本体と同じ)。
// 違う場所に置いている構成では -drive-dir で渡す。
//
//	elythia backfill emoji-system-file -config .config/default.yml
//	elythia backfill emoji-system-file -config .config/default.yml -apply
func EmojiSystemFile(args []string) int { return emojiSystemFile(defaultEnv(), args) }

func emojiSystemFile(e env, args []string) int {
	fs, cfgPath := newFlags(e, "emoji-system-file")
	driveDir := fs.String("drive-dir", "./drive-files", "local drive storage directory (storedInternal rows)")
	limit := fs.Int("limit", 0, "examine at most N applications (0 = no cap)")
	apply := fs.Bool("apply", false, "actually write; without this the run is a dry-run")
	// **`-dry-run` も受ける。** 既定が dry-run なので不要だが、姉妹バッチ
	// (remote-host / note-tags) はこちらの綴りで書かせる。定義していないと
	// `flag provided but not defined` で落ちるだけなので危険は無いが、手が覚えている
	// 綴りが通らないのは無駄な往復になる。
	dryRun := fs.Bool("dry-run", false, "explicitly request a dry-run (already the default)")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}
	if *dryRun && *apply {
		e.logger.Printf("-dry-run と -apply は同時に指定できない")
		return 1
	}

	cfg, db, ok := open(e, *cfgPath)
	if !ok {
		return 1
	}
	idGen, err := id.NewGenerator(cfg.ID)
	if err != nil {
		e.logger.Printf("id generator %q: %v", cfg.ID, err)
		return 1
	}

	metaRepo := repository.NewMetaRepository(db)
	// backend は meta から都度解決する (#2315)。オブジェクトストレージを有効に
	// している構成では複製も S3 側へ書く。
	storage := coredrive.NewMetaStorage(metaRepo.Fetch, *driveDir, cfg.DriveURL)
	driveSvc := coredrive.NewService(
		repository.NewDriveFileRepository(db),
		repository.NewDriveFolderRepository(db),
		storage,
		idGen,
	)
	// **storedInternal = true の行はローカル FS にある。** オブジェクトストレージへ
	// 移行する前に保存されたファイルをここから読む (#1414 / #2315)。
	driveSvc.SetLocalStorage(coredrive.NewLocalStorage(*driveDir, cfg.DriveURL))
	// **webpublic variant の生成に要る。** 無いと複製が webpublic を持たず、
	// 絵文字の publicUrl / type が原本側に落ちて元の行と食い違う。
	driveSvc.SetImageProcessor(coredrive.NewDefaultImageProcessor())

	// **Ctrl-C / SIGTERM で途中まで残す。** 中断すると残りの行は処理しないが、
	// そこまでに作った複製と絵文字の更新はそのまま生きる (冪等なので再実行で続く)。
	ctx, stop := e.signalContext()
	defer stop()

	res, err := e.emojiBackfill(ctx, db, driveSvc,
		// **`Apply` をそのまま渡す。** ここで否定を挟むと、書き間違い 1 つで
		// 既定が「全部書く」に反転する (options のゼロ値は何も書かない側)。
		maintenance.EmojiSystemFileBackfillOptions{Apply: *apply, Limit: *limit})
	// **エラーでも途中までの結果を出す。** 捨てると、複製を作った行があったことが
	// 運用者に伝わらない (次の実行で `already` になるだけなので壊れはしないが、
	// 中断のたびに「何も起きなかった」ように見える)。
	report(e.stderr, res, *apply, err)
	if err != nil {
		e.logger.Printf("backfill: %v", err)
		return 1
	}
	if res.NeedsAttention() {
		return 1
	}
	return 0
}

// report prints every non-trivial row and the totals.
//
// **一覧は要対応だけに絞らない。** skip した理由 (絵文字が削除済み) は毎回ふつうに
// 出るが、黙って落とすと「対象 3 件のはずが 1 件しか出ない」ときに理由が分からなく
// なる。exit code に効くのは要対応だけ。
func report(w io.Writer, res maintenance.EmojiSystemFileBackfillResult, applied bool, runErr error) {
	mode := "dry-run (no writes)"
	copiedLabel := "would-copy"
	if applied {
		mode = "applied"
		copiedLabel = "copied"
	}
	// **落ちた実行を `done` と書かない。** 列挙の段で落ちると件数はすべて 0 に
	// なるので、`done ... failed=0` は「何も要対応が無かった」と読めてしまう。
	verb := "done"
	if runErr != nil {
		verb = "aborted"
	}
	for _, e := range res.Entries {
		// 既に複製済みの行は毎回全件出るだけで読むところが無い。件数は totals に出る。
		if e.Outcome == maintenance.EmojiSystemFileAlready {
			continue
		}
		outcome := string(e.Outcome)
		if !applied && e.Outcome == maintenance.EmojiSystemFileCopied {
			outcome = copiedLabel
		}
		fmt.Fprintf(w, "%-12s application=%s emoji=%s name=%q copiedFile=%s %s\n",
			outcome, e.ApplicationID, e.EmojiID, e.EmojiName, e.CopiedFileID, e.Reason)
	}
	fmt.Fprintf(w, "%s [%s]: scanned=%d %s=%d already=%d skipped=%d needs-review=%d unrepairable=%d failed=%d\n",
		verb, mode, res.Scanned, copiedLabel, res.Copied, res.Already, res.Skipped,
		res.NeedsReview, res.Unrepairable, res.Failed)
	if res.NeedsAttention() {
		fmt.Fprintf(w, "要対応が %d 件ある: unrepairable と needs-review は再実行しても直らないので、"+
			"モデレーターが admin/emoji/update で画像を差し替え直すか、絵文字を削除する "+
			"(#3014 から差し替え先も system 所有へ複製する。手順は docs/deployment.md。"+
			"承認済みの申請は却下できない)。failed は原因を取り除いて再実行する\n",
			res.Unrepairable+res.NeedsReview+res.Failed)
	}
}
