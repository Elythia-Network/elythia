package hashtags

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/shiroha-a/mk/internal/api/apierr"
	"github.com/shiroha-a/mk/internal/api/meself"
	"github.com/shiroha-a/mk/internal/api/pagination"
	"github.com/shiroha-a/mk/internal/api/userrelation"
	"github.com/shiroha-a/mk/internal/core/userpack"
	"github.com/shiroha-a/mk/internal/entity"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/misc/searchnorm"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/server/middleware"
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
}

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
	var req struct {
		Limit                    *int   `json:"limit"`
		Sort                     string `json:"sort"`
		Offset                   int    `json:"offset"`
		AttachedToUserOnly       bool   `json:"attachedToUserOnly"`
		AttachedToLocalUserOnly  bool   `json:"attachedToLocalUserOnly"`
		AttachedToRemoteUserOnly bool   `json:"attachedToRemoteUserOnly"`
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
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
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
	if req.Offset > 0 {
		q = q.Offset(req.Offset)
	}
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
	var req struct {
		Query  string `json:"query"`
		Limit  *int   `json:"limit"`
		Offset int    `json:"offset"`
	}
	if err := c.Bind(&req); err != nil || req.Query == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "query is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	// repository 層の検索と同じく、空の結果で答える (エラーではない)。
	if !colfit.Storable(req.Query) {
		return c.JSON(http.StatusOK, []string{})
	}
	// upstream search.ts: name LIKE sqlLikeEscape(query.toLowerCase())+'%' (前方一致)
	// を mentionedLocalUsersCount DESC で並べ offset を適用する。hashtag.name は
	// normalizeForSearch 済 (lowercase) で格納されるため小文字化で一致する。
	q := h.db.Model(&model.Hashtag{}).
		Where(`"name" LIKE ? ESCAPE '\'`, escapeLike(strings.ToLower(req.Query))+"%").
		Order(`"mentionedLocalUsersCount" DESC`).
		Limit(limit)
	if req.Offset > 0 {
		q = q.Offset(req.Offset)
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
	var req struct {
		Tag string `json:"tag"`
	}
	if err := c.Bind(&req); err != nil || req.Tag == "" {
		return c.JSON(http.StatusBadRequest, apierr.Error("INVALID_PARAM", "tag is required.", "3d81ceae-475f-4600-b2a8-2bc116157532"))
	}
	// upstream show.ts: hashtags.findOneBy({name: normalizeForSearch(tag)})。
	// hashtag.name は normalizeForSearch (NFKC+lowercase) 済で格納されるため、
	// 入力も同じく正規化して一致させる ('Misskey' → 'misskey')。
	// 列に入らない文字を含む tag はどの行とも一致しえない (#3025)。**引く前に
	// 弾く** — 比較の右辺に載せるとクエリごと落ち、下の `err != nil` が
	// **DB 障害まで NO_SUCH_HASHTAG に潰す**経路に流れ込む。
	if !colfit.Storable(req.Tag) {
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_HASHTAG", "No such hashtag.", "110ee688-193e-4a3a-9ecf-c167b2e6981e"))
	}
	var tag model.Hashtag
	if err := h.db.Where("name = ?", searchnorm.Normalize(req.Tag)).First(&tag).Error; err != nil {
		// HTTP semantics 的には not-found = 404 が望ましいが、upstream
		// Misskey TS は ApiError の既定動作で 400 を返している (#925)。
		// drop-in 互換を優先して 400 + NO_SUCH_HASHTAG body に揃える。
		// error id も upstream と同じ ...c167b2e6981e に統一。
		return c.JSON(http.StatusBadRequest, apierr.Error("NO_SUCH_HASHTAG", "No such hashtag.", "110ee688-193e-4a3a-9ecf-c167b2e6981e"))
	}
	return c.JSON(http.StatusOK, packTag(&tag))
}

// Trend handles POST /api/hashtags/trend.
//
// Misskey TS 互換: 直近 200 分 (10 分 × 20 bucket) の note 数を hashtag
// 別に集計し、frontend の MkMiniChart が描画できる時系列を返す。
//
// TS 上流 (HashtagService.getCharts) は Redis HyperLogLog で users 単位
// のユニーク数を count するが、mk-go では HLL を維持せず note テーブル
// から動的に集計する。
//
// mk-go の note テーブルは createdAt 列を持たず、aidx ID 先頭 8 文字に
// ms timestamp が埋め込まれている (`internal/misc/id`)。200 分前の
// cutoff prefix を生成して `id > cutoffID` で範囲フィルタをかける。
// tags 配列 (varchar(128)[]) には migration 000043 で GIN index を張った
// ので `<>` フィルタも index で効く。
//
// visibility フィルタ: TS の HashtagService.updateHashtag は public /
// home の note のみ集計対象にする。followers / specified を含めると
// 公開トレンドに非公開タグが現れてプライバシー上の問題になるため、
// SQL の WHERE で同じ条件を強制する。
//
// 取得した row は Go 側で aidx parse → bucket idx 計算 → tag/bucket 別
// の unique users を集計する。直近 200 分の対象 note 数は中規模インス
// タンスなら数千件オーダーで、Go 側の集計コストは無視できる。
//
// chart の bucket は新しいもの順 (index 0 = 直近 10 分、index 19 = 200 分前)。
// usersCount は TS と同じく `max(chart)` で算出する。
func (h *Handler) Trend(c echo.Context) error {
	const (
		bucketSeconds = 600 // 10 分
		bucketCount   = 20  // 過去 200 分
		topN          = 10
	)

	gen, err := h.generator()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	cutoff := time.Now().Add(-time.Duration(bucketSeconds*bucketCount) * time.Second)
	cutoffID := id.AidxCutoffPrefix(cutoff)

	// model.StringArray を含む struct を Scan(&rows) に渡すと GORM の
	// schema reflection が `unsupported data type: &[]` を warn-log する
	// (model.StringArray の Scanner で runtime には正しく動くが、初期 type
	// infer で躓く)。Rows().Scan() で個別に scan するとこの noise を回避
	// できるので、本 handler では明示的なループ scan を採用する。
	type row struct {
		ID     string
		UserID string
		Tags   []string
	}
	var rows []row
	sqlRows, err := h.db.Raw(`
		SELECT "id", "userId", "tags"
		FROM "note"
		WHERE "id" > ?
		  AND "tags" <> ARRAY[]::varchar[]
		  AND "visibility" IN ('public', 'home')
	`, cutoffID).Rows()
	if err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
		var r row
		var tags model.StringArray
		if err := sqlRows.Scan(&r.ID, &r.UserID, &tags); err != nil {
			return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
		}
		r.Tags = []string(tags)
		rows = append(rows, r)
	}
	if err := sqlRows.Err(); err != nil {
		return c.JSON(http.StatusInternalServerError, apierr.Error("INTERNAL_ERROR", "Internal error.", "5d37dbcb-891e-41ca-a3d6-e690c97775ac"))
	}

	// bucketUsers[tag][bucketIdx] = set of unique userId
	bucketUsers := make(map[string][bucketCount]map[string]struct{})
	totalUsers := make(map[string]map[string]struct{})

	now := time.Now()
	for _, r := range rows {
		ts, perr := gen.ParseTime(r.ID)
		if perr != nil {
			continue
		}
		secsAgo := now.Sub(ts).Seconds()
		if secsAgo < 0 || secsAgo >= float64(bucketSeconds*bucketCount) {
			continue
		}
		bucketIdx := int(secsAgo) / bucketSeconds
		for _, tag := range r.Tags {
			if tag == "" {
				continue
			}
			buckets, exists := bucketUsers[tag]
			if !exists {
				for i := range buckets {
					buckets[i] = map[string]struct{}{}
				}
				bucketUsers[tag] = buckets
			}
			bucketUsers[tag][bucketIdx][r.UserID] = struct{}{}

			if _, exists := totalUsers[tag]; !exists {
				totalUsers[tag] = map[string]struct{}{}
			}
			totalUsers[tag][r.UserID] = struct{}{}
		}
	}

	// ranking: total users 降順、同点は tag name 昇順 (deterministic)
	type ranked struct {
		tag   string
		total int
	}
	rankings := make([]ranked, 0, len(totalUsers))
	for tag, users := range totalUsers {
		rankings = append(rankings, ranked{tag: tag, total: len(users)})
	}
	sort.Slice(rankings, func(i, j int) bool {
		if rankings[i].total != rankings[j].total {
			return rankings[i].total > rankings[j].total
		}
		return rankings[i].tag < rankings[j].tag
	})
	if len(rankings) > topN {
		rankings = rankings[:topN]
	}

	result := make([]map[string]any, 0, len(rankings))
	for _, rk := range rankings {
		chart := make([]int, bucketCount)
		buckets := bucketUsers[rk.tag]
		maxUsers := 0
		for i := 0; i < bucketCount; i++ {
			n := len(buckets[i])
			chart[i] = n
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
	return c.JSON(http.StatusOK, result)
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
	var req struct {
		Tag    string `json:"tag"`
		Sort   string `json:"sort"`
		State  string `json:"state"`
		Origin string `json:"origin"`
		Limit  *int   `json:"limit"`
		Offset int    `json:"offset"`
	}
	if err := c.Bind(&req); err != nil || req.Tag == "" || req.Sort == "" {
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
	limit, limitOK := pagination.ResolveLimit(req.Limit, 10, 100)
	if !limitOK {
		return apierr.JSONInvalidParam(c)
	}
	if req.Offset < 0 {
		req.Offset = 0
	}

	// 列に入らない文字を含む tag はどの行とも一致しえない (#3025)。配列の
	// containment も bind parameter なので、載せるとクエリごと落ちる。
	if !colfit.Storable(req.Tag) {
		return c.JSON(http.StatusOK, []any{})
	}
	// containment query。upstream は `:tag <@ user.tags` (= tag 配列が user.tags の
	// 部分集合)。単一要素なので `tags @> {normTag}` と等価。
	normTag := searchnorm.Normalize(req.Tag)
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
	if err := q.Limit(limit).Offset(req.Offset).Find(&users).Error; err != nil {
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
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	ctx := c.Request().Context()
	packed := make([]entity.UserDetailed, len(users))
	for i, u := range users {
		d := entity.PackUserDetailed(u, profByID[u.ID], gen)
		// 認証 caller には viewer->user の relation block を付与 (匿名/self は no-op、#1957-a)。
		viewerIsFollowing := h.relation.Apply(&d, viewerID, u, profByID[u.ID])
		// **カウントの可視性ゲートを通す (#1558)。** `sort:"+follower"` が
		// 使えるので、忘れると非公開のカウントで並べ替えて読める。
		entity.GateCountVisibility(&d, viewerID == u.ID, false, viewerIsFollowing)
		packed[i] = d
	}
	// 本家は packMany(users, me, {schema: 'UserDetailed'}) なので、ピン留めと
	// 移行先もまとめて埋める (#3330)。匿名の閲覧者にはピン留めを出さない (本家
	// packMany と同じ)。
	if h.extras != nil {
		targets := make([]userpack.DetailTarget, 0, len(users))
		for i, u := range users {
			targets = append(targets, userpack.DetailTarget{User: u, Profile: profByID[u.ID], Detailed: &packed[i]})
		}
		h.extras.FillDetailedExtrasMany(ctx, viewer, targets)
	}
	out := make([]any, 0, len(users))
	for i, u := range users {
		// upstream の pack は isDetailed && isMe で MeDetailed を返すので、
		// 結果に自分が混ざるときは自分だけ MeDetailed になる。
		out = append(out, meself.Pack(ctx, packed[i], u, profByID[u.ID], viewer))
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
