package hashtags

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/api/apierr"
	"github.com/elythia-network/elythia/internal/api/jsnum"
	"github.com/elythia-network/elythia/internal/api/meself"
	"github.com/elythia-network/elythia/internal/api/pagination"
	"github.com/elythia-network/elythia/internal/api/userrelation"
	corehashtag "github.com/elythia-network/elythia/internal/core/hashtag"
	"github.com/elythia-network/elythia/internal/core/userpack"
	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/misc/colfit"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/misc/searchnorm"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/server/middleware"
	"github.com/labstack/echo/v4"
	"golang.org/x/sync/singleflight"
	"gorm.io/gorm"
)

// Handler handles hashtag-related API endpoints.
type Handler struct {
	db *gorm.DB
	// relation は hashtags/users の embed user に viewer 視点の relation block を
	// 付与する (upstream packMany(users, me))。未配線 / 匿名では no-op (#1957-a)。
	relation userrelation.Repos
	// idGen は note ID parse / createdAt 抽出に使う (#2106 L25)。未配線時は aidx に fallback。
	idGen id.Generator
	// extras は hashtags/users の利用者のピン留め・移行先をまとめて埋める (#3330)。
	extras userpack.DetailExtrasMany
	// moderators は閲覧者がモデレーターかを判定する。本家 packMany はモデレーターに
	// moderationNote と 2FA の項目を足し、カウントのゲートを外す (#3330)。
	moderators userpack.ModeratorChecker
	// trendFlight は hashtags/trend の同時の集計を 1 回にまとめる (#3579)。
	// プロセスごとに持つので、複数プロセスで動かすとプロセスの数だけ集計する。
	trendFlight singleflight.Group
	// meta は hashtags/trend から meta.hiddenTags / meta.sensitiveWords に当たる
	// タグを外すのに使う (#3628)。キャッシュ付きの meta (metaUpdated で無効化される)
	// を渡す。未配線なら外さない (起動時検査が production の配線漏れを拾う)。
	meta repository.MetaRepository
	// selfPacker は閲覧者自身を MeDetailed へ昇格させ、/api/i の handler で未読・
	// role・ピン留めを埋める (#3784)。未配線なら補完せずに昇格だけする。
	selfPacker *meself.Packer
	// entityPacker は利用者や投稿を包むときの依存 (role 由来の値、メディアのプロキシ等) を
	// 持つ (#3792)。組み立てで渡す。
	entityPacker *entity.Packer
}

// SetEntityPacker wires the Packer this handler packs entities with (#3792).
func (h *Handler) SetEntityPacker(p *entity.Packer) { h.entityPacker = p }

// HasEntityPacker reports whether SetEntityPacker received a Packer that
// rewrites media URLs. 無いと avatarUrl などを生のまま返し、閲覧者の IP が
// リモートへ漏れる。
func (h *Handler) HasEntityPacker() bool { return h.entityPacker.HasMediaURL() }

// SetSelfPacker wires the Packer that promotes the viewer's own entry into
// MeDetailed. Unset promotes without the handler-only fields.
func (h *Handler) SetSelfPacker(p *meself.Packer) { h.selfPacker = p }

// SetMetaRepo wires the meta repository whose hiddenTags / sensitiveWords are
// kept out of hashtags/trend (#3628).
func (h *Handler) SetMetaRepo(m repository.MetaRepository) {
	h.meta = m
}

// HasMetaRepo reports whether the meta repository was wired.
//
// 未配線だと、管理画面で隠したタグとセンシティブな語のタグがトレンドに出る。
// 起動時検査に使う。
func (h *Handler) HasMetaRepo() bool { return h.meta != nil }

// SetModeratorChecker wires the moderator check of hashtags/users (#3330).
func (h *Handler) SetModeratorChecker(m userpack.ModeratorChecker) {
	h.moderators = m
}

// HasModeratorChecker reports whether the moderator check was wired.
//
// 未配線だとモデレーターにも moderationNote などが出ず、非公開のカウントも
// 0 のまま返る。起動時検査に使う。
func (h *Handler) HasModeratorChecker() bool { return h.moderators != nil }

