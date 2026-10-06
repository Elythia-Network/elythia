package repository

import (
	"fmt"

	"gorm.io/gorm"
)

// Drive usage kinds returned by UsageBreakdown. 種類は「その file を誰が指して
// いるか」で決まり、判定は下の優先順で行う (1 つの file が avatar と banner の
// 両方に指されることがあるため、順序が無いと合計が二重計上になる)。
const (
	// DriveUsageKindAvatar is a file some user's `avatarId` points at.
	DriveUsageKindAvatar = "avatar"
	// DriveUsageKindBanner is a file some user's `bannerId` points at.
	DriveUsageKindBanner = "banner"
	// DriveUsageKindEmoji is a file a custom emoji's originalUrl / publicUrl
	// points at.
	DriveUsageKindEmoji = "emoji"
	// DriveUsageKindAttachment is a user-owned file that is none of the above.
	//
	// **「note に添付済み」ではない。** 利用者のドライブに実体があるもの全部で、
	// 一度も添付されていない file も入る。note からの参照で絞るには
	// `note.fileIds` を引く必要があり (実測 1.1-4.2 秒)、しかも drive_file を
	// 指すのは note だけではない (note_draft / gallery_post / chat_message)。
	// note だけで「未参照」を出すと消してよい量を過大に見せるので、ここでは
	// 参照の有無を判定しない (#3053)。
	DriveUsageKindAttachment = "attachment"
	// DriveUsageKindOther is a file with no owner that is none of the above:
	// 取り込みが中断して残った zip や、著者が materialize されていないリモート
	// 添付 (#2717)。
	DriveUsageKindOther = "other"
)

// Drive usage origins returned by UsageBreakdown. `userHost IS NULL` かどうかで
// 決まる (ListForAdmin の origin filter と同じ基準)。
const (
	DriveUsageOriginLocal  = "local"
	DriveUsageOriginRemote = "remote"
)

// driveUsageKinds / driveUsageOrigins fix the order of the breakdown rows so the
// response is deterministic regardless of what the DB returns.
var (
	driveUsageKinds   = []string{DriveUsageKindAttachment, DriveUsageKindAvatar, DriveUsageKindBanner, DriveUsageKindEmoji, DriveUsageKindOther}
	driveUsageOrigins = []string{DriveUsageOriginLocal, DriveUsageOriginRemote}
)

// DriveUsageBucket is one aggregate cell.
type DriveUsageBucket struct {
	// Count is the number of drive_file rows.
	Count int64
	// Size is the sum of the `size` column in bytes. **DB が把握している量**で
	// あって、object storage に実際に置かれている量ではない。
	Size int64
	// LinkCount is how many of Count are `isLink = true`, i.e. rows that hold no
	// bytes of their own. mk-go はリモートメディアをキャッシュしないので
	// (docs/divergence/operations.md 5.5)、リモート側は Count == LinkCount かつ Size == 0 に
	// なる。TS 由来の DB から引き継いだ実体つきリモート行だけがそこから外れる。
	LinkCount int64
}

// DriveUsageKindRow is one (origin, kind) cell of the breakdown.
type DriveUsageKindRow struct {
	Kind   string
	Origin string
	DriveUsageBucket
}

// DriveUsageHostRow is one remote host's usage.
type DriveUsageHostRow struct {
	Host string
	DriveUsageBucket
}

// DriveUsageUserRow is one local user's usage. Username is a display label only and
// is empty when the `user` row cannot be joined (FK があるので通常は起きない。
// driveUsageUserSQL の LEFT JOIN のコメントを参照)。
type DriveUsageUserRow struct {
	UserID   string
	Username string
	DriveUsageBucket
}

// DriveUsageBreakdown is the whole instance-wide drive usage picture.
type DriveUsageBreakdown struct {
	// ByKind always holds len(driveUsageKinds) * len(driveUsageOrigins) rows in a
	// fixed order, zero-filled where the DB returned nothing.
	ByKind []DriveUsageKindRow
	// ByHost / ByUser are the top rows by size, capped by the caller's topN.
	ByHost []DriveUsageHostRow
	ByUser []DriveUsageUserRow
}

// Total sums every ByKind row. ByKind covers every drive_file row exactly once,
// so this is exact rather than an estimate.
func (b *DriveUsageBreakdown) Total() DriveUsageBucket { return b.originTotal("") }

// LocalTotal / RemoteTotal sum the ByKind rows of one origin.
func (b *DriveUsageBreakdown) LocalTotal() DriveUsageBucket {
	return b.originTotal(DriveUsageOriginLocal)
}

func (b *DriveUsageBreakdown) RemoteTotal() DriveUsageBucket {
	return b.originTotal(DriveUsageOriginRemote)
}

