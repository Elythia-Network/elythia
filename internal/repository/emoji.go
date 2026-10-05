package repository

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// bumpEmojiUpdatedAt injects updatedAt=now into an emoji write field map when the
// caller did not set it. upstream CustomEmojiService は update / 各 bulk op で
// 必ず updatedAt = new Date() を書くため、map-based GORM Updates でも同等に bump
// する (model.Emoji.UpdatedAt は autoUpdateTime tag を持たない、#1948-13)。
func bumpEmojiUpdatedAt(fields map[string]any) {
	if _, ok := fields["updatedAt"]; !ok {
		fields["updatedAt"] = time.Now()
	}
}

// emojiNameTokenRe extracts `:name:` tokens from an admin/emoji/list query,
// mirroring upstream list.ts の `/\:([a-z0-9_]*)\:/g` (#1543)。
var emojiNameTokenRe = regexp.MustCompile(`:([a-z0-9_]*):`)

// extractEmojiNameTokens returns the inner names of all `:xxx:` tokens in query
// (空トークンは除外)。1 つ以上見つかれば admin/emoji/list は完全一致集合で絞る。
func extractEmojiNameTokens(query string) []string {
	matches := emojiNameTokenRe.FindAllStringSubmatch(query, -1)
	if len(matches) == 0 {
		return nil
	}
	names := make([]string, 0, len(matches))
	for _, m := range matches {
		if m[1] != "" {
			names = append(names, m[1])
		}
	}
	return names
}

// EmojiRepository provides data access for the `emoji` table.
type EmojiRepository interface {
	FindByNameAndHost(name string, host *string) (*model.Emoji, error)
	// ListLocal returns all local custom emojis (host IS NULL).
	ListLocal() ([]*model.Emoji, error)
	// Admin CRUD
	Create(e *model.Emoji) error
	FindByID(id string) (*model.Emoji, error)
	FindManyByIDs(ids []string) ([]*model.Emoji, error)
	UpdateFields(id string, fields map[string]any) error
	// UpdateFieldsMany applies the same field map to every row whose id is in
	// ids. Used by admin bulk ops (category / license / isSensitive etc.).
	UpdateFieldsMany(ids []string, fields map[string]any) error
	Delete(id string) error
	// DeleteMany removes rows matching any id in ids.
	DeleteMany(ids []string) error
	// ListWithFilter returns emojis matching search/category/host filters.
	// sinceID / untilID は cursor-based pagination 用で、空文字列なら無視。
	// frontend Paginator (cursor 方式) と offset 方式の両方を受け付ける。
	ListWithFilter(query, category string, local bool, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error)
	// FindManyByNamesAndHost returns emojis matching any of the given names for
	// a specific host. host=nil searches local emojis (host IS NULL).
	FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error)
	// FindManyByKeys returns emojis matching any of the given (name, host)
	// pairs, across hosts. It exists for display paths (resolving emoji URLs
	// for packed notes / users) and **may be served from a cache**:
	// CachedEmojiRepository answers it from a bounded TTL cache. Use
	// FindManyByNamesAndHost when the result feeds a write decision.
	//
	// On error the returned slice may still hold the rows that were resolved
	// (e.g. cache hits), so display callers can use it as a partial result.
	FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error)
	// ListRemoteWithFilter mirrors ListWithFilter for remote emojis. host empty
	// matches any remote host.
	ListRemoteWithFilter(query, host, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error)
	// ListV2 returns emojis matching v2 filters with sorting + pagination.
	ListV2(filter model.EmojiV2Filter) ([]*model.Emoji, error)
	// CountV2 returns total count of emojis matching v2 filters (pagination ignored).
	CountV2(filter model.EmojiV2Filter) (int64, error)
}

type emojiRepository struct {
	db *gorm.DB
}

// NewEmojiRepository creates a new EmojiRepository.
func NewEmojiRepository(db *gorm.DB) EmojiRepository {
	return &emojiRepository{db: db}
}

func (r *emojiRepository) ListLocal() ([]*model.Emoji, error) {
	var emojis []*model.Emoji
	if err := r.db.Where("host IS NULL").Order("category ASC, name ASC").Find(&emojis).Error; err != nil {
		return nil, err
	}
	return emojis, nil
}