// SetDetailExtras wires the batch filler of pinnedNotes / pinnedPage / movedTo /
// alsoKnownAs for hashtags/users (#3330).
func (h *Handler) SetDetailExtras(x userpack.DetailExtrasMany) {
	h.extras = x
}

// HasDetailExtras reports whether the detail extras filler was wired.
//
// 未配線だと hashtags/users の pinnedNotes などが空、movedTo / alsoKnownAs が
// null のまま返る。起動時検査に使う。
func (h *Handler) HasDetailExtras() bool { return h.extras != nil }

// NewHandler creates a new hashtags Handler.
func NewHandler(db *gorm.DB) *Handler {
	return &Handler{db: db}
}

// SetIDGen wires the configured ID generator so Trend / Users use the operator's
// configured generator instead of constructing an aidx generator per request (#2106 L25).
func (h *Handler) SetIDGen(g id.Generator) {
	h.idGen = g
}

// generator returns the wired ID generator, falling back to aidx when unset
// (preserves test compat for handlers constructed without SetIDGen)。
func (h *Handler) generator() (id.Generator, error) {
	if h.idGen != nil {
		return h.idGen, nil
	}
	return id.NewGenerator("aidx")
}

// SetRelationRepos wires the repositories used to populate viewer-relative
// relation fields on hashtags/users (#1957-a). Unset = relations omitted.
func (h *Handler) SetRelationRepos(r userrelation.Repos) {
	h.relation = r
}

// listSortOrders は upstream Misskey TS hashtags/list paramDef enum を、
// 対応する order by 句に対応付ける (#925, #1544)。+ が DESC、- が ASC
// (upstream の符号付けと同じ)。キー集合が paramDef enum の妥当値も兼ねる。
var listSortOrders = map[string]string{
	"+mentionedUsers": `"mentionedUsersCount" DESC`, "-mentionedUsers": `"mentionedUsersCount" ASC`,
	"+mentionedLocalUsers": `"mentionedLocalUsersCount" DESC`, "-mentionedLocalUsers": `"mentionedLocalUsersCount" ASC`,
	"+mentionedRemoteUsers": `"mentionedRemoteUsersCount" DESC`, "-mentionedRemoteUsers": `"mentionedRemoteUsersCount" ASC`,
	"+attachedUsers": `"attachedUsersCount" DESC`, "-attachedUsers": `"attachedUsersCount" ASC`,
	"+attachedLocalUsers": `"attachedLocalUsersCount" DESC`, "-attachedLocalUsers": `"attachedLocalUsersCount" ASC`,
	"+attachedRemoteUsers": `"attachedRemoteUsersCount" DESC`, "-attachedRemoteUsers": `"attachedRemoteUsersCount" ASC`,
}