// originTotal sums the ByKind rows matching origin ("" sums every row).
func (b *DriveUsageBreakdown) originTotal(origin string) DriveUsageBucket {
	var out DriveUsageBucket
	for _, row := range b.ByKind {
		if origin != "" && row.Origin != origin {
			continue
		}
		out.Count += row.Count
		out.Size += row.Size
		out.LinkCount += row.LinkCount
	}
	return out
}

// driveUsageKindSQL classifies every drive_file row in a single pass.
//
// **リモート添付の参照元は引かない。** drive_file 1 行ごとに note の GIN index を
// 叩く形 (`EXISTS (SELECT 1 FROM note WHERE "fileIds" @> ...)`) は合成データ
// 2,005,400 行で 4.2 秒、note 側から unnest しても 1.1 秒掛かる。3 つの LEFT JOIN は
// どれも小さいテーブル (user / emoji) が相手なので hash join で済み、同じデータで
// 1.6 秒だった (#3053)。
//
// **emoji の URL は空文字を除く。** `emoji.publicUrl` は DEFAULT ” を持つので、
// 除かないと publicUrl 未設定の emoji が「url が空の drive_file」と結合しうる。
// `originalUrl` は NOT NULL で DEFAULT を持たないが、空文字を入れること自体はできる
// ので同じ guard を掛けてある。
//
// **`UNION` (重複排除) でなければならない。** webpublic variant を持たない絵文字は
// `originalUrl == publicUrl` になる (`publicUrl := webpublicUrl ?? url`) ので、
// `UNION ALL` にすると同じ URL が 2 行になり、LEFT JOIN が drive_file の行を複製して
// **件数も使用量も 2 倍に数える**。`av` / `bn` の `DISTINCT` も同じ理由 —
// `user.avatarId` に一意制約は無く、複数人が同じファイルをアイコンにできる。
const driveUsageKindSQL = `
SELECT
	CASE WHEN f."userHost" IS NULL THEN 'local' ELSE 'remote' END AS origin,
	CASE
		WHEN av.id IS NOT NULL THEN 'avatar'
		WHEN bn.id IS NOT NULL THEN 'banner'
		WHEN em.url IS NOT NULL THEN 'emoji'
		WHEN f."userId" IS NOT NULL THEN 'attachment'
		ELSE 'other'
	END AS kind,
	COUNT(*) AS cnt,
	COALESCE(SUM(f.size), 0) AS total_size,
	COUNT(*) FILTER (WHERE f."isLink") AS link_count
FROM "drive_file" f
LEFT JOIN (SELECT DISTINCT "avatarId" AS id FROM "user" WHERE "avatarId" IS NOT NULL) av ON av.id = f.id
LEFT JOIN (SELECT DISTINCT "bannerId" AS id FROM "user" WHERE "bannerId" IS NOT NULL) bn ON bn.id = f.id
LEFT JOIN (
	SELECT "originalUrl" AS url FROM "emoji" WHERE "originalUrl" <> ''
	UNION
	SELECT "publicUrl" FROM "emoji" WHERE "publicUrl" <> ''
) em ON em.url = f.url
GROUP BY 1, 2`

// driveUsageHostSQL ranks remote hosts. 並びは size 降順 → count 降順 → host 昇順。
//
// **size だけで並べない。** mk-go のリモート行は size が全て 0 なので、size だけだと
// 全ホストが同点になり順序が実行ごとに変わる。件数を次に見るのは、その状態でも意味の
// ある順序 (実体は無くても行数を食っているホスト順) を出すため。`host ASC` は
// グループキーなので、最後の tiebreaker として必ず一意に決まる。
const driveUsageHostSQL = `
SELECT f."userHost" AS host,
	COUNT(*) AS cnt,
	COALESCE(SUM(f.size), 0) AS total_size,
	COUNT(*) FILTER (WHERE f."isLink") AS link_count
FROM "drive_file" f
WHERE f."userHost" IS NOT NULL
GROUP BY 1
ORDER BY total_size DESC, cnt DESC, host ASC
LIMIT ?`

