package maintenance

import (
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
)

// AvatarPublicURLBackfillResult reports the outcome of one keyset batch.
type AvatarPublicURLBackfillResult struct {
	Scanned int // users inspected this batch
	// Updated counts users whose stored URL differed from the public one.
	//
	// **「書けた件数」ではなく「書く必要があった件数」。** 読んでから撃つまでの
	// 間に利用者がアイコンを変えていると下の CAS が空振りするが、それはここに
	// 出さない — dry-run は衝突を知りようがないので、出すと dry-run と本実行で
	// 意味の違う数が並ぶ。空振りした行は冪等なので次回の実行が拾う。
	Updated int
	LastID  string // greatest user id seen; "" when no rows remained
}

// BackfillAvatarPublicURLBatch rewrites one keyset batch of `user.avatarUrl` /
// `user.bannerUrl` to the public (metadata-stripped) variant of the drive file
// the row already points at.
//
// 既存の行は原本 (`drive_file.url`) を指している。原本はアップロードされた
// バイト列そのままなので EXIF / XMP が載っており、`avatarUrl` はタイムラインや
// ActivityPub の actor icon に出るため、設定したときのまま公開され続ける。
// 書き込み側は直したが、**それは以後の更新にしか効かない。**
//
// **対象はローカル利用者 (`host IS NULL`) だけ。** mk-go はリモートのアイコンを
// drive に保存しないので `avatarId` を書かないが、**upstream は保存して書く**
// (`ApPersonService` の `avatarId: avatar.id`)。TS から引き継いだ DB には
// リモート利用者の古い `avatarId` が残っており、mk-go の `refreshActor` は
// `avatarUrl` しか更新しない (`core/federation/resolver.go`) ので id はそのまま
// 残り続ける。ここで host を見ないと、**mk-go が actor から取り直した現在の
// リモート URL を、TS 時代のキャッシュへ巻き戻す**。しかも mk-go の
// リモートキャッシュ掃除は accessKey を NULL にして実体を消すので、
// 書き戻した URL は後で 404 になる。
//
// **参照先を引けなかった行は触らない。** `user.avatarId` の FK は
// `ON DELETE SET NULL` なので、ファイルを消せば id ごと NULL になり上の WHERE から
// 外れる。引けないのは user と drive_file を別々のクエリで読む間に削除が入った
// ときだけで、次回の実行で自然に整合する。
//
// 冪等。`LastID` を次回の fromID に渡せば再開できる。`dryRun` は UPDATE を
// 撃たずに件数だけ数える。
func BackfillAvatarPublicURLBatch(db *gorm.DB, fromID string, batchSize int, dryRun bool) (AvatarPublicURLBackfillResult, error) {
	batchSize = clampBatchSize(batchSize)

	var users []*model.User
	err := db.Model(&model.User{}).
		Select(`"id"`, `"host"`, `"avatarId"`, `"avatarUrl"`, `"bannerId"`, `"bannerUrl"`).
		Where(`"id" > ? AND "host" IS NULL AND ("avatarId" IS NOT NULL OR "bannerId" IS NOT NULL)`, fromID).
		Order(`"id" ASC`).
		Limit(batchSize).
		Find(&users).Error
	if err != nil {
		return AvatarPublicURLBackfillResult{}, err
	}

	fileIDs := make([]string, 0, len(users)*2)
	for _, u := range users {
		if u.AvatarID != nil && *u.AvatarID != "" {
			fileIDs = append(fileIDs, *u.AvatarID)
		}
		if u.BannerID != nil && *u.BannerID != "" {
			fileIDs = append(fileIDs, *u.BannerID)
		}
	}

	// 1 バッチ分の drive_file をまとめて引く。1 行ずつ引くと user 1 件あたり
	// 最大 2 往復になる。
	files := make(map[string]*model.DriveFile, len(fileIDs))
	if len(fileIDs) > 0 {
		var rows []*model.DriveFile
		if err := db.Model(&model.DriveFile{}).
			Select(`"id"`, `"type"`, `"url"`, `"webpublicUrl"`).
			Where(`"id" IN ?`, fileIDs).
			Find(&rows).Error; err != nil {
			return AvatarPublicURLBackfillResult{}, err
		}
		for _, f := range rows {
			files[f.ID] = f
		}
	}

	res := AvatarPublicURLBackfillResult{}
	for _, u := range users {
		res.Scanned++
		res.LastID = u.ID

		updates := map[string]any{}
		guards := map[string]any{}
		for _, m := range []struct {
			id       *string
			url      *string
			column   string
			idColumn string
		}{
			{u.AvatarID, u.AvatarURL, "avatarUrl", "avatarId"},
			{u.BannerID, u.BannerURL, "bannerUrl", "bannerId"},
		} {
			if m.id == nil || *m.id == "" {
				continue
			}
			f, ok := files[*m.id]
			if !ok {
				continue
			}
			// **アニメーションになりうる形式で公開用を持つ行は触らない。**
			// 公開用は 1 コマの静止画かもしれず、寄せ直すと**いま動いている
			// アニメーションのアイコンをその瞬間に静止画へ固定する**。どちらか
			// はバイト列を読まないと分からないので、読まずに済むほうへ倒す。
			// 触らなければ今の URL (原本) のままで、挙動は変わらない。
			//
			// GIF は `GenerateWebpublic` のガードが入って以降は公開用を作らない
			// (`hasStrippableMetadata` が GIF を見ないので例外は起きない) ので、
			// 公開用を持つのは過去の行。APNG はメタデータ付きだけが今も作られる。
			//
			// **AVIF は全件が外れる。** あの形式は寸法にもメタデータにも関係なく
			// 必ず公開用が作られるので、右辺が常に真になる。静止画の AVIF で
			// 撮影情報を持つものも寄せ直されない。WebP はガードが MIME 判定で
			// 拾えないため、アニメーションでも公開用を持ちうる。
			//
			// 取りこぼした行は利用者がアイコンを設定し直せば直る — ただし
			// アニメーションの AVIF と、メタデータ付き / 2048px 超のアニメーション
			// WebP は、設定し直すと静止画になる (書き込み経路も同じ MIME 判定)。
			if mayBeAnimated(f.Type) && f.WebpublicURL != nil && *f.WebpublicURL != "" {
				continue
			}

			want := entity.WebpublicOrOriginalURL(f)
			if want == "" || (m.url != nil && *m.url == want) {
				continue
			}
			updates[m.column] = want
			// **読んだときの id を UPDATE の条件に持ち込む。** 読んでから撃つまでの
			// 間に利用者がアイコンを変えると、古い id から導いた URL で新しい
			// 画像を上書きし、`avatarId` と `avatarUrl` が食い違ったまま残る。
			// 姉妹バッチ (`host_backfill`) も同じ形で旧値を条件に入れている。
			guards[m.idColumn] = *m.id
		}

		if len(updates) == 0 {
			continue
		}
		res.Updated++
		if dryRun {
			continue
		}
		if _, err := updateUserMediaURLs(db, u.ID, guards, updates); err != nil {
			return res, err
		}
	}

	return res, nil
}