func (r *emojiRepository) Create(e *model.Emoji) error {
	return r.db.Create(e).Error
}

func (r *emojiRepository) FindByID(id string) (*model.Emoji, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var e model.Emoji
	if err := r.db.Where("id = ?", id).First(&e).Error; err != nil {
		return nil, err
	}
	return &e, nil
}

func (r *emojiRepository) UpdateFields(id string, fields map[string]any) error {
	bumpEmojiUpdatedAt(fields)
	res := r.db.Model(&model.Emoji{}).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	// GORM の Updates は対象 row が存在しなくても Error を返さない。
	// admin/emoji/update が "No such emoji" を正しく返すために
	// RowsAffected==0 を ErrRecordNotFound に昇格する (#650 問題 2)。
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

func (r *emojiRepository) FindManyByIDs(ids []string) ([]*model.Emoji, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var emojis []*model.Emoji
	if err := r.db.Where("id IN ?", ids).Find(&emojis).Error; err != nil {
		return nil, err
	}
	return emojis, nil
}

func (r *emojiRepository) UpdateFieldsMany(ids []string, fields map[string]any) error {
	ids = storableIDs(ids)
	if len(ids) == 0 || len(fields) == 0 {
		return nil
	}
	bumpEmojiUpdatedAt(fields)
	return r.db.Model(&model.Emoji{}).Where("id IN ?", ids).Updates(fields).Error
}

func (r *emojiRepository) Delete(id string) error {
	return r.db.Where("id = ?", id).Delete(&model.Emoji{}).Error
}

func (r *emojiRepository) DeleteMany(ids []string) error {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	return r.db.Where("id IN ?", ids).Delete(&model.Emoji{}).Error
}

func (r *emojiRepository) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	if len(names) == 0 {
		return nil, nil
	}
	q := r.db.Where("name IN ?", names)
	if host == nil {
		q = q.Where("host IS NULL")
	} else {
		q = q.Where("host = ?", *host)
	}
	var emojis []*model.Emoji
	if err := q.Find(&emojis).Error; err != nil {
		return nil, err
	}
	return emojis, nil
}

// emojiKeysChunk caps the number of (name, host) pairs per FindManyByKeys
// query. PostgreSQL のバインド変数は 1 クエリ 65535 個までなので、1 組で 2 個
// 使う remote の pair が大量に来ても上限に届かない大きさで区切る。
const emojiKeysChunk = 500

func (r *emojiRepository) FindManyByKeys(keys []model.EmojiKey) ([]*model.Emoji, error) {
	// 列に入らない文字 (NUL など) を含む値は保存された行に現れないので、一致
	// しえない。**1 つでも IN に載せるとクエリごと落ち**、同じ呼び出しの他の
	// 絵文字まで解決できなくなるので、引く前に落とす (#3025 と同じ扱い)。
	valid := make([]model.EmojiKey, 0, len(keys))
	for _, k := range keys {
		if storable(k.Name) && storable(k.Host) {
			valid = append(valid, k)
		}
	}
	var out []*model.Emoji
	var firstErr error
	for start := 0; start < len(valid); start += emojiKeysChunk {
		end := min(start+emojiKeysChunk, len(valid))
		var remote [][]any
		var local []string
		for _, k := range valid[start:end] {
			if k.Host == "" {
				local = append(local, k.Name)
			} else {
				remote = append(remote, []any{k.Name, k.Host})
			}
		}
		q := r.db.Model(&model.Emoji{})
		switch {
		case len(remote) > 0 && len(local) > 0:
			q = q.Where("(name, host) IN ?", remote).Or("host IS NULL AND name IN ?", local)
		case len(remote) > 0:
			q = q.Where("(name, host) IN ?", remote)
		default:
			q = q.Where("host IS NULL AND name IN ?", local)
		}
		var rows []*model.Emoji
		if err := q.Find(&rows).Error; err != nil {
			// 1 つの chunk が落ちても残りは引く。表示の経路は返った行を使うので、
			// ここで止めると落ちた chunk より後の絵文字まで出なくなる。
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, rows...)
	}
	return out, firstErr
}