// driveUsageUserSQL ranks local users. 集計してから user を join するので、走査するのは
// drive_file 1 本だけで済む。
//
// **`WITH` で始めない。** gorm の dbresolver は raw SQL を TrimSpace して先頭 6 文字が
// `select` のときだけリードレプリカへ回し、それ以外は primary へ倒す
// (`dbresolver` の switchGuess)。CTE で書くと、他の 2 本がレプリカへ行くのにこれだけ
// primary を叩く。`internal/repository` の raw SQL で `WITH` 始まりは他に無い
// (`dbReplications` は既定 false なので、有効にした構成でだけ 1 本が非対称になる)。
//
// **内側の ORDER BY が上位 N を決める。** 外側は選ばれた N 行以下を並べ直すだけなので、
// 内側を落とすと「任意の N 人」を取ったうえで整列だけ正しく見える状態になる。
// 外側の ORDER BY は保険で、**それ単体では観測できない** — 現在のプランナは副問い合わせの
// 順序をそのまま通すため、外側を消しても結果は変わらない (実測)。LEFT JOIN が順序を
// 崩す形へプランが変わったときに効く。
//
// **`LEFT JOIN` も保険。** `drive_file.userId` には `ON DELETE SET NULL` の FK があり、
// 1 つの文の中では利用者の行が必ず存在するので INNER にしても結果は変わらない
// (= これもそれ単体では観測できない)。FK を持たない DB を掴んだときに、使用量の行ごと
// 落とさないための書き方。
const driveUsageUserSQL = `
SELECT agg.user_id, COALESCE(u.username, '') AS username, agg.cnt, agg.total_size, agg.link_count
FROM (
	SELECT "userId" AS user_id,
		COUNT(*) AS cnt,
		COALESCE(SUM(size), 0) AS total_size,
		COUNT(*) FILTER (WHERE "isLink") AS link_count
	FROM "drive_file"
	WHERE "userHost" IS NULL AND "userId" IS NOT NULL
	GROUP BY 1
	ORDER BY total_size DESC, cnt DESC, user_id ASC
	LIMIT ?
) agg
LEFT JOIN "user" u ON u.id = agg.user_id
ORDER BY agg.total_size DESC, agg.cnt DESC, agg.user_id ASC`

// DriveUsageMaxTopN caps the per-host / per-user rankings. 応答の大きさを縛るための
// もので、呼び出し側が大きい値を渡しても黙って切り詰める。
//
// **現在の配線ではここに到達しない** (router が `driveusage.DefaultTopN` = 30 を固定で
// 渡す)。設定から件数を選べるようにしたときに応答が青天井にならないための上限。
const DriveUsageMaxTopN = 100

// DriveUsageRepository aggregates instance-wide drive usage (#3053).
//
// **DriveFileRepository には足さない。** あちらは file 1 件ずつのデータアクセスで、
// 実装を差し替えた test double が 10 ファイル以上に散っている。インスタンス全体の
// 集計は関心が別で、mock しても意味が無い (集計 SQL そのものを確かめたいので、
// 検証は実 PostgreSQL に対して行う) ため、独立した口にしてある。
type DriveUsageRepository interface {
	// Breakdown returns the whole instance's drive usage: 種類 x ローカル/
	// リモートの内訳と、ホスト別 / 利用者別の上位 topN。topN は
	// 1..DriveUsageMaxTopN に丸める。
	//
	// **返すのは DB の `size` 合計**であって object storage に実際に置かれて
	// いる量ではない。削除の失敗や孤児があれば必ずずれる。
	Breakdown(topN int) (*DriveUsageBreakdown, error)
}

type driveUsageRepository struct {
	db *gorm.DB
}

// NewDriveUsageRepository creates a new DriveUsageRepository.
func NewDriveUsageRepository(db *gorm.DB) DriveUsageRepository {
	return &driveUsageRepository{db: db}
}

// clampDriveUsageTopN folds the caller's ranking size into 1..DriveUsageMaxTopN.
//
// **0 に倒さない。** `LIMIT 0` は「ホストが 1 つも無い」と区別が付かない応答に
// なるので、下限は 1 にする。
func clampDriveUsageTopN(topN int) int {
	if topN <= 0 {
		return 1
	}
	if topN > DriveUsageMaxTopN {
		return DriveUsageMaxTopN
	}
	return topN
}

// driveUsageKindScan / driveUsageHostScan / driveUsageUserScan are the raw rows
// of the three aggregation queries.
type driveUsageKindScan struct {
	Origin    string `gorm:"column:origin"`
	Kind      string `gorm:"column:kind"`
	Cnt       int64  `gorm:"column:cnt"`
	TotalSize int64  `gorm:"column:total_size"`
	LinkCount int64  `gorm:"column:link_count"`
}

type driveUsageHostScan struct {
	Host      string `gorm:"column:host"`
	Cnt       int64  `gorm:"column:cnt"`
	TotalSize int64  `gorm:"column:total_size"`
	LinkCount int64  `gorm:"column:link_count"`
}

type driveUsageUserScan struct {
	UserID    string `gorm:"column:user_id"`
	Username  string `gorm:"column:username"`
	Cnt       int64  `gorm:"column:cnt"`
	TotalSize int64  `gorm:"column:total_size"`
	LinkCount int64  `gorm:"column:link_count"`
}

