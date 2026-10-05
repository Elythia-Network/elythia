package maintenance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/core/drive"
	"github.com/elythia-network/elythia/internal/core/emojiapplication"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/safehttp"
)

// SystemFileCopier duplicates a user-owned drive file as a system-owned one and
// removes such a copy again.
//
// **承認経路 (#2966) と同じ実装を渡すこと。** `*drive.Service` がそのまま満たす。
// ここで別の複製処理を書くと、片方だけ直したときに「新規の承認は system 所有、
// 既存データの修復は利用者所有」という食い違いが黙って生まれる。
type SystemFileCopier interface {
	CopyToSystemFile(ctx context.Context, src *model.DriveFile, name string, sensitive bool, max int64) (*model.DriveFile, error)
	DeleteSystemFile(id string) error
}

// EmojiSystemFileOutcome classifies what happened to one approved application.
type EmojiSystemFileOutcome string

const (
	// EmojiSystemFileCopied means a system-owned copy was created and the emoji
	// now points at it. In a dry-run it means the row would be copied.
	EmojiSystemFileCopied EmojiSystemFileOutcome = "copied"
	// EmojiSystemFileAlready means the emoji already referenced a system-owned
	// file, so nothing was done.
	EmojiSystemFileAlready EmojiSystemFileOutcome = "already"
	// EmojiSystemFileSkipped means there is nothing left to protect (the emoji
	// was deleted after approval).
	EmojiSystemFileSkipped EmojiSystemFileOutcome = "skipped"
	// EmojiSystemFileNeedsReview means the emoji is still exposed to someone
	// else's drive file, but this batch must not repair it on its own.
	EmojiSystemFileNeedsReview EmojiSystemFileOutcome = "needs-review"
	// EmojiSystemFileUnrepairable means the original image cannot be recovered,
	// so re-running will not help.
	EmojiSystemFileUnrepairable EmojiSystemFileOutcome = "unrepairable"
	// EmojiSystemFileFailed means the repair was attempted and failed for a
	// reason that a later run can get past.
	EmojiSystemFileFailed EmojiSystemFileOutcome = "failed"
)

// EmojiSystemFileEntry is one application's result.
type EmojiSystemFileEntry struct {
	ApplicationID string
	EmojiID       string
	EmojiName     string
	Outcome       EmojiSystemFileOutcome
	Reason        string
	// CopiedFileID is the system-owned drive file created for this row. Empty
	// unless Outcome is EmojiSystemFileCopied and the run actually wrote.
	CopiedFileID string
}

// EmojiSystemFileBackfillResult aggregates one run.
type EmojiSystemFileBackfillResult struct {
	Scanned      int
	Copied       int
	Already      int
	Skipped      int
	NeedsReview  int
	Unrepairable int
	Failed       int
	Entries      []EmojiSystemFileEntry
}

// NeedsAttention reports whether the run left something a human must look at.
// CLI はこれを exit code に使う。
//
// **`skipped` だけは含めない。** そこに入るのは「承認後にモデレーターが絵文字を
// 削除した」— 守るべき絵文字がもう無いので何も残っていない状態で、運用していれば
// ふつうに起きる。これで毎回赤くすると本物の要対応が埋もれる。
//
// **逆に `needs-review` は含める。** あちらは「まだ他人の drive ファイルを
// 参照している = この issue が消そうとしている状態のまま」で、バッチが直せない
// だけ。exit code に出さないと「対象は全部片付いた」と読まれる。
func (r EmojiSystemFileBackfillResult) NeedsAttention() bool {
	return r.Unrepairable > 0 || r.Failed > 0 || r.NeedsReview > 0
}