func (r *emojiRepository) ListRemoteWithFilter(query, host, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(query) || !storable(host) {
		return nil, nil
	}
	// **paginationOrder を使わないのは意図的** (#2713)。upstream
	// admin/emoji/list-remote.ts:62,75 は makePaginationQuery の**後に**
	// `.orderBy('emoji.id', 'DESC')` を呼ぶ。TypeORM の orderBy は既存の
	// ORDER BY を上書きするので、upstream は sinceId 単独でも DESC のまま。
	// ここを paginationOrder に寄せると逆に乖離する。
	//
	// 兄弟の ListWithFilter (local) は upstream が上書きしないので
	// paginationOrder を使っており、非対称なのはそのため。
	q := r.db.Where("host IS NOT NULL").Order("id DESC")
	if query != "" {
		// upstream list-remote.ts:72 は `name LIKE :query` (Postgres LIKE は
		// case-sensitive) かつ sqlLikeEscape で %/_ を literal 化する。mk-go も
		// case-sensitive LIKE + escapeLike + ESCAPE '\' に揃える (#1948-13)。
		q = q.Where(`name LIKE ? ESCAPE '\'`, "%"+escapeLike(query)+"%")
	}
	if host != "" {
		q = q.Where("host = ?", host)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q = q.Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	var emojis []*model.Emoji
	if err := q.Find(&emojis).Error; err != nil {
		return nil, err
	}
	return emojis, nil
}

func (r *emojiRepository) ListWithFilter(query, category string, local bool, sinceID, untilID string, limit, offset int) ([]*model.Emoji, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(query) || !storable(category) {
		return nil, nil
	}
	// upstream list.ts は makePaginationQuery を使うため、カーソル無し時は
	// id DESC (最新が先頭)、sinceId のみ指定時は id ASC で並べる (#1543)。
	q := r.db.Order(paginationOrder(sinceID, untilID, "id"))
	if local {
		q = q.Where("host IS NULL")
	}
	if query != "" {
		// upstream list.ts: query が :name: 形式を含むなら該当 emoji 名の完全一致
		// 集合で絞り、そうでなければ name / aliases / category のいずれかに部分
		// 一致する emoji を返す (#1543)。
		if names := extractEmojiNameTokens(query); len(names) > 0 {
			q = q.Where("name IN ?", names)
		} else {
			// upstream list.ts は JS String.includes (case-sensitive 部分一致) で
			// in-memory filter する。mk-go も case-sensitive LIKE に揃える (#1948-13)。
			// aliases は upstream の `aliases.some(a => a.includes(q))` と同じく要素単位で
			// 突合する (array_to_string 結合だと要素境界を跨いだ誤 match が起きる、#2034)。
			like := "%" + escapeLike(query) + "%"
			q = q.Where(`name LIKE ? ESCAPE '\' OR EXISTS (SELECT 1 FROM unnest(aliases) AS alias WHERE alias LIKE ? ESCAPE '\') OR category LIKE ? ESCAPE '\'`, like, like, like)
		}
	}
	if category != "" {
		q = q.Where("category = ?", category)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	q = q.Limit(limit)
	if offset > 0 {
		q = q.Offset(offset)
	}
	var emojis []*model.Emoji
	if err := q.Find(&emojis).Error; err != nil {
		return nil, err
	}
	return emojis, nil
}

// v2SortAllowList is the allow-list of column names accepted in sortKeys.
var v2SortAllowList = map[string]string{
	"id":          "id",
	"updatedAt":   `"updatedAt"`,
	"name":        "name",
	"host":        "host",
	"uri":         "uri",
	"publicUrl":   `"publicUrl"`,
	"type":        "type",
	"category":    "category",
	"license":     "license",
	"isSensitive": `"isSensitive"`,
	"localOnly":   `"localOnly"`,
	"aliases":     "aliases",
	"roleIdsThatCanBeUsedThisEmojiAsReaction": `"roleIdsThatCanBeUsedThisEmojiAsReaction"`,
}

// multipleWordsToQuery mirrors upstream CustomEmojiService.multipleWordsToQuery:
// クエリを空白で分割し、各語を sqlLikeEscape(escapeLike) して `%word%` の LIKE
// パターン配列にする。各フィールドはこの配列に対する `~~ ANY(...)` (case-sensitive
// LIKE の OR) で突合する (#1999)。
func multipleWordsToQuery(words string) []string {
	parts := strings.Fields(words)
	out := make([]string, 0, len(parts))
	for _, x := range parts {
		// **語ごとに落とす (#3025)。** ここは OR 展開なので、NUL を含む語の枝が
		// 偽になるだけで他の語の一致は残る。全部落ちたら空になり、呼び出し側の
		// `len(patterns) == 0` が `1 = 0` に倒す。
		if !storable(x) {
			continue
		}
		out = append(out, "%"+escapeLike(x)+"%")
	}
	return out
}

// buildV2Query constructs the common WHERE clause for ListV2/CountV2.
//
// upstream CustomEmojiService.fetchEmojis に揃える (#1999): 各テキストフィールドは
// case-sensitive な `LIKE ANY(...)` + multipleWordsToQuery (空白分割 OR) + escapeLike、
// updatedAt は `CAST(... AS DATE)` で date 切り捨て、aliases は unnest で要素単位突合。
func (r *emojiRepository) buildV2Query(filter model.EmojiV2Filter) *gorm.DB {
	q := r.db.Model(&model.Emoji{})
	if filter.Query != nil {
		fq := filter.Query
		// **LIKE を通らない値もここで弾く (#3025)。** 日付は
		// `multipleWordsToQuery` を経由しないので、NUL を含むとクエリごと落ちる。
		// こちらは AND なので、一致しえない以上は空結果 (`1 = 0`) が正しい。
		if !storable(fq.UpdatedAtFrom) || !storable(fq.UpdatedAtTo) {
			return q.Where("1 = 0")
		}
		// **role id は overlap (`&&`) = OR なので要素ごとに落とす。** 全体を
		// 空にすると、一緒に指定した他の role の一致まで消える。書き込み側の
		// `normalizeEmojiRoleIDs` (#3018) と同じ扱い。
		//
		// **`fq` に書き戻さない。** `filter.Query` はポインタなので、書き戻すと
		// 同じ filter で 2 回呼ぶ経路 (`ListV2` の直後に `CountV2`) の 2 回目で
		// `len(fq.RoleIDs) > 0` が偽になり、**guard も後段のフィルタも両方
		// 素通りして全件を数える** (実測で `allCount` が絞り前の件数になった)。
		roleIDs := storableIDs(fq.RoleIDs)
		if len(fq.RoleIDs) > 0 && len(roleIDs) == 0 {
			// 指定はあったのに 1 つも残らなかった = どの role にも一致しない。
			// ここでフィルタごと落とすと**絞ったつもりで全件出る**。
			return q.Where("1 = 0")
		}
		// likeAny applies `(<col> LIKE p1 OR <col> LIKE p2 ...)` (case-sensitive,
		// ESCAPE '\') over the whitespace-split escaped patterns = upstream の
		// `<col> ~~ ANY(multipleWordsToQuery(...))` と等価。空白のみの値は ANY(空) =
		// false 相当で空結果にする。OR 展開で `ARRAY[?]` の record 化を避ける。
		likeAny := func(col, val string) {
			if val == "" {
				return
			}
			patterns := multipleWordsToQuery(val)
			if len(patterns) == 0 {
				q = q.Where("1 = 0")
				return
			}
			conds := make([]string, len(patterns))
			args := make([]any, len(patterns))
			for i, p := range patterns {
				conds[i] = col + ` LIKE ? ESCAPE '\'`
				args[i] = p
			}
			q = q.Where("("+strings.Join(conds, " OR ")+")", args...)
		}
		// host: upstream は local→IS NULL、remote→host 指定時 LIKE ANY / 未指定時
		// IS NOT NULL。combined/未指定 hostType では host 条件を付けない。
		switch fq.HostType {
		case "local":
			q = q.Where("host IS NULL")
		case "remote":
			if fq.Host != "" {
				likeAny("host", fq.Host)
			} else {
				q = q.Where("host IS NOT NULL")
			}
		}
		likeAny("name", fq.Name)
		likeAny("category", fq.Category)
		likeAny("type", fq.Type)
		likeAny("license", fq.License)
		likeAny("uri", fq.URI)
		likeAny(`"publicUrl"`, fq.PublicURL)
		// originalUrl は upstream v2 list の paramDef にあるが fetchEmojis では WHERE 句に
		// 使われず無視される。filter すると upstream と乖離するため適用しない (#2034。
		// EmojiV2Query.OriginalURL は request 互換のため残すが query では使わない)。
		// aliases は配列要素ごとに突合する (upstream は unnest(aliases) の subquery)。
		// array_to_string 結合では要素境界を跨いだ誤 match が起きるため避ける。
		if fq.Aliases != "" {
			patterns := multipleWordsToQuery(fq.Aliases)
			if len(patterns) == 0 {
				q = q.Where("1 = 0")
			} else {
				conds := make([]string, len(patterns))
				args := make([]any, len(patterns))
				for i, p := range patterns {
					conds[i] = `alias LIKE ? ESCAPE '\'`
					args[i] = p
				}
				q = q.Where(`EXISTS (SELECT 1 FROM unnest(aliases) AS alias WHERE (`+strings.Join(conds, " OR ")+`))`, args...)
			}
		}
		if fq.IsSensitive != nil {
			q = q.Where(`"isSensitive" = ?`, *fq.IsSensitive)
		}
		if fq.LocalOnly != nil {
			q = q.Where(`"localOnly" = ?`, *fq.LocalOnly)
		}
		// updatedAtFrom/To は upstream 同様 date 単位に切り捨てて突合する。
		if fq.UpdatedAtFrom != "" {
			q = q.Where(`CAST("updatedAt" AS DATE) >= ?`, fq.UpdatedAtFrom)
		}
		if fq.UpdatedAtTo != "" {
			q = q.Where(`CAST("updatedAt" AS DATE) <= ?`, fq.UpdatedAtTo)
		}
		if len(roleIDs) > 0 {
			q = q.Where(`"roleIdsThatCanBeUsedThisEmojiAsReaction" && ARRAY[?]::varchar[]`, roleIDs)
		}
	}
	if filter.SinceID != "" {
		q = q.Where("id > ?", filter.SinceID)
	}
	if filter.UntilID != "" {
		q = q.Where("id < ?", filter.UntilID)
	}
	return q
}

func (r *emojiRepository) ListV2(filter model.EmojiV2Filter) ([]*model.Emoji, error) {
	q := r.buildV2Query(filter)

	// 有効なsortKeyが1つもなければid DESCにfallbackする
	applied := false
	for _, sk := range filter.SortKeys {
		if len(sk) < 2 {
			continue
		}
		dir := sk[0]
		col := sk[1:]
		dbCol, ok := v2SortAllowList[col]
		if !ok {
			continue
		}
		if dir == '-' {
			q = q.Order(fmt.Sprintf("%s DESC", dbCol))
		} else {
			q = q.Order(fmt.Sprintf("%s ASC", dbCol))
		}
		applied = true
	}
	if !applied {
		q = q.Order("id DESC")
	}

	limit := filter.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	q = q.Limit(limit)

	if filter.Page > 0 {
		q = q.Offset((filter.Page - 1) * limit)
	}

	var emojis []*model.Emoji
	if err := q.Find(&emojis).Error; err != nil {
		return nil, err
	}
	return emojis, nil
}

func (r *emojiRepository) CountV2(filter model.EmojiV2Filter) (int64, error) {
	q := r.buildV2Query(filter)
	var count int64
	if err := q.Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

// FindByNameAndHost looks up a custom emoji by its name and host.
// host=nil の場合はローカル絵文字 (host IS NULL) として検索する。
func (r *emojiRepository) FindByNameAndHost(name string, host *string) (*model.Emoji, error) {
	if !storable(name) || (host != nil && !storable(*host)) {
		return nil, ErrNotFound
	}
	var e model.Emoji
	q := r.db.Where("name = ?", name)
	if host == nil {
		q = q.Where("host IS NULL")
	} else {
		q = q.Where("host = ?", *host)
	}
	if err := q.First(&e).Error; err != nil {
		return nil, err
	}
	return &e, nil
}