// driveUsageScanner is the seam between the three queries and the assembly.
//
// **エラー経路を確かめるために要る。** 3 本は同じテーブルを読むので、実 DB では
// 「2 本目だけ失敗させる」条件が作れない (1 本目が先に落ちる)。実装を差し替えられる
// 形にしておかないと、`hosts, _ := r.scanHosts(...)` のように**戻り値の err を捨てる
// 変異が緑のまま通る** — 症状は DB 障害が「ホスト 0 件 / 利用者 0 件」の 200 に化ける
// ことで、#2792 が禁じている形そのもの。
type driveUsageScanner interface {
	scanKinds() ([]driveUsageKindScan, error)
	scanHosts(topN int) ([]driveUsageHostScan, error)
	scanUsers(topN int) ([]driveUsageUserScan, error)
}

func (r *driveUsageRepository) Breakdown(topN int) (*DriveUsageBreakdown, error) {
	return driveUsageBreakdownFrom(r, topN)
}

func driveUsageBreakdownFrom(s driveUsageScanner, topN int) (*DriveUsageBreakdown, error) {
	topN = clampDriveUsageTopN(topN)

	kinds, err := s.scanKinds()
	if err != nil {
		return nil, err
	}
	hosts, err := s.scanHosts(topN)
	if err != nil {
		return nil, err
	}
	users, err := s.scanUsers(topN)
	if err != nil {
		return nil, err
	}
	return assembleDriveUsage(kinds, hosts, users)
}

func (r *driveUsageRepository) scanKinds() ([]driveUsageKindScan, error) {
	var rows []driveUsageKindScan
	if err := r.db.Raw(driveUsageKindSQL).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("drive usage by kind: %w", err)
	}
	return rows, nil
}

func (r *driveUsageRepository) scanHosts(topN int) ([]driveUsageHostScan, error) {
	var rows []driveUsageHostScan
	if err := r.db.Raw(driveUsageHostSQL, topN).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("drive usage by host: %w", err)
	}
	return rows, nil
}

func (r *driveUsageRepository) scanUsers(topN int) ([]driveUsageUserScan, error) {
	var rows []driveUsageUserScan
	if err := r.db.Raw(driveUsageUserSQL, topN).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("drive usage by user: %w", err)
	}
	return rows, nil
}

// assembleDriveUsage folds the raw rows into the fixed-order breakdown.
func assembleDriveUsage(kinds []driveUsageKindScan, hosts []driveUsageHostScan, users []driveUsageUserScan) (*DriveUsageBreakdown, error) {
	out := &DriveUsageBreakdown{
		ByKind: newDriveUsageKindRows(),
		ByHost: make([]DriveUsageHostRow, 0, len(hosts)),
		ByUser: make([]DriveUsageUserRow, 0, len(users)),
	}
	for _, k := range kinds {
		idx := driveUsageKindIndex(k.Origin, k.Kind)
		if idx < 0 {
			// 分類の CASE が返しうる値は上の定数で尽きている。それ以外が
			// 返ったら SQL と定数が食い違っているので、黙って捨てると合計だけ
			// 減った表になる。
			return nil, fmt.Errorf("drive usage: unknown origin/kind %q/%q", k.Origin, k.Kind)
		}
		out.ByKind[idx].Count += k.Cnt
		out.ByKind[idx].Size += k.TotalSize
		out.ByKind[idx].LinkCount += k.LinkCount
	}
	for _, h := range hosts {
		out.ByHost = append(out.ByHost, DriveUsageHostRow{
			Host:             h.Host,
			DriveUsageBucket: DriveUsageBucket{Count: h.Cnt, Size: h.TotalSize, LinkCount: h.LinkCount},
		})
	}
	for _, u := range users {
		out.ByUser = append(out.ByUser, DriveUsageUserRow{
			UserID:           u.UserID,
			Username:         u.Username,
			DriveUsageBucket: DriveUsageBucket{Count: u.Cnt, Size: u.TotalSize, LinkCount: u.LinkCount},
		})
	}
	return out, nil
}

// newDriveUsageKindRows builds the zero-filled fixed-order breakdown skeleton.
func newDriveUsageKindRows() []DriveUsageKindRow {
	rows := make([]DriveUsageKindRow, 0, len(driveUsageKinds)*len(driveUsageOrigins))
	for _, origin := range driveUsageOrigins {
		for _, kind := range driveUsageKinds {
			rows = append(rows, DriveUsageKindRow{Kind: kind, Origin: origin})
		}
	}
	return rows
}

// driveUsageKindIndex returns the position of (origin, kind) in the skeleton, or
// -1 when the pair is not one this package knows.
func driveUsageKindIndex(origin, kind string) int {
	for i, org := range driveUsageOrigins {
		if org != origin {
			continue
		}
		for j, k := range driveUsageKinds {
			if k == kind {
				return i*len(driveUsageKinds) + j
			}
		}
	}
	return -1
}