// updateUserMediaURLs writes updates onto one user row, but only while every
// guard column still holds the value the caller read.
//
// **読んだときの `avatarId` / `bannerId` を条件に持ち込む。** 読んでから撃つまでの
// 間に利用者がアイコンを変えると、古い id から導いた URL で新しい画像を上書きし、
// `avatarId` と `avatarUrl` が食い違ったまま次の更新まで残る。姉妹バッチ
// (`host_backfill`) も旧値を条件に入れている。
//
// 戻り値は「実際に書けたか」。false は衝突で、冪等なので次回の実行が拾う。
func updateUserMediaURLs(db *gorm.DB, userID string, guards, updates map[string]any) (bool, error) {
	q := db.Model(&model.User{}).Where(`"id" = ?`, userID)
	for col, val := range guards {
		q = q.Where(`"`+col+`" = ?`, val)
	}
	tx := q.Updates(updates)
	if tx.Error != nil {
		return false, tx.Error
	}
	return tx.RowsAffected > 0, nil
}

// mayBeAnimated reports whether the MIME type can carry multiple frames.
//
// **形式の判定だけで、実際にアニメーションかは見ていない。** 見るにはバイト列が
// 要る。ここは「触らない」側へ倒すための粗い網なので、静止画の WebP まで
// 巻き込むのは織り込み済み (触らなければ現状維持で、壊れるものは無い)。
//
// `internal/core/drive` の `isAnimatedMime` とは目的が違うので別に持つ。あちらは
// 「webpublic を作らない」判定で、GIF と APNG に絞ることに意味がある
// (WebP / AVIF まで広げると静止画の再エンコードまで止まる)。こちらは
// 「寄せ直さない」判定なので、広いほうが安全側。
func mayBeAnimated(mime string) bool {
	switch mime {
	case "image/gif", "image/apng", "image/vnd.mozilla.apng", "image/webp", "image/avif":
		return true
	}
	return false
}

// clampBatchSize bounds the keyset batch size.
//
// drive_file を `IN ?` でまとめて引くので、1 バッチで最大 batchSize*2 個の
// プレースホルダを作る。PostgreSQL の上限は 65535 なので、operator が大きな
// -batch を渡しても越えないところで頭を打つ。
func clampBatchSize(n int) int {
	if n <= 0 {
		return 1000
	}
	if n > 10000 {
		return 10000
	}
	return n
}