// List handles POST /api/hashtags/list.
func (h *Handler) List(c echo.Context) error {
	// offset は本家の paramDef に無いので、本家と同じく読まない (#3746)。
	var req struct {
		Limit                    *jsnum.Int `json:"limit"`
		Sort                     string     `json:"sort"`
		AttachedToUserOnly       bool       `json:"attachedToUserOnly"`
		AttachedToLocalUserOnly  bool       `json:"attachedToLocalUserOnly"`
		AttachedToRemoteUserOnly bool       `json:"attachedToRemoteUserOnly"`
	}
	if err := c.Bind(&req); err != nil || req.Sort == "" {
		// upstream Misskey TS は paramDef で sort を required にしている (#925)。
		// mk-go も同 shape に揃え、permissive な挙動を弾く。
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "Invalid parameters.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	order, ok := listSortOrders[req.Sort]
	if !ok {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "sort must be one of upstream-defined enum.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// upstream list.ts: attachedTo* で attachedUsersCount 系が != 0 の tag のみに絞る。
	q := h.db.Model(&model.Hashtag{})
	if req.AttachedToUserOnly {
		q = q.Where(`"attachedUsersCount" != 0`)
	}
	if req.AttachedToLocalUserOnly {
		q = q.Where(`"attachedLocalUsersCount" != 0`)
	}
	if req.AttachedToRemoteUserOnly {
		q = q.Where(`"attachedRemoteUsersCount" != 0`)
	}
	q = q.Order(order).Limit(limit)
	var tags []*model.Hashtag
	if err := q.Find(&tags).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	result := make([]map[string]any, 0, len(tags))
	for _, t := range tags {
		result = append(result, packTag(t))
	}
	return c.JSON(http.StatusOK, result)
}

// Search handles POST /api/hashtags/search.
func (h *Handler) Search(c echo.Context) error {
	// query は欠落(nil)と空文字列を区別する。本家は空文字列を通し、前方一致の
	// '%' で全タグから返す (#3746)。
	var req struct {
		Query  *string    `json:"query"`
		Limit  *jsnum.Int `json:"limit"`
		Offset jsnum.Int  `json:"offset"`
	}
	if err := c.Bind(&req); err != nil || req.Query == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "query is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	query := *req.Query
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	// repository 層の検索と同じく、空の結果で答える (エラーではない)。
	if !colfit.Storable(query) {
		return c.JSON(http.StatusOK, []string{})
	}
	// upstream search.ts: name LIKE sqlLikeEscape(query.toLowerCase())+'%' (前方一致)
	// を mentionedLocalUsersCount DESC で並べ offset を適用する。hashtag.name は
	// normalizeForSearch 済 (lowercase) で格納されるため小文字化で一致する。
	q := h.db.Model(&model.Hashtag{}).
		Where(`"name" LIKE ? ESCAPE '\'`, escapeLike(strings.ToLower(query))+"%").
		Order(`"mentionedLocalUsersCount" DESC`).
		Limit(limit)
	if req.Offset > 0 {
		q = q.Offset(int(req.Offset))
	}
	var tags []*model.Hashtag
	if err := q.Find(&tags).Error; err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		names = append(names, t.Name)
	}
	return c.JSON(http.StatusOK, names)
}

// Show handles POST /api/hashtags/show.
func (h *Handler) Show(c echo.Context) error {
	// tag は欠落(nil)と空文字列を区別する。本家は空文字列を通し、一致する
	// タグが無いので NO_SUCH_HASHTAG になる (#3746)。
	var req struct {
		Tag *string `json:"tag"`
	}
	if err := c.Bind(&req); err != nil || req.Tag == nil {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "tag is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	tagName := *req.Tag
	// upstream show.ts: hashtags.findOneBy({name: normalizeForSearch(tag)})。
	// hashtag.name は normalizeForSearch (NFKC+lowercase) 済で格納されるため、
	// 入力も同じく正規化して一致させる ('Misskey' → 'misskey')。
	// 列に入らない文字を含む tag はどの行とも一致しえない (#3025)。**引く前に
	// 弾く** — 比較の右辺に載せるとクエリごと落ち、下の `err != nil` が
	// **DB 障害まで NO_SUCH_HASHTAG に潰す**経路に流れ込む。
	if !colfit.Storable(tagName) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_HASHTAG", "No such hashtag.", "110ee688-193e-4a3a-9ecf-c167b2e6981e"))
	}
	var tag model.Hashtag
	if err := h.db.Where("name = ?", searchnorm.Normalize(tagName)).First(&tag).Error; err != nil {
		// HTTP semantics 的には not-found = 404 が望ましいが、upstream
		// Misskey TS は ApiError の既定動作で 400 を返している (#925)。
		// drop-in 互換を優先して 400 + NO_SUCH_HASHTAG body に揃える。
		// error id も upstream と同じ ...c167b2e6981e に統一。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_HASHTAG", "No such hashtag.", "110ee688-193e-4a3a-9ecf-c167b2e6981e"))
	}
	return c.JSON(http.StatusOK, packTag(&tag))
}

// trendBucketSeconds / trendBucketCount / trendTopN define the hashtags/trend
// window: 20 buckets of 10 minutes (200 minutes), top 10 tags.
const (
	trendBucketSeconds = 600 // 10 分
	trendBucketCount   = 20  // 過去 200 分
	trendTopN          = 10
)

// aggregateTrend は bucket を uint32 の bit で持つので、32 個を超えるとコンパイルを止める。
var _ [32 - trendBucketCount]struct{}

// trendRow is one public / home note that carries tags, as read by Trend.
type trendRow struct {
	ID     string
	UserID string
	Tags   []string
}

// Trend handles POST /api/hashtags/trend.
//
// 応答の形 (tag / chart / usersCount) は本家と同じだが、数え方は Elythia
// 独自。直近 200 分 (10 分 × 20 bucket) の note から、hashtag ごとに投稿した
// user の数 (重複を除く) を数え、frontend の MkMiniChart が描画できる時系列を
// 返す。本家は Redis の 1 時間の窓のランキングと HyperLogLog から組み立て、
// 窓の取り方も件数も違う (docs/divergence/api.md「hashtags/trend のランキングの
// 数え方」)。mk-go では HLL を維持せず note テーブルから動的に集計する。
//
// mk-go の note テーブルは createdAt 列を持たず、aidx ID 先頭 8 文字に
// ms timestamp が埋め込まれている (`internal/misc/id`)。200 分前の
// cutoff prefix を生成して `id > cutoffID` で範囲フィルタをかける。
//
// visibility フィルタ: TS の HashtagService.updateHashtag は public /
// home の note のみ集計対象にする。followers / specified を含めると
// 公開トレンドに非公開タグが現れてプライバシー上の問題になるため、
// SQL の WHERE で同じ条件を強制する。
//
// chart の bucket は新しいもの順 (index 0 = 直近 10 分、index 19 = 200 分前)。
// usersCount は本家と同じく `max(chart)` で算出する。
//
// 同時に来た呼び出しは 1 回の集計にまとめる (#3579)。結果は閲覧者・認証・
// リクエストの中身に依らないので、誰に返しても同じ値になる。期限付きの
// キャッシュは持たない — 本家はサーバー側で毎回 Redis から引き直すので、
// 集計が終わった後に来た呼び出しは必ず集計し直す (鮮度を本家と揃える)。
// 本家の cacheSec (60 秒) は匿名 GET の Cache-Control だけで、middleware の
// APICacheControl が同じ値を付けている。
//
// 本家の updateHashtagsRanking と同じく、meta.hiddenTags と meta.sensitiveWords
// に当たるタグはランキングに入れない (#3628)。
func (h *Handler) Trend(c echo.Context) error {
	hidden, sensitive, err := h.trendExclusions()
	if err != nil {
		// 隠すための設定が読めないまま集計すると、隠したタグを公開のトレンドに
		// 出してしまう。meta はキャッシュから読むので、ここで落ちるのは DB に
		// 届かないときで、そのときは集計の SELECT も通らない。
		slog.Error("hashtags/trend: fetch meta", "error", err)
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// まとめる単位を、除外の設定ごとに分ける。設定を変えた後に来た呼び出しが、
	// 変える前の設定で始まっていた集計に合流して、外したはずのタグ (または
	// 戻したはずのタグ) の結果を受け取らないようにする。変える前から待っていた
	// 呼び出しには、変える前の設定の結果が返る (本家でも、変える前に積んだ
	// ランキングはそのまま読まれる)。
	key := trendFlightKey(hidden, sensitive)
	// computeTrend はリクエストの context を使わない。使うと、先頭の呼び出しが
	// 切断されたときに、待っていた呼び出しまで同じ誤りで 500 になる。
	v, err, _ := h.trendFlight.Do(key, func() (any, error) {
		v, err := h.computeTrend(corehashtag.NewRankingFilter(hidden, sensitive))
		if err != nil {
			// 待っていた呼び出しにも同じエラーが返るので、記録は集計した側で 1 回だけ。
			slog.Error("hashtags/trend: aggregate", "error", err)
		}
		return v, err
	})
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	// 同じ値を複数の呼び出しへ返すので、ここから先で書き換えない (JSON に
	// するだけ)。
	return c.JSON(http.StatusOK, v)
}

// trendExclusions returns meta.hiddenTags and meta.sensitiveWords, or nothing
// when the meta repository is not wired.
func (h *Handler) trendExclusions() (hidden, sensitive []string, err error) {
	if h.meta == nil {
		return nil, nil, nil
	}
	m, err := h.meta.Fetch()
	if err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, nil, nil
	}
	return m.HiddenTags, m.SensitiveWords, nil
}

// trendFlightKey returns the singleflight key of a trend aggregation with the
// given exclusions. Different settings never share a key.
//
// 設定は長くなりうるので、そのまま連ねずに hash にする。区切りの曖昧さで別の
// 設定が同じ鍵にならないよう、要素ごとに長さを前置する。
func trendFlightKey(hidden, sensitive []string) string {
	sum := sha256.New()
	var n [8]byte
	for _, list := range [][]string{hidden, sensitive} {
		binary.BigEndian.PutUint64(n[:], uint64(len(list)))
		sum.Write(n[:])
		for _, v := range list {
			binary.BigEndian.PutUint64(n[:], uint64(len(v)))
			sum.Write(n[:])
			sum.Write([]byte(v))
		}
	}
	return "trend:" + hex.EncodeToString(sum.Sum(nil))
}

// computeTrend reads the tagged public / home notes of the last 200 minutes
// and aggregates them into the hashtags/trend response, leaving out the tags
// that filter excludes.
func (h *Handler) computeTrend(filter *corehashtag.RankingFilter) ([]map[string]any, error) {
	gen, err := h.generator()
	if err != nil {
		return nil, err
	}

	cutoff := time.Now().Add(-time.Duration(trendBucketSeconds*trendBucketCount) * time.Second)
	cutoffID := id.AidxCutoffPrefix(cutoff)

	// model.StringArray を含む struct を Scan(&rows) に渡すと GORM の
	// schema reflection が `unsupported data type: &[]` を warn-log する
	// (model.StringArray の Scanner で runtime には正しく動くが、初期 type
	// infer で躓く)。Rows().Scan() で個別に scan するとこの noise を回避
	// できるので、本 handler では明示的なループ scan を採用する。
	var rows []trendRow
	sqlRows, err := h.db.Raw(`
		SELECT "id", "userId", "tags"
		FROM "note"
		WHERE "id" > ?
		  AND "tags" <> ARRAY[]::varchar[]
		  AND "visibility" IN ('public', 'home')
	`, cutoffID).Rows()
	if err != nil {
		return nil, err
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
		var r trendRow
		var tags model.StringArray
		if err := sqlRows.Scan(&r.ID, &r.UserID, &tags); err != nil {
			return nil, err
		}
		r.Tags = []string(tags)
		rows = append(rows, r)
	}
	if err := sqlRows.Err(); err != nil {
		return nil, err
	}
	return aggregateTrend(rows, gen, time.Now(), filter.Excludes), nil
}

// aggregateTrend ranks the tags of rows by unique users and builds the
// per-bucket unique-user chart of the top tags.
//
// tag ごとに「user → その user が投稿した bucket の bit 集合」を持つ。
// 以前は tag ごとに bucket 20 個分の set を作っていたので、tag の種類が
// 多いと集計 1 回で数十万の allocation になっていた (#3579)。
// 数えるものは同じ: total は tag の unique user 数、chart[i] は bucket i の
// unique user 数。
//
// excluded が true を返すタグはランキングに入れない (#3628)。数えた後、並べる
// 前に外すので、判定はタグの種類ごとに 1 回で済み、残るタグの値は変わらない。
func aggregateTrend(rows []trendRow, gen id.Generator, now time.Time, excluded func(tag string) bool) []map[string]any {
	perTag := make(map[string]map[string]uint32)
	for _, r := range rows {
		ts, perr := gen.ParseTime(r.ID)
		if perr != nil {
			continue
		}
		secsAgo := now.Sub(ts).Seconds()
		if secsAgo < 0 || secsAgo >= float64(trendBucketSeconds*trendBucketCount) {
			continue
		}
		bit := uint32(1) << (int(secsAgo) / trendBucketSeconds)
		for _, tag := range r.Tags {
			if tag == "" {
				continue
			}
			users := perTag[tag]
			if users == nil {
				users = make(map[string]uint32)
				perTag[tag] = users
			}
			users[r.UserID] |= bit
		}
	}

	// ranking: total users 降順、同点は tag name 昇順 (deterministic)
	type ranked struct {
		tag   string
		total int
	}
	rankings := make([]ranked, 0, len(perTag))
	for tag, users := range perTag {
		if excluded(tag) {
			continue
		}
		rankings = append(rankings, ranked{tag: tag, total: len(users)})
	}
	sort.Slice(rankings, func(i, j int) bool {
		if rankings[i].total != rankings[j].total {
			return rankings[i].total > rankings[j].total
		}
		return rankings[i].tag < rankings[j].tag
	})
	if len(rankings) > trendTopN {
		rankings = rankings[:trendTopN]
	}

	result := make([]map[string]any, 0, len(rankings))
	for _, rk := range rankings {
		chart := make([]int, trendBucketCount)
		for _, bits := range perTag[rk.tag] {
			for i := 0; i < trendBucketCount; i++ {
				if bits&(1<<i) != 0 {
					chart[i]++
				}
			}
		}
		maxUsers := 0
		for _, n := range chart {
			if n > maxUsers {
				maxUsers = n
			}
		}
		result = append(result, map[string]any{
			"tag":        rk.tag,
			"chart":      chart,
			"usersCount": maxUsers,
		})
	}
	return result
}

// validUsersSorts は upstream Misskey TS hashtags/users paramDef enum と一致 (#925)。
var validUsersSorts = map[string]struct{}{
	"+follower": {}, "-follower": {},
	"+createdAt": {}, "-createdAt": {},
	"+updatedAt": {}, "-updatedAt": {},
}

// validUsersStates / validUsersOrigins は upstream hashtags/users paramDef の
// state / origin enum と一致。default は upstream に合わせ state=all / origin=local。
var validUsersStates = map[string]struct{}{"all": {}, "alive": {}}
var validUsersOrigins = map[string]struct{}{"combined": {}, "local": {}, "remote": {}}

// usersAliveWindow は state=alive の "生存" 判定窓 (upstream の now-5日)。
const usersAliveWindow = 5 * 24 * time.Hour

// Users handles POST /api/hashtags/users.
//
// 指定 hashtag を user.tags に持つユーザーを UserDetailed[] で返す
// (upstream hashtags/users)。tag は searchnorm.Normalize (NFKC + lowercase) で
// 正規化して array containment (`tags @> {normTag}`) する。user.tags 側も同じ
// 正規化で populate される前提 (Part 2)。
func (h *Handler) Users(c echo.Context) error {
	// tag は欠落(nil)と空文字列を区別する。本家は空文字列を通し、空の結果を
	// 返す (#3746)。
	var req struct {
		Tag    *string    `json:"tag"`
		Sort   string     `json:"sort"`
		State  string     `json:"state"`
		Origin string     `json:"origin"`
		Limit  *jsnum.Int `json:"limit"`
		Offset jsnum.Int  `json:"offset"`
	}
	if err := c.Bind(&req); err != nil || req.Tag == nil || req.Sort == "" {
		// upstream Misskey TS は paramDef で tag + sort を required にしている (#925)。
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "tag and sort are required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if _, ok := validUsersSorts[req.Sort]; !ok {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "sort must be one of upstream-defined enum.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if req.State == "" {
		req.State = "all"
	}
	if _, ok := validUsersStates[req.State]; !ok {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "state must be one of upstream-defined enum.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	if req.Origin == "" {
		req.Origin = "local"
	}
	if _, ok := validUsersOrigins[req.Origin]; !ok {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "origin must be one of upstream-defined enum.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit.IntPtr(), 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if req.Offset < 0 {
		req.Offset = 0
	}
	tagValue := *req.Tag

	// 列に入らない文字を含む tag はどの行とも一致しえない (#3025)。配列の
	// containment も bind parameter なので、載せるとクエリごと落ちる。
	if !colfit.Storable(tagValue) {
		return c.JSON(http.StatusOK, []any{})
	}
	// containment query。upstream は `:tag <@ user.tags` (= tag 配列が user.tags の
	// 部分集合)。単一要素なので `tags @> {normTag}` と等価。
	normTag := searchnorm.Normalize(tagValue)
	q := h.db.Model(&model.User{}).
		Where("tags @> ?", model.StringArray{normTag}).
		Where("\"isSuspended\" = FALSE")
	if req.State == "alive" {
		q = q.Where("\"updatedAt\" > ?", time.Now().Add(-usersAliveWindow))
	}
	switch req.Origin {
	case "local":
		q = q.Where("host IS NULL")
	case "remote":
		q = q.Where("host IS NOT NULL")
	}
	switch req.Sort {
	case "+follower":
		q = q.Order("\"followersCount\" DESC")
	case "-follower":
		q = q.Order("\"followersCount\" ASC")
	case "+createdAt":
		q = q.Order("id DESC")
	case "-createdAt":
		q = q.Order("id ASC")
	case "+updatedAt":
		q = q.Order("\"updatedAt\" DESC")
	case "-updatedAt":
		q = q.Order("\"updatedAt\" ASC")
	}

	var users []*model.User
	if err := q.Limit(limit).Offset(int(req.Offset)).Find(&users).Error; err != nil {
		return apierr.JSONInternalError(c)
	}

	// UserDetailed pack 用に profile を batch fetch (per-user N+1 回避)。
	profByID := make(map[string]*model.UserProfile, len(users))
	if len(users) > 0 {
		ids := make([]string, len(users))
		for i, u := range users {
			ids[i] = u.ID
		}
		var profiles []*model.UserProfile
		if err := h.db.Where("\"userId\" IN ?", ids).Find(&profiles).Error; err != nil {
			return apierr.JSONInternalError(c)
		}
		for _, p := range profiles {
			profByID[p.UserID] = p
		}
	}

	// createdAt 抽出用の idGen は Trend と同じく aidx を使う。
	gen, err := h.generator()
	if err != nil {
		return apierr.JSONInternalError(c)
	}
	viewer := middleware.GetUser(c)
	ctx := c.Request().Context()
	// 本家は packMany(users, me, {schema: 'UserDetailed'})。モデレーター向けの
	// 項目・関係・カウントのゲート (`sort:"+follower"` で非公開のカウントを並べ
	// 替えて読めないよう、#1558)・ピン留め・移行先を DetailedMany でまとめて組み、
	// 関係は getRelations と同じく関係ごとに 1 回の IN で引く (#3330)。匿名の
	// 閲覧者にはピン留めを出さない (本家 packMany と同じ)。
	packer := userpack.New(h.entityPacker, userpack.Lookups{
		Relations:  h.relation,
		Moderators: h.moderators,
		ExtrasMany: h.extras,
	}, gen)
	packed := packer.DetailedMany(ctx, viewer, users, profByID)
	out := make([]any, 0, len(users))
	for i, u := range users {
		// upstream の pack は isDetailed && isMe で MeDetailed を返すので、
		// 結果に自分が混ざるときは自分だけ MeDetailed になる。
		out = append(out, h.selfPacker.Pack(ctx, packed[i], u, profByID[u.ID], viewer))
	}
	return c.JSON(http.StatusOK, out)
}

// escapeLike escapes LIKE metacharacters so user input matches literally
// (upstream sqlLikeEscape 相当)。PostgreSQL の既定 escape 文字は backslash
// なので \ 自身もエスケープする。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func packTag(t *model.Hashtag) map[string]any {
	return map[string]any{
		"tag":                       t.Name,
		"mentionedUsersCount":       t.MentionedUsersCount,
		"mentionedLocalUsersCount":  t.MentionedLocalUsersCount,
		"mentionedRemoteUsersCount": t.MentionedRemoteUsersCount,
		"attachedUsersCount":        t.AttachedUsersCount,
		"attachedLocalUsersCount":   t.AttachedLocalUsersCount,
		"attachedRemoteUsersCount":  t.AttachedRemoteUsersCount,
	}
}
