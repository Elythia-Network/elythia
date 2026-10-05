package repository

import (
	"strings"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// DriveFileRepository provides data access for the `drive_file` table.
type DriveFileRepository interface {
	Create(f *model.DriveFile) error
	FindByID(id string) (*model.DriveFile, error)
	FindByIDs(ids []string) ([]*model.DriveFile, error)
	FindByMD5(userID, md5 string) (*model.DriveFile, error)
	// FindAllByMD5 returns every file of the user with the given md5 hash.
	// upstream drive/files/find-by-hash は findBy({md5, userId}) で一致
	// 全件を返すため、最新 1 件の FindByMD5 (upload dedup 用) と別に持つ
	// (#1564)。
	FindAllByMD5(userID, md5 string) ([]*model.DriveFile, error)
	FindByAccessKey(accessKey string) (*model.DriveFile, error)
	// FindByAnyURL looks up a DriveFile whose url / webpublicUrl /
	// thumbnailUrl matches. upstream drive/files/show の url 指定検索
	// (anyOf param) で使う (#1564)。3 列の index は migration
	// 000059-000061 で追加済み (#1625、BitmapOr で結合される)。
	FindByAnyURL(url string) (*model.DriveFile, error)
	// FindByAnyAccessKey looks up a DriveFile whose primary / thumbnail /
	// webpublic access key matches. Used by `/files/:accessKey` to resolve
	// storedInternal=true rows that the configured storage backend (S3)
	// does not hold (#1414).
	FindByAnyAccessKey(accessKey string) (*model.DriveFile, error)
	// FindByURI looks up a drive_file by its AP `uri` field. Used for
	// deduping remote attachments on inbound Note ingest (#378).
	FindByURI(uri string) (*model.DriveFile, error)
	Update(id string, fields map[string]any) error
	Delete(f *model.DriveFile) error
	// ListByUser returns the user's drive files (#1564 で filter/sort 対応):
	//   - folderID: anyFolder=false のとき nil は root (folderId IS NULL)
	//   - anyFolder: true で folder 条件を付けない (upstream drive/stream は
	//     全 folder 横断で返す)
	//   - fileType: MIME filter。"image/*" 形式は prefix match、それ以外は
	//     完全一致 (upstream drive/files.ts と同 semantics)。空は無条件
	//   - sort: "+createdAt"|"-createdAt"|"+name"|"-name"|"+size"|"-size"|""
	//     (空は sinceID/untilID 由来の id 順。createdAt は id 列で代理)
	ListByUser(userID string, folderID *string, anyFolder bool, fileType, sort, untilID, sinceID string, limit int) ([]*model.DriveFile, error)
	// ListForAdmin returns drive files across all users with optional
	// userID / origin / host / type filters. When userID is non-empty,
	// origin / host are ignored (upstream Misskey の semantics と一致)。
	// origin is "local" / "remote" / "combined" ("combined" is the
	// default). Empty host / type are no-ops.
	ListForAdmin(userID, origin, host, fileType, untilID, sinceID string, limit int) ([]*model.DriveFile, error)
	// ListSystemFiles returns local drive files that are not owned by any user
	// (userId IS NULL AND userHost IS NULL). #670 で導入した emoji copy /
	// import zip の保管先 system file を admin UI から可視化するための一覧
	// API (#686)。
	//
	// **リモートの owner 無し行は含めない (#2753)。** 表示中の note が参照して
	// いる添付が「emoji copy / import zip の保管先」として並ぶため。
	// それらは ListForAdmin に origin=remote を渡せば出る。
	// fileType は MIME prefix match (空なら無制約)、ID 範囲は他の admin
	// listing と同一の semantics。
	ListSystemFiles(fileType, untilID, sinceID string, limit int) ([]*model.DriveFile, error)
	FindByName(userID, name string, folderID *string) ([]*model.DriveFile, error)
	// CountByFolder は drive/folders/show の detail mode pack 用に、
	// 指定 folder 内に直下で属する file 数を返す (#845)。
	CountByFolder(folderID string) (int, error)
	ExistsByMD5(userID, md5 string) (bool, error)
	ListByFileIDs(fileIDs []string) ([]*model.DriveFile, error)
	UsageByUser(userID string) (int64, error)
	// UpdateBulkFolder moves the given files into folderID, scoped to the
	// owning user. The userID predicate prevents a caller from moving drive
	// files they do not own (IDOR): upstream drive/files/move-bulk only ever
	// touches the caller's own rows.
	UpdateBulkFolder(userID string, fileIDs []string, folderID *string) error
	// DeleteOrphans removes rows that are unowned (userId IS NULL), not
	// attributed to a remote host (userHost IS NULL) and not referenced by
	// any custom emoji. Returns affected count. See the orphanWhere const in
	// this file for why unowned remote rows are kept.
	DeleteOrphans() (int64, error)
	// ListOrphans returns up to limit rows DeleteOrphans would delete (userId
	// IS NULL, userHost IS NULL, not referenced by any custom emoji) so the
	// caller can delete each orphan's object-storage bytes before the DB rows
	// (admin/drive/cleanup, #1724). Order is unspecified; mirrors
	// ListRemoteCache's batched shape. See the orphanWhere const in this file
	// for why unowned remote rows are kept.
	ListOrphans(limit int) ([]*model.DriveFile, error)
	// ListOrphanRemoteAttachmentCandidates returns up to limit IDs of link-only
	// remote attachments that no note references and whose ID predates
	// cutoffID, in ascending ID order starting after afterID. The caller must
	// still exclude the ones a live ephemeral note references before deleting
	// (see orphanRemoteAttachmentWhere). afterID は keyset cursor で、空なら
	// 先頭から。
	ListOrphanRemoteAttachmentCandidates(cutoffID, afterID string, limit int) ([]string, error)
	// ExpireRemoteCache turns cached remote files (isLink=false with host set)
	// into link rows instead of deleting them (#3102).
	//
	// **upstream と同じ動作にしてある。** `CleanRemoteFilesProcessorService` は
	// `deleteFileSync(file, true)` を呼び、`deletePostProcess` の `isExpired`
	// 分岐が**行を残して link に倒す**。行ごと消すと `note.fileIds` の指す先が
	// 無くなり、packer が引けない ID を捨てるので**過去の投稿から添付が黙って
	// 消える**。実体 (object storage / ローカル FS) は呼び出し側が先に消すので、
	// 容量はどちらでも空く。
	//
	// `uri` を持たない行だけは倒せないので削除する (upstream の else 枝と同じ)。
	// 戻り値は倒した行と消した行の合計。
	ExpireRemoteCache() (int64, error)
	// ListRemoteCache returns up to limit cached remote files (isLink=false with
	// host set) so the caller can delete their object-storage objects before the
	// DB rows. Order is unspecified.
	ListRemoteCache(limit int) ([]*model.DriveFile, error)
	// ListByUserAll returns up to limit files owned by userID across all folders
	// (admin/delete-all-files-of-a-user storage cleanup).
	ListByUserAll(userID string, limit int) ([]*model.DriveFile, error)
	// ExpireByIDs turns the given rows into link rows (#3102)。ExpireRemoteCache
	// と同じ変換を id 指定で行う (storage を先に消すバッチ経路が使う)。
	ExpireByIDs(ids []string) (int64, error)
	// DeleteByIDs removes the given rows in one statement. Returns affected count.
	DeleteByIDs(ids []string) (int64, error)
	// DeleteByUser removes every drive_file owned by userID. Returns affected
	// count. Used by admin/delete-all-files-of-a-user.
	DeleteByUser(userID string) (int64, error)
	// DeleteByHost removes every drive_file whose userHost matches the given
	// remote instance host. Returns affected count. Used by
	// admin/federation/delete-all-files (#587)。
	DeleteByHost(host string) (int64, error)
	// ListByHost returns every drive_file whose userHost matches the given
	// remote instance host. Used by the drive Service to physically delete each
	// file's storage objects before the bulk row delete (#1772)。
	ListByHost(host string) ([]*model.DriveFile, error)
}

type driveFileRepository struct {
	db *gorm.DB
}

// NewDriveFileRepository creates a new DriveFileRepository.
func NewDriveFileRepository(db *gorm.DB) DriveFileRepository {
	return &driveFileRepository{db: db}
}

func (r *driveFileRepository) Create(f *model.DriveFile) error {
	return r.db.Create(f).Error
}

func (r *driveFileRepository) FindByID(id string) (*model.DriveFile, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var f model.DriveFile
	if err := r.db.First(&f, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *driveFileRepository) FindByIDs(ids []string) ([]*model.DriveFile, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var files []*model.DriveFile
	if err := r.db.Where("id IN ?", ids).Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

// FindByMD5 returns the user's most recent file with the given md5 hash.
func (r *driveFileRepository) FindByMD5(userID, md5 string) (*model.DriveFile, error) {
	if !storable(userID) || !storable(md5) {
		return nil, ErrNotFound
	}
	var f model.DriveFile
	if err := r.db.
		Where("\"userId\" = ? AND md5 = ?", userID, md5).
		Order("id DESC").
		First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

// FindAllByMD5 returns every file of the user with the given md5 hash,
// oldest first (id ASC). upstream find-by-hash の findBy({md5, userId}) は
// order 未指定だが、決定的な応答のため id 昇順に固定する。
func (r *driveFileRepository) FindAllByMD5(userID, md5 string) ([]*model.DriveFile, error) {
	// 列に入らない値はどの行とも一致しえない (#3025)。**引く前に弾く** —
	// 比較の右辺に載せるとクエリごと落ちて 500 になる。
	if !storable(userID) || !storable(md5) {
		return nil, nil
	}
	var files []*model.DriveFile
	if err := r.db.
		Where("\"userId\" = ? AND md5 = ?", userID, md5).
		Order("id ASC").
		Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

// FindByAnyURL resolves a file by matching url, webpublicUrl, or
// thumbnailUrl (upstream drive/files/show の url anyOf 検索、#1564)。
// OR 3 条件は migration 000059-000061 (#1625) の各列 index を BitmapOr で
// 束ねて解決される (seq scan 回避)。
func (r *driveFileRepository) FindByAnyURL(url string) (*model.DriveFile, error) {
	if !storable(url) {
		return nil, ErrNotFound
	}
	if url == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var f model.DriveFile
	if err := r.db.Where(
		`"url" = ? OR "webpublicUrl" = ? OR "thumbnailUrl" = ?`,
		url, url, url,
	).First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *driveFileRepository) FindByURI(uri string) (*model.DriveFile, error) {
	if !storable(uri) {
		return nil, ErrNotFound
	}
	if uri == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var f model.DriveFile
	if err := r.db.Where("uri = ?", uri).First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

// FindByAccessKey looks up a DriveFile whose primary access key matches.
// Returns gorm.ErrRecordNotFound when none match. Used by the media proxy
// to swap a `?preview` request for the cached webpublic / thumbnail variant
// when serving a local file (#637 M1)。
//
// 旧実装は thumbnail/webpublic access key も OR で引いていたが、呼び出し
// 側 (mediaproxy.swapToVariant) は primary key 一致のときしか swap しない
// ため、それらの match は dead clause だった。primary 単独 + unique index
// で planner も最短経路に落とせる (#637 review UR-014)。
func (r *driveFileRepository) FindByAccessKey(accessKey string) (*model.DriveFile, error) {
	if !storable(accessKey) {
		return nil, ErrNotFound
	}
	if accessKey == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var f model.DriveFile
	if err := r.db.Where("\"accessKey\" = ?", accessKey).First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

// FindByAnyAccessKey resolves an access key by matching the primary,
// thumbnail, or webpublic column. `/files/:accessKey` 経由でアクセスされる
// access key は元データ・thumbnail・webpublic のいずれかなので、3 列を OR で
// 引く必要がある (#1414)。3 列とも unique index 付きで planner は bitmap-or
// に落とせる。primary のみで充足する mediaproxy.swapToVariant とは別経路。
func (r *driveFileRepository) FindByAnyAccessKey(accessKey string) (*model.DriveFile, error) {
	if !storable(accessKey) {
		return nil, ErrNotFound
	}
	if accessKey == "" {
		return nil, gorm.ErrRecordNotFound
	}
	var f model.DriveFile
	if err := r.db.Where(
		`"accessKey" = ? OR "thumbnailAccessKey" = ? OR "webpublicAccessKey" = ?`,
		accessKey, accessKey, accessKey,
	).First(&f).Error; err != nil {
		return nil, err
	}
	return &f, nil
}

func (r *driveFileRepository) Update(id string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.DriveFile{}).Where("id = ?", id).Updates(fields).Error
}

func (r *driveFileRepository) Delete(f *model.DriveFile) error {
	return r.db.Delete(f).Error
}

// ListByUser returns the user's drive files filtered/sorted per the
// interface doc (#1564).
// driveTypeFilter applies upstream files.ts の `type` 絞り込みを 1 箇所に集める。
//
// upstream `admin/drive/files.ts` と `drive/files.ts` は `/*` 終端を
// `type.replace('/*','/') + '%'` の prefix LIKE にし、それ以外は完全一致にする。
//
// **LIKE のパターンは escape する (#3037)。** upstream は生のまま載せるので、
// `image/_` のような値が 1 文字 wildcard として働く。MIME の subtype に
// `_` は稀だが使えるので、escape しないと「その型だけ」を指定したつもりの
// 絞り込みが別の型まで拾う。mk-go は #1054 から LIKE を必ず escape する方針で、
// ここだけ通っていなかった (docs/divergence.md)。
//
// ok=false は「一致しえない値」= 呼び出し側は空を返す。列に入らない値
// (NUL / 不正な UTF-8) がそれで、比較の右辺に置くだけで PostgreSQL が
// クエリごと落とす (#3025 と同じ判断)。
func driveTypeFilter(q *gorm.DB, fileType string) (*gorm.DB, bool) {
	if fileType == "" {
		return q, true
	}
	if !storable(fileType) {
		return q, false
	}
	if strings.HasSuffix(fileType, "/*") {
		return q.Where(`"type" LIKE ? ESCAPE '\'`,
			escapeSQLLikePattern(strings.TrimSuffix(fileType, "*"))+"%"), true
	}
	return q.Where(`"type" = ?`, fileType), true
}

func (r *driveFileRepository) ListByUser(userID string, folderID *string, anyFolder bool, fileType, sort, untilID, sinceID string, limit int) ([]*model.DriveFile, error) {
	var rows []*model.DriveFile
	q := r.db.Where("\"userId\" = ?", userID)
	if !anyFolder {
		if folderID == nil {
			q = q.Where("\"folderId\" IS NULL")
		} else {
			q = q.Where("\"folderId\" = ?", *folderID)
		}
	}
	var typeOK bool
	if q, typeOK = driveTypeFilter(q, fileType); !typeOK {
		return nil, nil
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	// upstream files.ts は sort 指定時に makePaginationQuery の orderBy を
	// 上書きする (cursor の WHERE 条件はそのまま残る)。createdAt は id 列で
	// 代理 (= aidx は時系列順)。
	order := ""
	switch sort {
	case "+createdAt":
		order = "id DESC"
	case "-createdAt":
		order = "id ASC"
	case "+name":
		order = "name DESC"
	case "-name":
		order = "name ASC"
	case "+size":
		order = "size DESC"
	case "-size":
		order = "size ASC"
	default:
		order = paginationOrder(sinceID, untilID, "id")
	}
	if err := q.Order(order).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *driveFileRepository) FindByName(userID, name string, folderID *string) ([]*model.DriveFile, error) {
	q := r.db.Where(`"userId" = ? AND "name" = ?`, userID, name)
	if folderID != nil {
		q = q.Where(`"folderId" = ?`, *folderID)
	} else {
		q = q.Where(`"folderId" IS NULL`)
	}
	var files []*model.DriveFile
	if err := q.Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

func (r *driveFileRepository) ExistsByMD5(userID, md5 string) (bool, error) {
	var count int64
	if err := r.db.Model(&model.DriveFile{}).Where(`"userId" = ? AND "md5" = ?`, userID, md5).Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// CountByFolder returns the number of files directly in the given folder.
func (r *driveFileRepository) CountByFolder(folderID string) (int, error) {
	var n int64
	if err := r.db.Model(&model.DriveFile{}).Where(`"folderId" = ?`, folderID).Count(&n).Error; err != nil {
		return 0, err
	}
	return int(n), nil
}

func (r *driveFileRepository) ListByFileIDs(fileIDs []string) ([]*model.DriveFile, error) {
	if len(fileIDs) == 0 {
		return nil, nil
	}
	var files []*model.DriveFile
	if err := r.db.Where("id IN ?", fileIDs).Find(&files).Error; err != nil {
		return nil, err
	}
	return files, nil
}

func (r *driveFileRepository) UsageByUser(userID string) (int64, error) {
	var total int64
	// upstream calcDriveUsageOf は isLink=FALSE の行のみ SUM(size) する
	// (link = リモート/プロキシ file は実体を保持しないため usage に含めない、
	// #1831)。これが無いと link 行を持つ user の usage が本家より大きく出る。
	if err := r.db.Model(&model.DriveFile{}).
		Where(`"userId" = ?`, userID).
		Where(`"isLink" = ?`, false).
		Select("COALESCE(SUM(size), 0)").Scan(&total).Error; err != nil {
		return 0, err
	}
	return total, nil
}

func (r *driveFileRepository) UpdateBulkFolder(userID string, fileIDs []string, folderID *string) error {
	// userId で絞ることで他人の DriveFile を移動できる IDOR を防ぐ (#parity)。
	return r.db.Model(&model.DriveFile{}).
		Where("id IN ?", fileIDs).
		Where(`"userId" = ?`, userID).
		Update("folderId", folderID).Error
}

func (r *driveFileRepository) ListForAdmin(userID, origin, host, fileType, untilID, sinceID string, limit int) ([]*model.DriveFile, error) {
	q := r.db.Model(&model.DriveFile{})
	if userID != "" {
		// upstream は userId 指定時に origin / hostname を読まないので
		// それに合わせる。`/admin/user/<id>` のドライブタブが userId 単独で
		// 引いてくるが、handler 経由で origin="combined" が落ちてくると
		// 全 remote が混ざるバグになるため (#471)。
		q = q.Where(`"userId" = ?`, userID)
	} else {
		switch origin {
		case "local":
			q = q.Where(`"userHost" IS NULL`)
		case "remote":
			q = q.Where(`"userHost" IS NOT NULL`)
		}
		if host != "" {
			q = q.Where(`"userHost" = ?`, host)
		}
	}
	var typeOK bool
	if q, typeOK = driveTypeFilter(q, fileType); !typeOK {
		return nil, nil
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.DriveFile
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *driveFileRepository) ListSystemFiles(fileType, untilID, sinceID string, limit int) ([]*model.DriveFile, error) {
	// `"userHost" IS NULL` が要る (#2753)。`userId IS NULL` の集合は、もはや
	// local な system 資産だけではない — 著者が materialize されていない
	// リモート添付は owner 無しで保存され (#2717)、DeleteOrphanRemoteUsers
	// (#2340) が親 user を消した行も ON DELETE SET NULL で NULL になる。
	//
	// **問題はラベルであって削除経路ではない。** owner 無しの行は誰も個別削除
	// できない (drive_service.go の findOwnedFile が userId nil を無条件で弾き、
	// admin/drive/* に per-file の削除 endpoint が無い)。ただし
	// 「emoji copy / import zip の保管先」という説明のまま**表示中の note が
	// 参照している添付**を並べるので、運用判断の材料として誤っている。
	//
	// 除外してもリモート行が admin から見えなくなるわけではない。
	// ListForAdmin に origin=remote を渡せば userHost で絞られて出る
	// (host が表示され、操作者に文脈がある側)。host 単独では出ない —
	// handler が origin 未指定を local に倒すため (#1545)。
	q := r.db.Model(&model.DriveFile{}).Where(`"userId" IS NULL AND "userHost" IS NULL`)
	var typeOK bool
	if q, typeOK = driveTypeFilter(q, fileType); !typeOK {
		return nil, nil
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	var rows []*model.DriveFile
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// DeleteOrphans deletes the rows selected by orphanWhere. Returns affected
// count.
//
// 対象になる userId NULL の行の例: 取り込み中断で残った中間 zip。
// 一方 system 所有の emoji 画像 file (emoji.originalUrl / publicUrl が
// drive_file.url と結ばれている。mk-go では #670 の emoji copy / import zip が
// 保管先を作る)
// は巻き込まないよう emoji 非参照の guard で除外する (#722)。upstream
// Misskey TS の cleanup は単純に userId NULL を全消しするので、この guard を
// 持たない。
func (r *driveFileRepository) DeleteOrphans() (int64, error) {
	tx := r.db.Where(orphanWhere).Delete(&model.DriveFile{})
	return tx.RowsAffected, tx.Error
}

// orphanWhere は cleanup 対象の行を選ぶ条件。orphan (userId IS NULL) のうち、
// remote host に帰属する行 (userHost 非 NULL) と emoji 参照のある行を除外する。
// DeleteOrphans / ListOrphans で同一 guard を共有する (#1724)。
//
// **`userHost IS NULL` が要る。** リモートの添付は、著者が materialize されて
// いないと owner 無しで作られる (#2717)。それらは**表示中の note が参照している**
// のに、`userId IS NULL` だけだとこの掃除で消える。しかも ephemeral note は DB に
// 行が無いので「note から参照されているか」でも守れない。link-only の行なので
// 実体ストレージは消費せず、残しても DB 行のぶんだけ。
//
// 副作用として、`DeleteOrphanRemoteUsers` (#2340) が親 user を消して
// `ON DELETE SET NULL` になったリモートの行もここでは消えない。**それらを
// 回収するのは `ListOrphanRemoteAttachmentCandidates` (#2722) から始まる掃除**
// で、猶予・「どの note からも参照されていない」こと・「生きている ephemeral
// note の印が無い」ことを条件に消す。こちらの条件を
// リモートへ広げてはいけない (admin から任意のタイミングで走るので、TTL 内の
// ephemeral 添付を巻き込む)。
const orphanWhere = `"userId" IS NULL AND "userHost" IS NULL AND NOT EXISTS (
	SELECT 1 FROM "emoji" e
	WHERE e."originalUrl" = "drive_file"."url"
	   OR e."publicUrl"   = "drive_file"."url"
)`

func (r *driveFileRepository) ListOrphans(limit int) ([]*model.DriveFile, error) {
	if limit <= 0 {
		limit = 100
	}
	var rows []*model.DriveFile
	if err := r.db.Where(orphanWhere).Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// orphanRemoteAttachmentWhere は #2722 の掃除対象を選ぶ条件。
//
// **`userId IS NULL` のリモート行に寿命を与えるのがこの掃除の目的。** 著者が
// materialize されていないリモート添付は owner 無しで保存され (#2717)、
// ephemeral note 自体は Redis の TTL で消えるのに drive_file の行は永久に残る。
// `DeleteOrphanRemoteUsers` (#2340) が親 user を消して ON DELETE SET NULL に
// なった行も同じ形になる。
//
// **`NOT EXISTS (note が参照)` を外さないこと。** ephemeral note は
// `Materializer.EnsureNote` で永続化されうるが、**materialize は
// drive_file.userId を backfill しない**。さらに `upsertAttachments` の dedup
// (`FindByURI`) は既存行を再利用するので、owner 有りの note が owner 無しの行を
// 掴む経路もある。つまり「owner 無し = 参照されていない」は成り立たない。
// この述語だけが表示中の添付を守っている。`note.fileIds` には GIN index
// (`IDX_note_fileIds`、migration 000055。TS 由来の DB では migration 000068 が
// この名前を落として upstream の hash 名が残るが、index 自体はある) があるので
// index が効く。ただし GIN の pending list を flush する前は効かない — 実測で
// 同じクエリが VACUUM 前 4.9 秒 / VACUUM 後 1.5 ミリ秒だった。
//
// **cutoffID は「印を打つまでの窓」を覆うだけで、ephemeral note の寿命を覆う
// ものではない。** 行を作る upsertAttachments と、印を打つ
// `ephemeral.Store.PutNote` は別の処理なので、その隙間に掃除が走ると印の無い
// 行を消してしまう。それ以上の生存判定は呼び出し側 (processor) が Redis の印
// (`LiveFileIDs`) で行う — `Touch` が TTL を打ち直すので、猶予をいくら伸ばして
// も「TTL より長ければ安全」にはならない。
//
// **`isLink = true` に限る。** link-only の行は実体を持たないので DB 行を消す
// だけで完結する。mk-go は remote の実体をキャッシュしない (upsertAttachments は
// 常に `IsLink: true`) ので、実運用でこの条件から漏れるリモート孤児は無い。
// TS 製 DB 由来の `isLink = false` 行は object storage の実体を持つため、
// storage を先に消す `DeleteRemoteCache` / `ListRemoteCache` の経路で扱う。
//
// note 以外から `drive_file` を指すのは `note_draft.fileIds` /
// `gallery_post.fileIds` / `chat_message.fileId` と、`user.avatarId` /
// `user.bannerId` (この 2 本だけが実 FK。ON DELETE SET NULL)。**どれも
// 見ていないが、いずれも呼び出しユーザー所有の file しか受け付けない**ので
// owner 無しのリモート添付が載る経路が無い (drafts / gallery は
// `ownedFileIDs`、chat は所有チェック、avatar / banner は `/api/i/update` の
// 所有者検証を通る。リモートのアバターは `avatarUrl` で持ち drive_file を
// 作らない)。
//
// **TOCTOU の窓は残る。** 候補に挙げてから削除するまでの間に、進行中の
// 取り込みが `FindByURI` で同じ行を掴むことがある。ephemeral 経路なら印が
// 立つので次の実行では守られるが、その 1 回の実行では消えうる (結果は
// note の fileIds に残る dangling ID = 添付 1 件が表示されない)。窓は
// 秒単位で、猶予より古い行に限られる。
const orphanRemoteAttachmentWhere = `"userId" IS NULL
	AND "userHost" IS NOT NULL
	AND "isLink" = true
	AND id < ?
	AND NOT EXISTS (
		SELECT 1 FROM "note" n
		WHERE n."fileIds" @> ARRAY["drive_file".id]::varchar[]
	)`

func (r *driveFileRepository) ListOrphanRemoteAttachmentCandidates(cutoffID, afterID string, limit int) ([]string, error) {
	// **これは防御であって、現状の唯一の歯止めではない。** 空文字を渡しても
	// SQL 側の `id < ''` はどの ID にも一致しないので結果は変わらない (= この
	// 分岐を外しても振る舞いは同じで、テストでも差が出ない)。cutoff の比較を
	// いじったときに「全件対象」へ化けるのを止めるために残す。
	if cutoffID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	// **keyset cursor で進めること。** 消さずに残る行 (参照されているもの、
	// ephemeral が生きているもの) は条件に合致し続けるので、毎回先頭から
	// 引くと同じ行を延々と読み直す。afterID で切ると 1 回の実行あたり実質
	// 1 パスになる。
	//
	// 実測 (PostgreSQL 18 / drive_file 50 万行・うち owner 無し 2 万・参照
	// 済み 1.9 万 / note 40 万件 / VACUUM 済み): cursor 無しの形が 1 バッチ
	// 200 万 buffer なのに対し、深い位置の cursor 付きは 2.6 千 buffer。
	// 所要時間はキャッシュ状態で 20 倍変わる (cold で 17.6 秒、warm +
	// parallel worker 2 で 0.9 秒) ので buffer 数で比べている。
	var ids []string
	err := r.db.Raw(`
		SELECT id FROM "drive_file"
		WHERE `+orphanRemoteAttachmentWhere+`
		  AND id > ?
		ORDER BY id
		LIMIT ?`, cutoffID, afterID, limit).Scan(&ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// remoteCacheWhere は「実体をキャッシュしているリモートファイル」の条件。
// upstream `CleanRemoteFilesProcessorService` の絞りと同じ。
const remoteCacheWhere = `"isLink" = false AND "userHost" IS NOT NULL`

// linkifyUpdates は実体つきの行を link 行へ倒すときの列の値。
//
// **mk-go が普段作る link 行と同じ形にする** (`upsertAttachments` は
// `isLink: true` / `size: 0` / `storedInternal: false` / access key なしで作る)。
// 倒した後の行が mk-go 生まれの行と見分けが付かないので、`admin/drive/usage` の
// 「件数は積み上がるのに使用量は 0」(§5.5) もそのまま成り立つ。
//
// **size を 0 にするのは upstream と違う。** あちらは据え置くが、据え置くと
// 実体が無いのに使用量に乗り続ける (`admin/drive/usage` が嘘をつく)。upstream 自身
// も集計では `isLink = FALSE` で絞るので、link 行の size は読まれない。
//
// **access key は NULL に落とす。** 実体が消えた以上、古い `/files/<key>` の URL を
// 生かしておく意味が無い (upstream は新しい UUID を振り直して同じ効果を得ている)。
// `accessKey` の UNIQUE index は NULL を複数許すので衝突しない。
var linkifyUpdates = map[string]any{
	"isLink":             true,
	"url":                gorm.Expr(`"uri"`),
	"thumbnailUrl":       nil,
	"webpublicUrl":       nil,
	"storedInternal":     false,
	"size":               0,
	"accessKey":          nil,
	"thumbnailAccessKey": nil,
	"webpublicAccessKey": nil,
}

func (r *driveFileRepository) ExpireRemoteCache() (int64, error) {
	return r.expireWhere(remoteCacheWhere)
}

func (r *driveFileRepository) ExpireByIDs(ids []string) (int64, error) {
	// **列に入らない値は引く前に落とす** (#3025)。DeleteByIDs と同じ扱い。
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	return r.expireWhere(`id IN ?`, ids)
}

// expireWhere link-ifies the matching rows, deleting the ones without a uri.
//
// **`uri` が無い行は倒せない** — link 行の `url` は `uri` から作るので、NULL を
// 入れると NOT NULL 制約に当たるか、当たらなくても表示できない行が残る。upstream も
// `file.uri != null` を条件にして、満たさない行は削除する。
func (r *driveFileRepository) expireWhere(where string, args ...any) (int64, error) {
	var total int64
	tx := r.db.Model(&model.DriveFile{}).
		Where(where, args...).Where(`"uri" IS NOT NULL`).Updates(linkifyUpdates)
	if tx.Error != nil {
		return 0, tx.Error
	}
	total += tx.RowsAffected
	del := r.db.Where(where, args...).Where(`"uri" IS NULL`).Delete(&model.DriveFile{})
	if del.Error != nil {
		return total, del.Error
	}
	return total + del.RowsAffected, nil
}

func (r *driveFileRepository) ListRemoteCache(limit int) ([]*model.DriveFile, error) {
	if limit <= 0 {
		limit = 100
	}
	var files []*model.DriveFile
	err := r.db.Where(`"isLink" = false AND "userHost" IS NOT NULL`).
		Order("id DESC").Limit(limit).Find(&files).Error
	return files, err
}

func (r *driveFileRepository) ListByUserAll(userID string, limit int) ([]*model.DriveFile, error) {
	if userID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 100
	}
	var files []*model.DriveFile
	err := r.db.Where(`"userId" = ?`, userID).
		Order("id DESC").Limit(limit).Find(&files).Error
	return files, err
}

func (r *driveFileRepository) DeleteByIDs(ids []string) (int64, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return 0, nil
	}
	tx := r.db.Where("id IN ?", ids).Delete(&model.DriveFile{})
	return tx.RowsAffected, tx.Error
}

func (r *driveFileRepository) DeleteByUser(userID string) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	tx := r.db.Where(`"userId" = ?`, userID).Delete(&model.DriveFile{})
	return tx.RowsAffected, tx.Error
}

func (r *driveFileRepository) DeleteByHost(host string) (int64, error) {
	if host == "" {
		return 0, nil
	}
	// userHost が remote のリモートユーザーアップロード分のみ削除。
	// ローカル user (userHost IS NULL) は誤って巻き込まないよう明示一致のみ。
	tx := r.db.Where(`"userHost" = ?`, host).Delete(&model.DriveFile{})
	return tx.RowsAffected, tx.Error
}

func (r *driveFileRepository) ListByHost(host string) ([]*model.DriveFile, error) {
	if host == "" {
		return nil, nil
	}
	var rows []*model.DriveFile
	if err := r.db.Where(`"userHost" = ?`, host).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