// EmojiSystemFileBackfillOptions tunes one run.
//
// **ゼロ値が安全側 (= 何も書かない) になるようにする。** `DryRun bool` にすると
// `Options{}` が「全部書く」を意味し、呼び出し側が 1 箇所で `!apply` を書き忘れた
// だけで既定が反転する。
type EmojiSystemFileBackfillOptions struct {
	// Apply must be set for the run to write anything.
	Apply bool
	// Limit caps the number of applications examined (0 = no cap).
	//
	// **進捗を進める道具ではない。** 修復済みの行も `already` として枠を消費する
	// ので、`-limit 1` を繰り返しても先頭の行を見続ける。最初の 1 件だけ様子を
	// 見たいとき用。
	Limit int
	// MaxCopyBytes caps the body read from storage. 0 falls back to
	// emojiapplication.MaxEmojiCopyBytes — 申請側 (checkFile) と同じ上限。
	MaxCopyBytes int64
	// Now supplies emoji.updatedAt. nil uses time.Now.
	Now func() time.Time
}

// BackfillEmojiSystemFiles repoints emoji created by approved `kind = own`
// applications at a system-owned copy of the applicant's drive file (#2990).
//
// #2966 は**承認経路だけ**を直したので、それ以前に承認された絵文字は申請者所有の
// drive ファイルの URL を参照したままになっている。申請者がそのファイルを消すか、
// アカウントを消した時点で表示できなくなる。`emoji` テーブルは drive ファイル ID を
// 持たず `originalUrl` / `publicUrl` の文字列しか持たないので、参照元をたどって
// 保護する処理は既存に無い。
//
// **申請者所有の元ファイルは読むだけで、変更も移動も削除もしない。**
// **`emoji_application.fileId` も書き換えない** — 申請時に利用者が提出したファイル、
// という意味を保つ (#2966 と同じ方針)。
//
// 冪等。途中で落ちても、作れた複製の分だけ進んだ状態から再実行できる。
func BackfillEmojiSystemFiles(ctx context.Context, db *gorm.DB, copier SystemFileCopier, opts EmojiSystemFileBackfillOptions) (EmojiSystemFileBackfillResult, error) {
	var res EmojiSystemFileBackfillResult
	if db == nil {
		return res, errors.New("maintenance: db is nil")
	}
	if copier == nil {
		return res, errors.New("maintenance: system file copier is nil")
	}
	maxBytes := opts.MaxCopyBytes
	if maxBytes <= 0 {
		maxBytes = emojiapplication.MaxEmojiCopyBytes
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	// **問い合わせにも ctx を効かせる。** 中断したのに DB を叩き続けない。
	db = db.WithContext(ctx)

	apps, err := findApprovedOwnApplications(db, opts.Limit)
	if err != nil {
		return res, fmt.Errorf("list approved own applications: %w", err)
	}

	for _, app := range apps {
		// **中断は「残り全部が失敗」にしない。** ctx を見ないと、Ctrl-C のあと
		// 残りの行が lookup の失敗で `failed` になり、要対応として大量に出る。
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.Scanned++
		entry := processEmojiSystemFile(ctx, db, copier, app, maxBytes, nowFn, opts.Apply)
		res.Entries = append(res.Entries, entry)
		switch entry.Outcome {
		case EmojiSystemFileCopied:
			res.Copied++
		case EmojiSystemFileAlready:
			res.Already++
		case EmojiSystemFileSkipped:
			res.Skipped++
		case EmojiSystemFileNeedsReview:
			res.NeedsReview++
		case EmojiSystemFileUnrepairable:
			res.Unrepairable++
		case EmojiSystemFileFailed:
			res.Failed++
		}
	}
	return res, nil
}

// findApprovedOwnApplications lists the candidate rows in a stable order.
//
// **issue の検出 SQL は `JOIN drive_file` + `df."userId" IS NOT NULL` だが、
// そのままでは「元ファイルが既に削除されている対象」が 1 行も列挙されない。**
// それを一覧に出すのがこのバッチの要件の 1 つなので、join せずに 1 行ずつ解決する。
func findApprovedOwnApplications(db *gorm.DB, limit int) ([]*model.EmojiApplication, error) {
	q := db.Model(&model.EmojiApplication{}).
		Where("kind = ?", model.EmojiApplicationKindOwn).
		Where("status = ?", model.EmojiApplicationApproved).
		Where(`"emojiId" IS NOT NULL`).
		Where(`"fileId" IS NOT NULL`).
		Order("id ASC")
	if limit > 0 {
		q = q.Limit(limit)
	}
	var apps []*model.EmojiApplication
	if err := q.Find(&apps).Error; err != nil {
		return nil, err
	}
	return apps, nil
}

// processEmojiSystemFile classifies and (when apply) repairs one row.
func processEmojiSystemFile(
	ctx context.Context,
	db *gorm.DB,
	copier SystemFileCopier,
	app *model.EmojiApplication,
	maxBytes int64,
	nowFn func() time.Time,
	apply bool,
) EmojiSystemFileEntry {
	entry := EmojiSystemFileEntry{
		ApplicationID: app.ID,
		EmojiID:       derefString(app.EmojiID),
		EmojiName:     app.Name,
	}

	emoji, err := findEmojiByID(db, derefString(app.EmojiID))
	if err != nil {
		return classify(entry, EmojiSystemFileFailed, fmt.Sprintf("絵文字の読み出しに失敗: %v", err))
	}
	if emoji == nil {
		// **承認後にモデレーターが消した形。** `emojiId` は NULL に落ちない
		// (migration が FK を張っていない) ので、id はあるのに引けない。
		// 守るべき絵文字がもう無いので複製しない。
		return classify(entry, EmojiSystemFileSkipped, "絵文字が既に削除されている")
	}
	entry.EmojiName = emoji.Name

	// **冪等性の判定はここ。** 申請の `fileId` は書き換えないので、複製済みかどうかは
	// 申請側からは分からない。絵文字が今どの実体を指しているかで見る。
	current, err := findDriveFilesByURL(db, emoji.OriginalURL)
	if err != nil {
		return classify(entry, EmojiSystemFileFailed, fmt.Sprintf("参照先ファイルの読み出しに失敗: %v", err))
	}
	if anySystemOwned(current) {
		return classify(entry, EmojiSystemFileAlready, "既に system 所有のファイルを参照している")
	}

	src, err := findDriveFileByID(db, derefString(app.FileID))
	if err != nil {
		return classify(entry, EmojiSystemFileFailed, fmt.Sprintf("申請ファイルの読み出しに失敗: %v", err))
	}
	if src == nil {
		// **元画像を復元できない。** 絵文字にも申請にも触らず一覧に出す。
		return classify(entry, EmojiSystemFileUnrepairable, "申請ファイルが既に削除されている")
	}
	// **「申請ファイルが既に system 所有」を別扱いにしない。** それが起きるなら
	// 絵文字も同じ URL を指しているはずで、上の `already` が拾っている
	// (`findDriveFilesByURL` は同じ url の行を全部見る)。指していないなら、
	// 下の `needs-review` が正しい — 申請ファイルの所有者が何であれ、**絵文字は
	// system 所有のファイルを参照していない**。ここで申請ファイル側の所有者を見て
	// skip すると、絵文字の分類が**無関係なファイル**の所有者で決まってしまい、
	// まだ壊れうる絵文字が exit code から落ちる。
	if emoji.OriginalURL != src.URL {
		// **申請経由でない差し替えには触らない。** モデレーターが
		// `admin/emoji/update` で別の画像に差し替えると、絵文字はもう申請ファイルを
		// 参照していない。そこを申請ファイルで上書きすると差し替えを巻き戻す。
		//
		// **ただし放置でもない。** ここに来た時点で「system 所有のファイルを
		// 参照していない」ことは確定している (上の `already` を通過している) ので、
		// その絵文字は今も誰かの drive 操作で壊れうる。直すのはこのバッチの対象外
		// だが、要対応としては出す。**直し方は差し替え** — #3014 から
		// `admin/emoji/update` も差し替え先を system 所有へ複製するので、
		// 次の実行ではその絵文字は `already` に落ちる。
		return classify(entry, EmojiSystemFileNeedsReview,
			"絵文字が申請ファイルを参照していない (モデレーターが差し替えた可能性)。"+
				"参照先は system 所有ではないので、admin/emoji/update で画像を差し替え直すこと "+
				"(#3014 から差し替え先も system 所有へ複製する)。docs/deployment.md 参照")
	}
	if !emojiapplication.IsAllowedImageType(src.Type) {
		return classify(entry, EmojiSystemFileUnrepairable,
			fmt.Sprintf("申請ファイルの MIME が絵文字として許可されていない (%s)", src.Type))
	}
	// **行だけで分かる「複製できない」は読む前に判定する。** そうしないと
	// dry-run が「複製する」と言い、本実行で初めて unrepairable に化ける。
	if reason := unrepairableFromRow(src); reason != "" {
		return classify(entry, EmojiSystemFileUnrepairable, reason)
	}

	if !apply {
		return classify(entry, EmojiSystemFileCopied, "複製する (dry-run のため書き込みなし)")
	}

	// **sensitive は申請と絵文字の両方を見る。** 承認後にモデレーターが絵文字側で
	// 付け直していることがあり、複製にも同じ印を残しておくと管理画面で分かる。
	copied, err := copier.CopyToSystemFile(ctx, src, app.Name, app.IsSensitive || emoji.IsSensitive, maxBytes)
	if err != nil {
		// **失敗の種別を潰さない (#2792)。** 分ける基準は「再実行で直るか」。
		switch {
		case errors.Is(err, drive.ErrObjectNotFound):
			// 実体がもう無い。何度流しても同じなので人が判断する。
			return classify(entry, EmojiSystemFileUnrepairable,
				fmt.Sprintf("申請ファイルの実体がストレージに無い: %v", err))
		case errors.Is(err, safehttp.ErrResponseTooLarge):
			// **再実行では直らないので `failed` にしない。** 複製の上限
			// (`MaxEmojiCopyBytes`) を超える画像は、role policy の
			// `maxFileSizeMb` をそれより上へ設定していた時期に承認されたもの。
			// 待っても縮まないので、`admin/emoji/update` で別の画像へ差し替える
			// しかない (#3014 から差し替え先も system 所有へ複製する)。
			return classify(entry, EmojiSystemFileUnrepairable,
				fmt.Sprintf("申請ファイルが複製の上限 (%d bytes) を超えている: %v", maxBytes, err))
		}
		// ストレージや DB の障害。原因を取り除いて再実行する。
		return classify(entry, EmojiSystemFileFailed, fmt.Sprintf("複製に失敗: %v", err))
	}
	if copied == nil {
		return classify(entry, EmojiSystemFileFailed, "複製が返らなかった")
	}

	// **複製した実体の MIME を見る。** `Upload` は `AnalyseFile` でバイト列から型を
	// 引き直すので、元の行の宣言と実体がずれていると allowlist 外の型が絵文字として
	// 登録される (#2966 の承認経路も同じ理由で取り込み後に見ている)。
	if !emojiapplication.IsAllowedImageType(copied.Type) {
		deleteCopy(copier, copied.ID, &entry)
		return classify(entry, EmojiSystemFileUnrepairable,
			fmt.Sprintf("複製した実体の MIME が絵文字として許可されていない (%s)", copied.Type))
	}
	// **URL が `emoji` の列に入るかを見る (#3023)。** `drive_file.url` は
	// varchar(1024) だが `emoji.originalUrl` / `publicUrl` は varchar(512) で、
	// 長い prefix のオブジェクトストレージ構成では超える。見ないと下の `Updates` が
	// SQLSTATE 22001 で落ち、**再実行では直らないのに `failed` (= 再実行を促す)**
	// に分類される。上限超過を `unrepairable` に分けているのと同じ判断。
	//
	// **ここまで来ないことが多い** — `Put` が返す URL は `base (+ prefix) + accessKey`
	// (32 桁の hex 固定) なので `url` / `thumbnailUrl` / `webpublicUrl` の長さは必ず
	// 同じで、`drive_file` 側の後ろ 2 つは varchar(512)。つまりサムネイルを作る画像は
	// 上の `CopyToSystemFile` が先に落ち、こちらは `failed` (複製に失敗) になる。
	// 同じ理由で `publicUrl` 側の判定も現状は冗長だが、導出を変えたときに素通り
	// させないために両方見る。
	if !colfit.Fits(copied.URL, emojiURLMaxRunes) ||
		!colfit.Fits(drive.PreferWebpublicURL(copied), emojiURLMaxRunes) {
		deleteCopy(copier, copied.ID, &entry)
		return classify(entry, EmojiSystemFileUnrepairable,
			"複製の URL が emoji.originalUrl / publicUrl (varchar(512)) に入らない。保存先の設定を短くするしかない")
	}

	now := nowFn()
	// **条件付き更新。** 走っている間に誰かが同じ絵文字の画像を差し替えていたら、
	// その変更を上書きしない。
	tx := db.Model(&model.Emoji{}).
		Where(`id = ? AND "originalUrl" = ?`, emoji.ID, emoji.OriginalURL).
		Updates(map[string]any{
			// **`originalUrl` は複製の `url` と一致させる。** 孤児 cleanup は
			// `emoji.originalUrl = drive_file.url` または `publicUrl = url` を
			// 参照保護の条件にしているので、webpublic だけを入れると保護が外れる。
			"originalUrl": copied.URL,
			"publicUrl":   drive.PreferWebpublicURL(copied),
			"type":        drive.PreferWebpublicType(copied),
			"updatedAt":   now,
		})
	if tx.Error != nil {
		return finishFailedUpdate(db, copier, entry, emoji.ID, copied, tx.Error)
	}
	if tx.RowsAffected == 0 {
		// **他の書き込みが先に載った。** 再実行すれば `already` か
		// `needs-review` に落ち着くので、`failed` (= 再実行で直る) に入れる。
		deleteCopy(copier, copied.ID, &entry)
		return classify(entry, EmojiSystemFileFailed, "更新中に絵文字が変更された (競合)。再実行すること")
	}

	entry.CopiedFileID = copied.ID
	entry.Outcome = EmojiSystemFileCopied
	return entry
}

// finishFailedUpdate decides what to do when the conditional UPDATE returned an
// error.
//
// **エラーが返っても適用されていることがある。** PostgreSQL は COMMIT を送った
// 後・ack の前に接続が切れると、サーバー側は commit 済みなのにクライアントは
// エラーを受け取る。そのまま複製を消すと**絵文字が存在しないファイルを指す**
// (画像が 404 になり、以後の実行は `needs-review` に落ちて自動では直らない)。
//
// **読み直せないときは消さない。** 参照されている複製を消すほうが、参照されない
// 複製を残すより悪い — 後者は drive の孤児 cleanup が回収する。
func finishFailedUpdate(
	db *gorm.DB,
	copier SystemFileCopier,
	entry EmojiSystemFileEntry,
	emojiID string,
	copied *model.DriveFile,
	updateErr error,
) EmojiSystemFileEntry {
	landed, err := findEmojiByID(db, emojiID)
	switch {
	case err != nil:
		return classify(entry, EmojiSystemFileFailed,
			fmt.Sprintf("絵文字の更新に失敗し、載ったかも確認できない (複製 %s は残した。参照されていなければ孤児 cleanup が回収する): %v / %v",
				copied.ID, updateErr, err))
	case landed != nil && landed.OriginalURL == copied.URL:
		entry.CopiedFileID = copied.ID
		entry.Outcome = EmojiSystemFileCopied
		entry.Reason = fmt.Sprintf("更新はエラーを返したが適用されていた: %v", updateErr)
		return entry
	}
	deleteCopy(copier, copied.ID, &entry)
	return classify(entry, EmojiSystemFileFailed, fmt.Sprintf("絵文字の更新に失敗: %v", updateErr))
}

// unrepairableFromRow reports why src can never be copied, using only structural
// facts on the drive row. Returns "" when nothing rules it out.
func unrepairableFromRow(src *model.DriveFile) string {
	switch {
	case src.IsLink:
		return "申請ファイルがリンク行で実体を持たない"
	case src.AccessKey == nil || *src.AccessKey == "":
		return "申請ファイルに accessKey が無く実体を辿れない"
	case src.URL == "":
		// **url が空の行は複製しない。** 絵文字側の `originalUrl` も空だと
		// 「参照先を引く」も「URL が一致するか」も判定にならず、上の `already`
		// を素通りして毎回新しい複製を作る。
		return "申請ファイルに url が無い"
	}
	// **`size` は見ない。** あの列は行に書いてあるだけで実体の権威ではなく
	// (TS 由来の行や手で直した行ではずれうる)、`unrepairable` は「絵文字を消すか
	// 差し替えろ」という取り返しのつかない案内に繋がる。大きすぎるかどうかは
	// 実際に読んだ結果 (`safehttp.ErrResponseTooLarge`) だけで判定する。
	return ""
}

// deleteCopy removes a copy created by a repair that then failed.
//
// **消せなかったら理由に足す。** 後始末の失敗で分類は変えない (審査の結果と同じで、
// 呼び出し元のエラーを上書きしない) が、残った孤児を運用側が追えるようにする。
// 残ったものは絵文字から参照されないので drive の孤児 cleanup が回収する。
func deleteCopy(copier SystemFileCopier, fileID string, entry *EmojiSystemFileEntry) {
	if fileID == "" {
		return
	}
	if err := copier.DeleteSystemFile(fileID); err != nil {
		entry.Reason = fmt.Sprintf("複製 %s の後始末に失敗: %v / ", fileID, err)
	}
}

// classify stamps an outcome + reason onto entry and returns it.
func classify(entry EmojiSystemFileEntry, outcome EmojiSystemFileOutcome, reason string) EmojiSystemFileEntry {
	entry.Outcome = outcome
	entry.Reason = entry.Reason + reason
	return entry
}

// emojiURLMaxRunes は `emoji.originalUrl` / `publicUrl` の列幅 (#3023)。
// `drive_file.url` は varchar(1024) と広いので、保存先次第で超えうる。
// DDL 側の実値は `internal/repository/emoji_column_limits_test.go` が突き合わせる。
const emojiURLMaxRunes = 512

// anySystemOwned reports whether any of files belongs to the instance itself.
//
// **判定は `drive.IsSystemOwned` に寄せる。** `admin/emoji/add` (#2999) と
// `admin/emoji/update` (#3014) も同じ条件で「複製が要るか」を決めるので、ここに
// 自前の定義を置くと片方だけ直したときに「新規の登録は複製するのに、既存データの
// 検査は複製済みと見なす」形でずれる。
func anySystemOwned(files []*model.DriveFile) bool {
	for _, f := range files {
		if drive.IsSystemOwned(f) {
			return true
		}
	}
	return false
}

// findEmojiByID returns nil (no error) when the row is gone.
func findEmojiByID(db *gorm.DB, id string) (*model.Emoji, error) {
	if id == "" {
		return nil, nil
	}
	var e model.Emoji
	err := db.Where("id = ?", id).Take(&e).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// findDriveFileByID returns nil (no error) when the row is gone.
func findDriveFileByID(db *gorm.DB, id string) (*model.DriveFile, error) {
	if id == "" {
		return nil, nil
	}
	var f model.DriveFile
	err := db.Where("id = ?", id).Take(&f).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &f, nil
}

// findDriveFilesByURL resolves the rows whose canonical `url` equals raw.
//
// **`publicUrl` では引かない。** 孤児 cleanup は `originalUrl = url` と
// `publicUrl = url` の両方を見るが、ここが知りたいのは「絵文字が指している実体の
// 所有者」で、`originalUrl` は必ず `drive_file.url` と一致するのが #2966 以降の
// 不変条件。webpublic 側で引くと、webpublic を持つ別のファイルに当たりうる。
//
// **1 行に決め打たない。件数も絞らない。** `drive_file.url` に一意制約は無いので、
// `Take` だと重複時に任意の 1 行が返り、system 所有の行を引けば偽の「複製済み」、
// 利用者所有の行を引けば偽の「要確認」になる。`Limit` を付けるのも同じ形で、
// system 所有の行がその外に落ちると**実行のたびに新しい複製を作る** (冪等でなくなる)。
// 同じ url の行は現実には 1 つなので、全部見ても読む量は変わらない。
func findDriveFilesByURL(db *gorm.DB, raw string) ([]*model.DriveFile, error) {
	if raw == "" {
		return nil, nil
	}
	var rows []*model.DriveFile
	if err := db.Where("url = ?", raw).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
