package backfill

import "github.com/elythia-network/elythia/internal/cli/cliflag"

// AvatarPublicURL implements "elythia backfill avatar-public-url": it rewrites
// stored `user.avatarUrl` / `user.bannerUrl` to the public (metadata-stripped)
// variant of the drive file the row already points at.
//
// 既存の行は原本 (`drive_file.url`) を指している。原本はアップロードされた
// バイト列そのままで EXIF / XMP が載っており、`avatarUrl` はタイムラインや
// ActivityPub の actor icon に出るため、設定したときのまま公開され続ける。
// 書き込み側の修正は以後の更新にしか効かないので、既存行はこれで流し直す。
//
// **SQL migration では書けない。** 「webpublic があればそちら、無ければ原本」
// という判定は `entity.WebpublicOrOriginalURL` が持っており、列を跨いだ
// 条件付きの写しになる (note-tags が NFKC で同じ理由)。
//
// **対象はローカル利用者だけ。** upstream はリモートのアイコンを drive に
// 保存して `avatarId` を書くので、TS から引き継いだ DB にはリモート利用者の
// 古い id が残っている。host で絞らないと、mk-go が actor から取り直した
// 現在の URL を TS 時代のキャッシュへ巻き戻す。
//
// 冪等なので途中で失敗しても再実行して安全。まず -dry-run で件数を見積もること。
//
//	elythia backfill avatar-public-url -config .config/default.yml -dry-run
//	elythia backfill avatar-public-url -config .config/default.yml -batch 1000 -sleep-ms 200
//	elythia backfill avatar-public-url -config .config/default.yml -from <last-user-id>
func AvatarPublicURL(args []string) int { return avatarPublicURL(defaultEnv(), args) }

func avatarPublicURL(e env, args []string) int {
	fs, cfgPath := newFlags(e, "avatar-public-url")
	batchSize := fs.Int("batch", 1000, "rows per keyset batch")
	sleepMs := fs.Int("sleep-ms", 100, "sleep between batches to limit DB load")
	fromID := fs.String("from", "", "resume from this user id (exclusive)")
	dryRun := fs.Bool("dry-run", false, "count changes without writing")
	if code, ok := cliflag.Parse(fs, args); !ok {
		return code
	}

	_, db, ok := open(e, *cfgPath)
	if !ok {
		return 1
	}

	cursor := *fromID
	var scanned, updated int
	for {
		res, err := e.avatarBatch(db, cursor, *batchSize, *dryRun)
		if err != nil {
			// **件数とカーソルの基準を揃える。** 失敗したバッチの途中までを
			// scanned に足すと、-from が指す位置 (そのバッチの開始) と食い違う。
			// 冪等なのでこの位置から流し直して安全。
			e.logger.Printf("backfill failed (scanned=%d before this batch; resume with -from %q): %v",
				scanned, cursor, err)
			return 1
		}
		scanned += res.Scanned
		updated += res.Updated
		if res.Scanned == 0 {
			break
		}
		cursor = res.LastID
		e.logger.Printf("scanned=%d updated=%d cursor=%s", scanned, updated, cursor)
		pause(e, *sleepMs)
	}

	e.logger.Printf("done (%s): scanned=%d updated=%d", modeLabel(*dryRun, "dry-run"), scanned, updated)
	return 0
}
