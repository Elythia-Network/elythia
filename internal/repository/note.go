package repository

import (
	"gorm.io/plugin/dbresolver"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/misc/searchnorm"
	"github.com/shiroha-a/mk/internal/model"
	"gorm.io/gorm"
)

// aidxTime2000Ms は aidx ID のタイムスタンプ部分 (最初の8文字 base36) の
// epoch 起点 (2000-01-01T00:00:00Z in ms since Unix epoch)。
// misc/id パッケージに同値の非公開定数があるが、本ファイル内で時刻→ID 先頭
// 変換に使うため独自に定義する。
const aidxTime2000Ms int64 = 946684800000

// FeaturedNotesPerUserPoolSize は users/featured-notes の selection 段で
// engagement DESC top-N を SQL から取得する際の上限。upstream
// FeaturedService.getPerUserNotesRanking の threshold (50) と揃える (#1487 / #1491)。
// 選抜後は id DESC + untilID cursor + limit でページングする。
//
// テスト用 mock (internal/testutil/mock_repository.go の ListFeaturedByUser) も
// この定数を直接参照することで mock と real repo の pool cap が drift しない
// ことを保証する (#1491 review B 指摘の二重定義解消)。
const FeaturedNotesPerUserPoolSize = 50

// pureRenoteCondSQL は pure renote (= text/cw/files/poll/reply を伴わない renote)
// を表す SQL 断片。upstream isPureRenote (= !isQuote) と揃える (#1888)。timeline
// (withRenotes) / home TL / renote-mute の各 filter で共有し、列定義の drift を防ぐ。
// 列は全 caller で曖昧にならないよう非修飾 (JOIN 先に同名列は無い)。
//
// text/cw/replyId は IS NULL のみ判定する (空文字は判定しない)。upstream isQuote は
// `text != null` 等の null 判定なので、空文字 text="" の renote は upstream でも quote
// 扱いになり、この SQL と一致する (= NULL のみが pure)。空文字を pure とみなす方向に
// 変えない (それは upstream から乖離する)。
const pureRenoteCondSQL = `"renoteId" IS NOT NULL AND "text" IS NULL AND "cw" IS NULL AND "fileIds" = '{}' AND "hasPoll" = false AND "replyId" IS NULL`

// renoteHasContentSQL は pureRenoteCondSQL の content 部の否定 (= quote renote:
// text/cw/files/poll/reply のいずれかを伴う renote)。children listing で quote を
// 含めるのに使う (#1888)。
const renoteHasContentSQL = `("text" IS NOT NULL OR "cw" IS NOT NULL OR "fileIds" != '{}' OR "hasPoll" = TRUE OR "replyId" IS NOT NULL)`

// aidxCutoffID は与えられた time に対応する最小のaidx ID文字列を返す。
// aidxは「時刻base36(8) + nodeID(4) + counter(4)」の 16 文字で、先頭 8 文字が
// ms-since-2000 を base36 で表したもの。そのため lexicographic 比較で時刻順に
// 並ぶ。時刻以降の全IDを 「id >= aidxCutoffID(t)」 で、時刻以前を
// 「id < aidxCutoffID(t)」 で拾える。
func aidxCutoffID(t time.Time) string {
	ms := t.UnixMilli() - aidxTime2000Ms
	if ms < 0 {
		ms = 0
	}
	prefix := strconv.FormatInt(ms, 36)
	if len(prefix) < 8 {
		prefix = strings.Repeat("0", 8-len(prefix)) + prefix
	} else if len(prefix) > 8 {
		// 想定外 (2089年以降) だが安全側で末尾8文字を使う
		prefix = prefix[len(prefix)-8:]
	}
	return prefix + "00000000"
}

// NoteRepository provides data access for notes.
type NoteRepository interface {
	Create(note *model.Note) error
	FindByID(id string) (*model.Note, error)
	// FindByIDWithUser fetches a note with only its author (`User`) preloaded.
	// Renote / Reply の embed までは要らない caller (visibility チェック等) 向け
	// の軽量経路 (#425)。それらが必要な場合は FindByIDWithRelations を使う。
	FindByIDWithUser(id string) (*model.Note, error)
	// FindByIDWithRelations fetches a note with User / Renote / Renote.User /
	// Reply / Reply.User を 1 階層分 preload する。pack/render に流す経路向け
	// (#425)。GORM の preload は明示した relation しか辿らないため N+1 には
	// ならない。
	FindByIDWithRelations(id string) (*model.Note, error)
	FindByURI(uri string) (*model.Note, error)
	Delete(note *model.Note) error
	Update(note *model.Note, column string, value any) error
	UpdateFields(noteID string, fields map[string]any) error
	IncrementCount(noteID, column string, delta int) error
	IncrementReaction(noteID, reaction string, delta int) error
	ListByUserID(userID string, untilID, sinceID string, limit int) ([]*model.Note, error)
	// ListPublicNotesForFeed returns the notes shown in a user's RSS/Atom/JSON feed.
	ListPublicNotesForFeed(userID string, limit int) ([]*model.Note, error)
	// ListPublicByUserID is ListByUserID restricted to AP-servable notes
	// (visibility IN (public, home) AND NOT localOnly), for the actor outbox (#1878)。
	ListPublicByUserID(userID string, untilID, sinceID string, limit int) ([]*model.Note, error)
	// ListByUserIDFiltered returns notes owned by userID with upstream
	// `users/notes` filter parameters applied. 4 つの bool は upstream paramDef
	// と同じ semantics で各々:
	//   - withFiles: true なら fileIds 非空のみ (デフォルト false)
	//   - withReplies: false なら reply を除外 (デフォルト true)
	//   - withRenotes: false なら pure renote を除外 (デフォルト true)
	//   - withChannelNotes: false なら channel 投稿を除外 (デフォルト false)
	//
	// caller (handler) は JSON pointer field から bool 値を必ず upstream の
	// デフォルトに合わせて詰めること (#1021)。
	//
	// viewerID は visibility push-down のための閲覧者 ID。空文字は匿名
	// (public/home のみ)。core/note.CanSeeNote と同じ可視性条件を LIMIT 前に
	// SQL で絞ることで、post-fetch filter によるページ過少充填と followers
	// 判定の N+1 を避ける (#1418 review)。
	ListByUserIDFiltered(userID, viewerID, untilID, sinceID string, limit int, withFiles, withReplies, withRenotes, withChannelNotes bool) ([]*model.Note, error)
	// ListByChannelID は channel の note を viewer の可視性で絞って返す。
	// channels/timeline は public channel 自体が誰でも閲覧可能だが、そこに投稿
	// された note 個々の visibility (followers / specified) は viewer 単位で
	// 絞らないと非フォロワーに followers note が流出する (IDOR, #1440)。
	// viewerID 空文字は匿名 (public/home のみ)。条件は core/note.CanSeeNote と
	// 一致 (clip_note.ListByClipVisible と同じ式)。#1449 review で
	// ListByChannelIDVisible を別メソッドに切り出さず本 signature に viewerID を
	// 取り込む形に統合した (#1439 / #1441 と同じパターン)。
	ListByChannelID(channelID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error)
	// ListLocalByChannelID is ListByChannelID restricted to notes whose
	// author is local (`note.userHost IS NULL`), applied before LIMIT. Used
	// for anonymous visitors under ugcVisibilityForVisitor = local (upstream
	// generateUgcVisibilityQueryForVisitor).
	ListLocalByChannelID(channelID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error)
	FindManyByIDsWithUser(ids []string) ([]*model.Note, error)
	// ExistingNoteIDsOnPrimary returns the subset of ids that exist, reading
	// from the primary DB even when read replicas are configured.
	//
	// レプリカ構成では通常の SELECT が複製前の行を取りこぼす。「行が無い」を
	// 根拠に破壊的な操作をする経路はレプリカを見てはいけないので、primary を
	// 明示する。現在の利用者は antenna (#2719) と timeline (#2757) の宙吊り
	// ID 除去。
	ExistingNoteIDsOnPrimary(ids []string) ([]string, error)
	// ListRenotesOf / ListRepliesOf / ListChildrenOf は viewerID 視点の可視性を
	// LIMIT 前に SQL push-down する (#1500)。viewerID="" は匿名 (public/home のみ)。
	// 条件は core/note.CanSeeNote 完全版 (specified 込み): スレッドの reply/renote は
	// viewer が visibleUserIds 対象なら specified note も見えるため、timeline 系の
	// specified 除外版を流用しないこと。post-fetch filter だとページ過少充填 +
	// followers 判定 N+1 になるため (#1418 / #1452 と同 doctrine)。
	ListRenotesOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error)
	// ListRenoteOrReplyRemoteUserIDs returns the ids of remote users who
	// renoted or replied to noteID (#2995)。ノートを削除したとき、その相手の
	// サーバーにも Delete を届けるために使う。
	ListRenoteOrReplyRemoteUserIDs(noteID string) ([]string, error)
	ListRepliesOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error)
	ListChildrenOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error)
	SearchByFilter(filter model.NoteSearchFilter) ([]*model.Note, error)
	// ListFeatured returns ranked public notes (renote+reply count). When
	// channelID is non-empty restricts to that channel, mirroring upstream
	// FeaturedService.getInChannelNotesRanking. untilID enables cursor
	// pagination (id < untilID). offset is honoured only when no cursor
	// is given.
	ListFeatured(channelID, untilID string, limit, offset int) ([]*model.Note, error)
	// ListFeaturedByUser returns userID's featured notes for users/featured-notes.
	// upstream は Redis sorted set (FeaturedService.getPerUserNotesRanking) で
	// engagement 上位を選抜してから id DESC で並べ替え untilID でページング
	// する 2 段構成。mk-go も同じ 2 段で揃える (#1487 Option B):
	//
	//  1. selection: engagement = (renoteCount + repliesCount) DESC, id DESC で
	//     FeaturedNotesPerUserPoolSize (=50, upstream 同値) 件を SQL push-down で
	//     選抜。visibility 条件と channel 除外もここで同時に絞る (LIMIT 前)。
	//     post-fetch FilterVisible だとページ過少充填 + followers 判定 N+1 を
	//     起こすため (#1418 / #1440 と同 doctrine)。
	//  2. display: 選抜結果を Go 側で id DESC に並べ替え、untilID < id でページング
	//     してから先頭 limit 件を返す。これで「engagement で選抜・id で表示」が
	//     upstream と一致し、id cursor とソート順がずれて発生する重複/欠落を
	//     回避する (#1491 review)。
	//
	// viewerID は viewer 視点の visibility push-down 用。空文字は匿名
	// (public/home のみ)。
	ListFeaturedByUser(userID, viewerID, untilID string, limit int) ([]*model.Note, error)
	// ListRenotesByUser returns all notes authored by userID that renote renoteID.
	ListRenotesByUser(userID, renoteID string) ([]*model.Note, error)
	// ListMentions returns notes mentioning userID that userID can see.
	// visibility が空でなければ note.visibility = visibility の exact-match で
	// 絞る (upstream TS notes/mentions と同じ; 空は全種別)。振り分けを LIMIT 前に
	// SQL で行うことで handler の post-fetch 振り分けによる under-fill を解消する
	// (#1451)。following=true のとき note.userId が viewer の followee または
	// viewer 自身に限定する (upstream mentions.ts following param、#1554)。
	ListMentions(userID, visibility string, following bool, limit int, sinceID, untilID string) ([]*model.Note, error)
	// SearchByTag returns notes carrying tag that viewerID is allowed to see.
	// viewerID 空文字は匿名 (public/home のみ)。discovery 系の tag 検索は
	// ID 既知公開 (notes/show) doctrine の対象外なので、core/note.CanSeeNote と
	// 同じ可視性条件を LIMIT 前に SQL で絞る (#1439)。filter で reply/renote/poll/
	// withFiles を絞る (upstream search-by-tag.ts、#1554)。tagGroups は外側 OR・
	// 内側 AND の複合タグ検索 (string[][]、#1683)。単一タグは [][]string{{tag}}。
	SearchByTag(tagGroups [][]string, viewerID string, limit int, sinceID, untilID string, filter model.NoteSearchTagFilter) ([]*model.Note, error)
	// ListByFileID returns notes whose fileIds array contains fileID, with
	// cursor (sinceID/untilID) pagination matching upstream Misskey's
	// makePaginationQuery. limit <= 0 defaults to 10.
	ListByFileID(fileID, sinceID, untilID string, limit int) ([]*model.Note, error)
	IncrementUserNotesCount(userID string, delta int) error
	ListHomeTimeline(userID string, limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error)
	ListLocalTimeline(limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error)
	ListGlobalTimeline(limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error)
	// ListPublicNotes returns public, non-localOnly notes for the upstream
	// notes.ts public-note timeline (#2106 L4 / #2215), with optional
	// local/reply/renote/withFiles/poll filters and keyset pagination.
	ListPublicNotes(filter model.PublicNotesFilter, limit int, sinceID, untilID string) ([]*model.Note, error)
	// DeleteExpiredRemoteNotes deletes up to batchSize remote notes older
	// than expiryDays in a single DELETE statement. Returns the count
	// actually removed in this batch. Callers drive the loop themselves so
	// cancellation checkpoints and sleep pacing live in the processor
	// (see CleanRemoteNotesProcessor).
	DeleteExpiredRemoteNotes(expiryDays, batchSize int) (int64, error)
	// DeleteExpiredRemoteNotesAfter is DeleteExpiredRemoteNotes that walks
	// root notes in id order starting after the given cursor ("" = from the
	// beginning). It returns the note rows deleted, the number of roots
	// examined (fewer than batchSize = reached the end) and the id of the
	// last root examined (to resume after; "" when none).
	DeleteExpiredRemoteNotesAfter(expiryDays, batchSize int, after string) (deleted int64, scanned int, lastRootID string, err error)
	// DeleteByUserBatch deletes up to batchSize notes authored by userID in a
	// single DELETE statement. Returns the count actually removed. Callers
	// drive the loop themselves so that cancellation checkpoints and sleep
	// pacing live in the processor (see DeleteAccountProcessor).
	DeleteByUserBatch(userID string, batchSize int) (int64, error)
	// ListByUserList returns notes authored by members of the given user list
	// with keyset pagination via paginationOrder (DESC by default; ASC when
	// only sinceID is supplied). Channel notes are excluded.
	//
	// filter.ViewerID は viewer 視点の visibility push-down 用。followers は viewer
	// が author 本人か follow 済みのときだけ、specified は list timeline では除外
	// (DM 非表示 / upstream 準拠)。空文字は匿名 (public/home のみ)。これを LIMIT
	// 前に SQL で絞ることで、handler post-fetch FilterVisible のページ過少充填と
	// followers 判定 N+1 を解消する (#1452, #1418 / #1486 と同 doctrine)。
	//
	// 返信は user_list_membership.withReplies を尊重して出し分ける (#1496, upstream
	// と一致): 返信でない / 自己への返信 / viewer 宛ての返信 / メンバーが
	// withReplies=ON のときだけ含め、第三者宛ての返信は既定で除外する。
	//
	// filter の withRenotes / includeMyRenotes / includeRenotedMyNotes /
	// includeLocalRenotes / withFiles は applyTimelineFilter で他 timeline と同じ
	// SQL を適用する (#1498, upstream getFromDb と一致)。muting / WithReplies は
	// user-list-timeline では使わない (返信は上記 per-member 経路、muting は対象外)。
	ListByUserList(listID string, limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error)
	// CountReplyTargets returns the users that userID most frequently replies
	// to, ordered by reply count descending. Used by
	// users/get-frequently-replied-users.
	//
	// viewerID は viewer 視点での visibility push-down に使う。userID の reply note
	// のうち viewer が core/note.CanSeeNote で見られるものだけを集計する。空文字は
	// 匿名 (public/home のみ)。これが無いと userID の followers/specified reply の
	// 対人関係が第三者に leak する (#1486)。
	CountReplyTargets(userID, viewerID string, limit int) ([]model.ReplyTargetCount, error)
	// CountLocalNotes returns the number of notes authored by local users
	// (userHost IS NULL). nodeinfo `usage.localPosts` 相当 (#403)。
	CountLocalNotes() (int64, error)
	// CountLocalComments returns the number of reply notes from local users.
	// nodeinfo `usage.localComments` 相当 (#403)。
	CountLocalComments() (int64, error)
}

// NotePrimaryReader exposes the small set of note reads whose authorization
// decision must observe the primary immediately after pin, visibility or
// author-preference changes. Ordinary timeline/list reads remain replica-safe.
type NotePrimaryReader interface {
	FindByIDWithUserOnPrimary(id string) (*model.Note, error)
	FindByIDWithRelationsOnPrimary(id string) (*model.Note, error)
	FindManyByIDsWithUserOnPrimary(ids []string) ([]*model.Note, error)
	ListPublicByUserIDOnPrimary(userID string, untilID, sinceID string, limit int) ([]*model.Note, error)
}

type noteRepository struct {
	db *gorm.DB
}

// NewNoteRepository creates a new NoteRepository.
func NewNoteRepository(db *gorm.DB) NoteRepository {
	return &noteRepository{db: db}
}

// preloadNoteRelations applies Preload for the author plus the reply/renote
// targets (with their authors) needed by the NoteEntity packer. NoteEntity の
// packer は renote/reply の target note を埋めるためこれらの preload が必須。
// GORM の preload は明示した relation しか辿らないので N+1 には成らない。
//
// renote ブランチだけ 2 段展開する: 純粋リノート (boost) が引用投稿 (quote) を
// 包む場合、renote (= quote) のさらに renote (= 引用先) まで preload しないと
// frontend が引用先を「削除されたノート」として描画してしまう (packer の
// maxNoteEmbedDepth=2 と一致)。packer は renote を detail 付きで pack するので
// renote.renote と renote.reply の 2 つが depth-2 に出る。reply は detail 無しで
// pack するため reply.* は展開されない。
//
// Poll は note.hasPoll==true のとき 1:1 で attach する (#690)。Preload は
// 自動的に hasPoll=false の note では何も読まないので overhead は小さい
// (発行されるのは poll table への 1 IN 句 query)。
func preloadNoteRelations(db *gorm.DB) *gorm.DB {
	return db.Preload("User").
		Preload("Renote").
		Preload("Renote.User").
		Preload("Renote.Poll").
		Preload("Renote.Renote").
		Preload("Renote.Renote.User").
		Preload("Renote.Renote.Poll").
		Preload("Renote.Reply").
		Preload("Renote.Reply.User").
		Preload("Reply").
		Preload("Reply.User").
		Preload("Reply.Poll").
		Preload("Poll")
}

func (r *noteRepository) Create(note *model.Note) error {
	return r.db.Create(note).Error
}

func (r *noteRepository) FindByID(id string) (*model.Note, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var note model.Note
	if err := r.db.First(&note, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

func (r *noteRepository) FindByIDWithUser(id string) (*model.Note, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var note model.Note
	if err := r.db.Preload("User").First(&note, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

func (r *noteRepository) FindByIDWithUserOnPrimary(id string) (*model.Note, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var note model.Note
	if err := r.db.Clauses(dbresolver.Write).Preload("User").First(&note, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

func (r *noteRepository) FindByIDWithRelations(id string) (*model.Note, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var note model.Note
	if err := preloadNoteRelations(r.db).First(&note, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

func (r *noteRepository) FindByIDWithRelationsOnPrimary(id string) (*model.Note, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var note model.Note
	if err := preloadNoteRelations(r.db.Clauses(dbresolver.Write)).First(&note, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

// FindByURI looks up a note by its ActivityPub URI. リモート由来の note は
// uri 列に作成元のIRIが入っているため、配信や inbox 処理での重複検出に使う。
func (r *noteRepository) FindByURI(uri string) (*model.Note, error) {
	if !storable(uri) {
		return nil, ErrNotFound
	}
	var note model.Note
	if err := r.db.Where("uri = ?", uri).First(&note).Error; err != nil {
		return nil, err
	}
	return &note, nil
}

func (r *noteRepository) Delete(note *model.Note) error {
	return r.db.Delete(note).Error
}

func (r *noteRepository) Update(note *model.Note, column string, value any) error {
	return r.db.Model(note).Update(column, value).Error
}

// UpdateFields applies a map of column → value updates to the note row keyed
// by id. Update (single column) は既存呼び出し互換のため残す。
func (r *noteRepository) UpdateFields(noteID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Note{}).Where("id = ?", noteID).Updates(fields).Error
}

// CountLocalNotes returns the total number of notes authored by local users,
// including replies. nodeinfo `usage.localPosts` は本家 Misskey / Mastodon
// と同じ「local post の総数」定義なので replies も含めて数える。
// localComments (reply subset) との重複は仕様どおり。
// また soft-delete された user 由来の note も含む (userHost IS NULL だけで判定)。
// 本家 Misskey の countNotes も同じ挙動で、orphan note を除外するには
// 別途 JOIN が要るが nodeinfo の用途では不要コスト。
func (r *noteRepository) CountLocalNotes() (int64, error) {
	var count int64
	err := r.db.Model(&model.Note{}).
		Where(`"userHost" IS NULL`).
		Count(&count).Error
	return count, err
}

// CountLocalComments returns the number of reply notes by local users.
func (r *noteRepository) CountLocalComments() (int64, error) {
	var count int64
	err := r.db.Model(&model.Note{}).
		Where(`"userHost" IS NULL`).
		Where(`"replyId" IS NOT NULL`).
		Count(&count).Error
	return count, err
}

// IncrementCount adjusts a counter column on the note row by delta. A
// negative delta never takes the counter below 0.
// 集計列の更新はGORMのUpdateColumnでSQL式を直接適用する。
//
// 減らすときは 0 で止める (#3291)。カウンタを維持する前に作られた行は実件数より
// 小さいまま残っており (例: #1768 より前にクリップした行の clippedCount は 0)、
// そこから減らすと負になる。負の clippedCount は API にそのまま出るうえ、
// リモートノートの掃除の条件 (`clippedCount = 0`) に一致しなくなる。
func (r *noteRepository) IncrementCount(noteID, column string, delta int) error {
	expr := gorm.Expr("\""+column+"\" + ?", delta)
	if delta < 0 {
		expr = gorm.Expr("GREATEST(\""+column+"\" + ?, 0)", delta)
	}
	return r.db.Model(&model.Note{}).
		Where("id = ?", noteID).
		UpdateColumn(column, expr).Error
}

// IncrementReaction increments (or decrements when delta<0) the value of a
// reaction key inside the note.reactions JSONB object. 0以下になったキーは
// JSONBオブジェクトから完全に削除される。
//
// 結果値が正なら jsonb_set でカウントを上書き、0以下なら "reactions - key" で
// キーごと削除する。 CASE 式で1クエリにまとめている。
func (r *noteRepository) IncrementReaction(noteID, reaction string, delta int) error {
	expr := "CASE WHEN COALESCE((reactions->>?)::int, 0) + ? > 0 " +
		"THEN jsonb_set(reactions, ?, to_jsonb(COALESCE((reactions->>?)::int, 0) + ?)::jsonb, true) " +
		"ELSE reactions - ?::text END"
	pathArr := "{" + reaction + "}"
	return r.db.Model(&model.Note{}).
		Where("id = ?", noteID).
		UpdateColumn("reactions",
			gorm.Expr(expr, reaction, delta, pathArr, reaction, delta, reaction)).Error
}

// ListByUserID returns the user's notes ordered by id, optionally
// constrained by sinceID/untilID for keyset pagination.
func (r *noteRepository) ListByUserID(userID string, untilID, sinceID string, limit int) ([]*model.Note, error) {
	var notes []*model.Note
	q := preloadNoteRelations(r.db).Where("\"userId\" = ?", userID)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) ListPublicByUserID(userID string, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return r.listPublicByUserID(r.db, userID, untilID, sinceID, limit)
}

func (r *noteRepository) listPublicByUserID(db *gorm.DB, userID string, untilID, sinceID string, limit int) ([]*model.Note, error) {
	var notes []*model.Note
	q := preloadNoteRelations(db).
		Where("\"userId\" = ?", userID).
		Where("visibility IN ?", []string{"public", "home"}).
		Where("\"localOnly\" = ?", false)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) ListPublicByUserIDOnPrimary(userID string, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return r.listPublicByUserID(r.db.Clauses(dbresolver.Write), userID, untilID, sinceID, limit)
}

// ListByUserIDFiltered は ListByUserID に upstream 互換 filter を加えた版。
// fileIds 非空 / replyId / renoteId / channelId それぞれの SQL filter を
// optional に追加する。bool 引数は upstream paramDef のデフォルトに合わせて
// handler 側で必ず詰めること (例: handler は withReplies=true / withRenotes=true
// / withChannelNotes=false で渡す)。filter struct ではなく bool 引数で受ける
// のは testutil ↔ repository の import cycle を避けるため (#1021)。
// ListPublicNotesForFeed returns the notes shown in a user's RSS/Atom/JSON feed.
//
// upstream FeedService と同じ条件: 自分のノートのうち renote を除き、可視性が
// public / home のものを新しい順に取る。フォロワー限定・ダイレクトは公開
// フィードに出してはいけない。
func (r *noteRepository) ListPublicNotesForFeed(userID string, limit int) ([]*model.Note, error) {
	var notes []*model.Note
	err := preloadNoteRelations(r.db).
		Where(`"userId" = ?`, userID).
		Where(`"renoteId" IS NULL`).
		Where(`visibility IN ?`, []string{string(model.NoteVisibilityPublic), string(model.NoteVisibilityHome)}).
		Order(`id DESC`).
		Limit(limit).
		Find(&notes).Error
	if err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) ListByUserIDFiltered(userID, viewerID, untilID, sinceID string, limit int, withFiles, withReplies, withRenotes, withChannelNotes bool) ([]*model.Note, error) {
	var notes []*model.Note
	q := preloadNoteRelations(r.db).Where(`"userId" = ?`, userID)
	// upstream の generateVisibilityQuery 相当を LIMIT 前に push down する。
	// post-fetch filter だと viewer が見られない note を除外した分 1 ページが
	// limit 未満になりページネーションが途切れる + followers 判定が note ごとの
	// N+1 になるため、LIMIT 前に SQL で絞る。条件は core/note.CanSeeNote と
	// 一致させる (note_reaction.ListByUserID と同じ可視性定義)。
	q = applyViewerVisibility(q, viewerID)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if withFiles {
		// upstream TL filter (`applyTimelineFilter` 同 file 内) と同じ式。
		// 空 array リテラル `'{}'` と比較することで「fileIds 非空」を絞り込める。
		q = q.Where(`"fileIds" != '{}'`)
	}
	if !withReplies {
		// 自己スレッド (自分の投稿への自分の返信) は返信扱いしない。
		//
		// upstream の userTimeline は fanout 側で振り分けており、
		// `isReply(note) = replyId && replyUserId !== userId` が false のもの
		// (= 非返信 + 自己スレッド) を userTimeline に、それ以外を
		// userTimelineWithReplies に積む。つまり withReplies=false でも
		// 自己スレッドは出る。DB fallback もそれに揃える。
		q = q.Where(`("replyId" IS NULL OR "replyUserId" = "note"."userId")`)
	}
	if !withRenotes {
		// pure renote (= text/cw/files/poll/reply 全て空 + renoteId あり) を除外する。
		// quote (= 本文等を伴う renote) は通常の投稿として残す (#1888)。
		q = q.Where(`NOT (` + pureRenoteCondSQL + `)`)
	}
	if !withChannelNotes {
		q = q.Where(`"channelId" IS NULL`)
	} else if viewerID != userID {
		// upstream users/notes.ts: withChannelNotes でも本人以外にはセンシティブ
		// チャンネルの投稿を出さない (`channel.isSensitive = false`)。
		q = q.Where(`("channelId" IS NULL OR EXISTS (
			SELECT 1 FROM "channel" ch WHERE ch.id = "note"."channelId" AND ch."isSensitive" = false
		))`)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListByChannelID returns notes posted to the channel that viewerID can see.
// viewerID="" means anonymous (public/home only). Visibility conditions are
// pushed down before LIMIT to mirror core/note.CanSeeNote and avoid the
// underfilled-page / N+1 follower-check problems of post-fetch filtering
// (#1440).
func (r *noteRepository) ListByChannelID(channelID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return r.listByChannelID(channelID, viewerID, untilID, sinceID, limit, false)
}

// ListLocalByChannelID is ListByChannelID limited to local authors.
func (r *noteRepository) ListLocalByChannelID(channelID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	return r.listByChannelID(channelID, viewerID, untilID, sinceID, limit, true)
}

func (r *noteRepository) listByChannelID(channelID, viewerID, untilID, sinceID string, limit int, localOnly bool) ([]*model.Note, error) {
	var notes []*model.Note
	q := preloadNoteRelations(r.db).Where("\"channelId\" = ?", channelID)
	// チャンネルには remote 利用者の返信も入る (返信先のチャンネルを引き継ぐ) ので、
	// LIMIT の後で落とすとページが過少充填される。LIMIT 前に絞る。
	if localOnly {
		q = q.Where("\"userHost\" IS NULL")
	}
	// core/note.CanSeeNote と同じ可視性条件を LIMIT 前に SQL で絞る。
	// post-fetch filter だとページが過少充填されるのと followers 判定が
	// note ごとの N+1 になるため LIMIT 前に push down する (#1440)。
	q = applyViewerVisibility(q, viewerID)
	// channel 一覧は applyTimelineFilter を通らないので、suspended 除外を個別に
	// 掛ける (#2624)。掛けないと streaming では止まるのに再取得で出るという
	// 逆向きの食い違いになる。
	q = applySuspendedAuthorExclusion(q)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if limit <= 0 {
		limit = 30
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListRenotesOf returns notes whose renoteId equals noteID.
// テキストやファイルを伴わない pure renote だけでなく quote renote も含む。
func (r *noteRepository) ListRenotesOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	var notes []*model.Note
	q := preloadNoteRelations(r.db).Where("\"renoteId\" = ?", noteID)
	// visibility push-down (#1500): viewer が見られる renote のみ LIMIT 前に絞る。
	q = applyViewerVisibility(q, viewerID)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListRenoteOrReplyRemoteUserIDs returns the distinct ids of remote users who
// renoted or replied to noteID (upstream の getRenotedOrRepliedRemoteUsers 相当)。
//
// **可視性では絞らない。** 引きたいのは「この note を実際に取り込んで参照している
// リモート user」で、削除を届ける必要があるかどうかはその一点で決まる。相手が
// 今その note を見られるかは関係がない (見られなくなったからこそ消してほしい)。
//
// **`user` へ join せず `note.userHost` を見る。** 同じ判定を upstream も
// `userHost IS NOT NULL` で書いており、note 側に列があるので join を足す理由が無い。
//
// **件数は絞らない。** 返るのは 1 つの note に対する distinct なリモート user なので
// 実測で問題になる規模にならず、配送側は sharedInbox へ畳むのでインスタンス数まで
// 落ちる。`public` / `home` も broadcast が失敗したときはこの経路へ来る
// (`broadcastDelete` が false を返す) ので、「followers / specified だけ」では
// ない点に注意。
//
// **`renoteId` / `replyId` には index が要る** (000091 / 000092)。無いと削除の
// たびに `note` 全体の seq scan が同期の削除リクエストの中で走る。
func (r *noteRepository) ListRenoteOrReplyRemoteUserIDs(noteID string) ([]string, error) {
	var ids []string
	if err := r.db.Raw(`SELECT DISTINCT "userId" FROM "note"
		WHERE ("renoteId" = ? OR "replyId" = ?) AND "userHost" IS NOT NULL`,
		noteID, noteID).Scan(&ids).Error; err != nil {
		return nil, err
	}
	return ids, nil
}

// ListRepliesOf returns notes whose replyId equals noteID.
// すべてのユーザーからの返信を返す(ミュート判定はServiceで行う)。
func (r *noteRepository) ListRepliesOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	var notes []*model.Note
	q := preloadNoteRelations(r.db).Where("\"replyId\" = ?", noteID)
	// visibility push-down (#1500): viewer が見られる reply のみ LIMIT 前に絞る。
	q = applyViewerVisibility(q, viewerID)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListChildrenOf returns notes that are either replies or quote-renotes of the given noteID.
// notes/childrenでスレッドツリーの直下を取得するために使用する。
func (r *noteRepository) ListChildrenOf(noteID, viewerID, untilID, sinceID string, limit int) ([]*model.Note, error) {
	var notes []*model.Note
	// base 条件の OR は明示的に括弧で囲む。GORM は複数 Where の生 OR 文字列を暗黙に
	// グルーピングするが、その挙動に依存せず「後続 AND の visibility 述語が
	// replyId/renoteId の OR 全体に係る」意図を明示するための defensive clarity
	// (#1500)。実際のリーク防止は ListChildrenOf_VisibilityPushDown の回帰テストで担保。
	//
	// renoteId 側は upstream notes/children (children.ts:53-67) に合わせ、本文・
	// text/cw/file/poll/reply のいずれかを持つ引用 renote のみ含める。pure renote は
	// children に出さない (#1554、quote 判定は #1888 で cw/reply 込みに統一)。
	// reply 側にはこの content ガードを掛けない (返信は本文が空でも子として返す)。
	q := preloadNoteRelations(r.db).
		Where(`("replyId" = ? OR ("renoteId" = ? AND `+renoteHasContentSQL+`))`, noteID, noteID)
	// visibility push-down (#1500): viewer が見られる child のみ LIMIT 前に絞る。
	q = applyViewerVisibility(q, viewerID)
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if err := q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// SearchByFilter returns notes matching the filter criteria.
// 検索バックエンド (core/search.SQLLikeProvider) から呼ばれる。
// When f.Pgroonga is true the predicate uses the PGroonga `&@~` full-text
// operator; otherwise it uses a lower() + LIKE substring match (escaped, upstream sqlLike 相当). Visibility は
// f.ViewerID 視点で push-down する (空は public/home のみ、非空なら viewer 自身の
// followers/specified/visibleUserIds note も含む、#1554)。
func (r *noteRepository) SearchByFilter(f model.NoteSearchFilter) ([]*model.Note, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(f.Query) || !storable(f.UserID) || !storable(f.ChannelID) || !storable(f.Host) {
		return nil, nil
	}
	var notes []*model.Note
	q := preloadNoteRelations(r.db)
	if f.Pgroonga {
		// upstream TS では `note.text &@~ :q` を使用。`&@~` は PGroonga の
		// "クエリ文字列モード" マッチで、空白区切りの AND/OR 等が解釈される。
		q = q.Where("text &@~ ?", f.Query)
	} else {
		// upstream SearchService と同じ形にする (#2514):
		// `LOWER(note.text) LIKE %<sqlLikeEscape(lower(q))>%`。
		//
		// - **ILIKE ではなく lower() + LIKE**。pg_bigm の GIN index
		//   (`gin (lower(text) gin_bigm_ops)`) は LIKE しか加速しないため、
		//   ILIKE だと拡張を入れても index が効かない
		// - **エスケープ必須**。upstream は sqlLikeEscape で `\` `%` `_` を
		//   エスケープする。素通しだと利用者の入力した % / _ がワイルド
		//   カードとして解釈される
		// - クエリ側の小文字化は Go の strings.ToLower (Unicode simple
		//   mapping)。upstream の JS toLowerCase (full mapping) とは
		//   Final_Sigma 等の極端なケースで畳み方が違うが、PG の lower() も
		//   simple mapping なので両側が一致する mk-go の方が取りこぼしが
		//   少ない (mk-go 優位の divergence として維持)
		q = q.Where("lower(text) LIKE ?", "%"+escapeSQLLikePattern(strings.ToLower(f.Query))+"%")
	}
	// visibility push-down: viewer 視点の可視性で絞る (upstream SearchService の
	// generateVisibilityQuery、#1554)。ViewerID 空は匿名 (public/home のみ)、
	// 非空なら viewer 自身の followers/specified/visibleUserIds note も含む。
	// core/note.CanSeeNote と同条件 (mentions / search-by-tag と同じ helper)。
	q = applyViewerVisibility(q, f.ViewerID)
	// upstream searchNoteByLike は userId 優先の排他: userId があれば channelId は
	// 無視する (`if (opts.userId) {} else if (opts.channelId) {}`、SearchService.ts:208-212、
	// #1938)。両指定時に mk-go が両方 AND していた divergence を解消する。
	if f.UserID != "" {
		q = q.Where("\"userId\" = ?", f.UserID)
	} else if f.ChannelID != "" {
		q = q.Where("\"channelId\" = ?", f.ChannelID)
	}
	if f.Host != "" {
		// "." はローカル限定 (userHost が NULL のもの)
		if f.Host == "." {
			q = q.Where("\"userHost\" IS NULL")
		} else {
			q = q.Where("\"userHost\" = ?", f.Host)
		}
	}
	// 未ログインの閲覧者には、サーバーの ugcVisibilityForVisitor=local に従って
	// ローカルの投稿者のノートだけを返す (upstream generateUgcVisibilityQueryForVisitor)。
	// Host 指定とは独立に AND する。
	if f.LocalUsersOnly {
		q = q.Where(`"note"."userHost" IS NULL`)
	}
	if f.UntilID != "" {
		q = q.Where("id < ?", f.UntilID)
	}
	if f.SinceID != "" {
		q = q.Where("id > ?", f.SinceID)
	}
	// 投稿日時範囲 (upstream #16119、#2069)。cursor とは独立に AND する。
	if f.RangeStartID != "" {
		q = q.Where("id > ?", f.RangeStartID)
	}
	if f.RangeEndID != "" {
		q = q.Where("id < ?", f.RangeEndID)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 10
	}
	q = q.Order(paginationOrder(f.SinceID, f.UntilID, "id")).Limit(limit)
	// upstream notes/search.ts:47 の offset (OFFSET-based paging) を適用 (#1783)。
	if f.Offset > 0 {
		q = q.Offset(f.Offset)
	}
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ExistingNoteIDsOnPrimary implements NoteRepository.
//
// `dbresolver.Write` で primary に固定する。dbresolver は既定で SELECT を
// Replicas へ振るので、これを付けないと複製遅延の窓で「行が無い」と誤判定する。
func (r *noteRepository) ExistingNoteIDsOnPrimary(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	var found []string
	if err := r.db.Clauses(dbresolver.Write).
		Model(&model.Note{}).
		Where("id IN ?", ids).
		Pluck("id", &found).Error; err != nil {
		return nil, err
	}
	return found, nil
}

// FindManyByIDsWithUser returns the requested notes preserving the order of `ids`.
// Notes that are not found are simply omitted from the result.
func (r *noteRepository) FindManyByIDsWithUser(ids []string) ([]*model.Note, error) {
	ids = storableIDs(ids)
	return r.findManyByIDsWithUser(ids, false)
}

func (r *noteRepository) findManyByIDsWithUser(ids []string, primary bool) ([]*model.Note, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	// timeline read hot path。GORM の Preload は relation ごとに別クエリを発行する
	// ため preloadNoteRelations は 1 ページ多数往復になる (#1368 計測)。ここでは
	// relation を種類ごとに batch IN で引いて Go 側で配線し、往復を ~5 query
	// (notes / sub-notes / sub-sub-notes / users / polls) に畳む。出力 shape は
	// preload 版と同一 (TestNoteRepository_FindManyByIDsWithUser_HydratesRelations
	// が回帰を捕捉)。
	//
	// renote ブランチは 2 段展開する: 純粋リノート (boost) が引用投稿 (quote) を
	// 包む場合、renote (= quote) のさらに renote/reply (= 引用先) まで配線しないと
	// frontend が引用先を「削除されたノート」として描画する (packer の
	// maxNoteEmbedDepth=2 / detail gate と一致)。reply embed は detail:false で
	// 子を持たないため、reply target の子は辿らない。
	db := func() *gorm.DB {
		if primary {
			return r.db.Clauses(dbresolver.Write)
		}
		return r.db
	}
	var notes []*model.Note
	if err := db().Where("id IN ?", ids).Find(&notes).Error; err != nil {
		return nil, err
	}
	if len(notes) == 0 {
		return nil, nil
	}

	// (2) renote / reply 先 (sub-note) を 1 query で取得し配線する。
	subIDSet := make(map[string]struct{})
	for _, n := range notes {
		if n.RenoteID != nil && *n.RenoteID != "" {
			subIDSet[*n.RenoteID] = struct{}{}
		}
		if n.ReplyID != nil && *n.ReplyID != "" {
			subIDSet[*n.ReplyID] = struct{}{}
		}
	}
	subByID := make(map[string]*model.Note, len(subIDSet))
	if len(subIDSet) > 0 {
		var subs []*model.Note
		if err := db().Where("id IN ?", mapKeys(subIDSet)).Find(&subs).Error; err != nil {
			return nil, err
		}
		for _, s := range subs {
			subByID[s.ID] = s
		}
		for _, n := range notes {
			if n.RenoteID != nil {
				n.Renote = subByID[*n.RenoteID]
			}
			if n.ReplyID != nil {
				n.Reply = subByID[*n.ReplyID]
			}
		}
	}

	// (2.5) 2 段目: 1 段目 renote target (= quote 候補) の renote (= 引用先) と
	// reply (= 返信先) をさらに 1 query で取得し配線する。pure renote → quote →
	// 引用先 の 3 段目を埋めるため。packer は renote を detail 付きで pack する
	// ので renote.renote と renote.reply が depth-2 に出る (reply.* は出さない)。
	var renoteTargetSubs []*model.Note
	for _, n := range notes {
		if n.RenoteID != nil {
			if s := subByID[*n.RenoteID]; s != nil {
				renoteTargetSubs = append(renoteTargetSubs, s)
			}
		}
	}
	lvl2IDSet := make(map[string]struct{})
	for _, s := range renoteTargetSubs {
		for _, id := range []*string{s.RenoteID, s.ReplyID} {
			if id == nil {
				continue
			}
			if _, ok := subByID[*id]; !ok {
				lvl2IDSet[*id] = struct{}{}
			}
		}
	}
	if len(lvl2IDSet) > 0 {
		var lvl2 []*model.Note
		if err := db().Where("id IN ?", mapKeys(lvl2IDSet)).Find(&lvl2).Error; err != nil {
			return nil, err
		}
		for _, s := range lvl2 {
			subByID[s.ID] = s
		}
	}
	// 1 段目 renote target に 2 段目 renote を配線する。配線は lvl2IDSet が空でも
	// 必ず実行する: 同ページの別ノートが既に depth-2 ターゲットを depth-1 sub として
	// 読み込んでいる (aliasing) と lvl2IDSet が空になるが、その場合でも subByID には
	// ターゲットが入っているので配線が必要 (これを skip すると quote-of-quote が
	// 「削除されたノート」表示で再発する)。
	for _, s := range renoteTargetSubs {
		if s.RenoteID != nil {
			s.Renote = subByID[*s.RenoteID]
		}
		if s.ReplyID != nil {
			s.Reply = subByID[*s.ReplyID]
		}
	}

	// notes + sub-notes をまとめて user / poll の配線対象にする。
	hydrate := make([]*model.Note, 0, len(notes)+len(subByID))
	hydrate = append(hydrate, notes...)
	for _, s := range subByID {
		hydrate = append(hydrate, s)
	}

	// (3) author user を 1 query で取得し配線する。
	userIDSet := make(map[string]struct{}, len(hydrate))
	for _, n := range hydrate {
		if n.UserID != "" {
			userIDSet[n.UserID] = struct{}{}
		}
	}
	if len(userIDSet) > 0 {
		var users []*model.User
		if err := db().Where("id IN ?", mapKeys(userIDSet)).Find(&users).Error; err != nil {
			return nil, err
		}
		userByID := make(map[string]*model.User, len(users))
		for _, u := range users {
			userByID[u.ID] = u
		}
		for _, n := range hydrate {
			n.User = userByID[n.UserID]
		}
	}

	// (4) poll を 1 query で取得し配線する。preload 版と同じく全 note を対象にして
	// HasPoll の正確性に依存しない (poll 行が在れば attach、無ければ nil)。
	noteIDs := make([]string, 0, len(hydrate))
	for _, n := range hydrate {
		noteIDs = append(noteIDs, n.ID)
	}
	var polls []*model.Poll
	if err := db().Where(`"noteId" IN ?`, noteIDs).Find(&polls).Error; err != nil {
		return nil, err
	}
	if len(polls) > 0 {
		pollByNote := make(map[string]*model.Poll, len(polls))
		for _, p := range polls {
			pollByNote[p.NoteID] = p
		}
		for _, n := range hydrate {
			if p, ok := pollByNote[n.ID]; ok {
				n.Poll = p
			}
		}
	}

	// (5) ids の順序を保って返す。
	byID := make(map[string]*model.Note, len(notes))
	for _, n := range notes {
		byID[n.ID] = n
	}
	ordered := make([]*model.Note, 0, len(ids))
	for _, id := range ids {
		if n, ok := byID[id]; ok {
			ordered = append(ordered, n)
		}
	}
	return ordered, nil
}

func (r *noteRepository) FindManyByIDsWithUserOnPrimary(ids []string) ([]*model.Note, error) {
	ids = storableIDs(ids)
	return r.findManyByIDsWithUser(ids, true)
}

// mapKeys returns the keys of a string-set as a slice (順序不定)。
func mapKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func (r *noteRepository) ListFeatured(channelID, untilID string, limit, offset int) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	q := preloadNoteRelations(r.db).Where("visibility = 'public'")
	if channelID != "" {
		// チャンネル指定時は当該チャンネル内のノートだけを返す
		// (upstream FeaturedService.getInChannelNotesRanking と一致)。
		// 過去はこの WHERE 節が無く、グローバル featured が混ざってチャンネル
		// ハイライトに無関係ノートが出ていた (#489)。
		q = q.Where(`"channelId" = ?`, channelID)
	}
	if untilID != "" {
		q = q.Where(`id < ?`, untilID)
	}
	q = q.Order(`("renoteCount" + "repliesCount") DESC, id DESC`).Limit(limit)
	// cursor 指定時は offset を無視 (upstream makePaginationQuery と一致)
	if untilID == "" && offset > 0 {
		q = q.Offset(offset)
	}
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) ListFeaturedByUser(userID, viewerID, untilID string, limit int) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	// === selection: engagement DESC top-N を SQL push-down で選抜 ===
	// upstream FeaturedService.getPerUserNotesRanking が Redis sorted set
	// 上位 50 件を引いてくる工程に相当。visibility 条件と channel 除外も
	// LIMIT 前に同じ SQL で適用する (#1487 / #1491, ListByUserIDFiltered と
	// 同 pattern)。post-fetch FilterVisible だと limit が削られページ過少充填、
	// followers 判定が N+1。untilID はここでは適用しない (engagement プール
	// を id DESC でページングする display 段で適用する)。
	q := preloadNoteRelations(r.db).
		Where(`"userId" = ?`, userID).
		Where(`"channelId" IS NULL`)
	q = applyViewerVisibility(q, viewerID)
	// **凍結した利用者のノートを出さない。** upstream
	// `users/featured-notes.ts` は `generateSuspendedUserQueryForNote` を
	// 掛けている。`users/show` が凍結ユーザーを `NO_SUCH_USER` で隠すのに
	// 同じ利用者のノートが本文つきで返るのは、経路ごとに結果が食い違う形。
	q = applySuspendedAuthorExclusion(q)
	q = q.Order(`("renoteCount" + "repliesCount") DESC, id DESC`).Limit(FeaturedNotesPerUserPoolSize)
	var pool []*model.Note
	if err := q.Find(&pool).Error; err != nil {
		return nil, err
	}
	// === display: id DESC sort → untilID filter → 先頭 limit 件 ===
	// upstream featured-notes.ts:100-104 と同じ手順。engagement プールを
	// id DESC で見せると id cursor とソート順が一致してページングが整合的に
	// なる (engagement DESC で表示すると untilId 進行時に重複/欠落するため)。
	sort.Slice(pool, func(i, j int) bool {
		return pool[i].ID > pool[j].ID
	})
	if untilID != "" {
		filtered := pool[:0]
		for _, n := range pool {
			if n.ID < untilID {
				filtered = append(filtered, n)
			}
		}
		pool = filtered
	}
	if len(pool) > limit {
		pool = pool[:limit]
	}
	return pool, nil
}

// ListRenotesByUser returns all notes authored by userID that renote renoteID.
func (r *noteRepository) ListRenotesByUser(userID, renoteID string) ([]*model.Note, error) {
	if !storable(userID) || !storable(renoteID) {
		return []*model.Note{}, nil
	}
	var notes []*model.Note
	if err := r.db.Where("\"userId\" = ? AND \"renoteId\" = ?", userID, renoteID).
		Order("id ASC").Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) ListMentions(userID, visibility string, following bool, limit int, sinceID, untilID string) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	// match 条件: TS notes/mentions と同じく mentions または visibleUserIds の
	// どちらかに viewer が含まれれば対象とする (TS: `:meId <@ note.mentions OR
	// :meId <@ note.visibleUserIds`)。本文に @mention の無い specified DM は
	// visibleUserIds にしか入らないため (= 宛先指定だけの DM)、mentions 単独 match
	// だと受信者の notes/mentions / Direct タブで取りこぼす (#1484)。OR は単一 note
	// 行の boolean なので重複行は出ない。
	//
	// 両枝とも containment 演算子 `@>` で書く: `scalar = ANY(array)` は GIN を使えず、
	// `array @> ARRAY[scalar]` だけが GIN index 対象になる。viewer は単一要素なので
	// 論理的に等価で、将来 visibleUserIds に GIN index を足せば mentions 枝
	// (IDX_note_mentions, #1427) と揃って planner が BitmapOr で両枝 index 利用できる
	// (#1484 review)。
	q := preloadNoteRelations(r.db).
		Where(`(mentions @> ARRAY[?]::varchar[] OR "visibleUserIds" @> ARRAY[?]::varchar[])`, userID, userID)
	// visibility push-down: mention 対象 (= viewer = userID) が CanSeeNote で
	// 見られる note のみ返す。mentions 配列は本文の @user パース結果で specified
	// の visibleUserIds とは独立なので、これが無いと followers/specified note を
	// mention されただけの非対象 viewer が本文と author を取得できてしまう (#1441)。
	// viewer は notes/mentions の認証ユーザー = userID 固定。
	// notes/mentions は RequireAuth 配下で userID 非空のため、helper の認証分岐
	// (= 4 分岐 CanSeeNote) がそのまま適用される。
	q = applyViewerVisibility(q, userID)
	// visibility kind filter: upstream TS notes/mentions と同じく visibility 指定
	// 時のみ note.visibility = <値> の exact-match で絞る (空は全種別)。これを
	// LIMIT 前に SQL で行うことで、handler 側 post-fetch 振り分けで起きていた
	// ページ under-fill を解消する (#1451)。値は parameterized で injection-safe、
	// 未知の値は単に 0 件になる (TS と同じ挙動)。
	if visibility != "" {
		q = q.Where(`"visibility" = ?`, visibility)
	}
	// following=true: note.userId が viewer の followee または viewer 自身に限定
	// (upstream mentions.ts:84-87)。subquery は ListHomeTimeline と同じ following
	// テーブル参照。
	if following {
		q = q.Where(`("userId" IN (SELECT "followeeId" FROM "following" WHERE "followerId" = ?) OR "userId" = ?)`, userID, userID)
	}
	// upstream mentions.ts:69 は makePaginationQuery を使わず、sinceId/sinceDate が
	// あれば untilId の有無に関わらず ASC、無ければ DESC で order する。汎用
	// paginationOrder (ASC は sinceID && !untilID のみ) と異なるので mentions 専用
	// の order に揃える (#1948-18)。
	mentionsOrder := "id DESC"
	if sinceID != "" {
		mentionsOrder = "id ASC"
	}
	q = q.Order(mentionsOrder).Limit(limit)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) SearchByTag(tagGroups [][]string, viewerID string, limit int, sinceID, untilID string, filter model.NoteSearchTagFilter) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	// tagGroups は upstream search-by-tag.ts の query (string[][]) と同じ
	// 外側 OR・内側 AND の複合タグ検索 (#1683)。各 group は AND (= note.tags が
	// group の全 tag を含む = `tags @> ARRAY[inner]`)、group 同士は OR。単一タグ
	// 検索は [][]string{{tag}} を渡せばよい。空 tag は除外し、有効な group が
	// 無ければ空結果を返す。
	var conds []string
	var args []any
	for _, g := range tagGroups {
		clean := make([]string, 0, len(g))
		for _, t := range g {
			// upstream search-by-tag.ts:106,113 は query tag を normalizeForSearch
			// (NFKC+lowercase) してから `:tag <@ note.tags` で突合する。note.tags も
			// 同 normalize で格納されるので、query も正規化して一致させる (#1948-18)。
			if n := searchnorm.Normalize(t); n != "" {
				clean = append(clean, n)
			}
		}
		if len(clean) == 0 {
			continue
		}
		ph := make([]string, len(clean))
		for i := range clean {
			ph[i] = "?"
		}
		conds = append(conds, "tags @> ARRAY["+strings.Join(ph, ",")+"]::varchar[]")
		for _, t := range clean {
			args = append(args, t)
		}
	}
	if len(conds) == 0 {
		return []*model.Note{}, nil
	}
	q := preloadNoteRelations(r.db).
		Where("("+strings.Join(conds, " OR ")+")", args...)
	// visibility push-down: core/note.CanSeeNote と同じ条件を LIMIT 前に絞る。
	// ListByUserIDFiltered / clip の ListByClipVisible と同じ可視性定義 (#1439)。
	q = applyViewerVisibility(q, viewerID)
	// hashtag 一覧も applyTimelineFilter を通らないので suspended 除外を個別に
	// 掛ける (#2624)。channel 一覧 (ListByChannelID) と同じ理由。
	q = applySuspendedAuthorExclusion(q)
	// reply/renote/poll/withFiles filter (upstream search-by-tag.ts:124-150、#1554)。
	if filter.Reply != nil {
		if *filter.Reply {
			q = q.Where(`"replyId" IS NOT NULL`)
		} else {
			q = q.Where(`"replyId" IS NULL`)
		}
	}
	if filter.Renote != nil {
		if *filter.Renote {
			q = q.Where(`"renoteId" IS NOT NULL`)
		} else {
			q = q.Where(`"renoteId" IS NULL`)
		}
	}
	if filter.Poll != nil {
		q = q.Where(`"hasPoll" = ?`, *filter.Poll)
	}
	if filter.WithFiles {
		q = q.Where(`"fileIds" != '{}'`)
	}
	// 未ログインの閲覧者向けの ugcVisibilityForVisitor=local (upstream
	// search-by-tag.ts の generateUgcVisibilityQueryForVisitor)。LIMIT の前に
	// 絞らないと、ページがリモートのノートで埋まって件数が欠ける。
	if filter.LocalUsersOnly {
		q = q.Where(`"note"."userHost" IS NULL`)
	}
	q = q.Order(paginationOrder(sinceID, untilID, "id")).Limit(limit)
	if sinceID != "" {
		q = q.Where("id > ?", sinceID)
	}
	if untilID != "" {
		q = q.Where("id < ?", untilID)
	}
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) ListByFileID(fileID, sinceID, untilID string, limit int) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	q := preloadNoteRelations(r.db).Where(`"fileIds" @> ARRAY[?]::varchar[]`, fileID)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	var notes []*model.Note
	if err := q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

func (r *noteRepository) IncrementUserNotesCount(userID string, delta int) error {
	return r.db.Exec(`UPDATE "user" SET "notesCount" = "notesCount" + ? WHERE "id" = ?`, delta, userID).Error
}

// applyTimelineFilter adds common filter conditions to a GORM query builder.
func applyTimelineFilter(q *gorm.DB, f model.TimelineDBFilter) *gorm.DB {
	if f.LocalUsersOnly {
		// 未ログインの閲覧者向けの ugcVisibilityForVisitor=local。ノート自身の
		// 投稿者だけを見る (返信先・リノート先は upstream も残す)。
		q = q.Where(`"note"."userHost" IS NULL`)
	}
	if f.WithFiles {
		q = q.Where(`"fileIds" != '{}'`)
	}
	if f.WithRenotes != nil && !*f.WithRenotes {
		// pure renote (text/cw/files/poll/reply 全て空の renote) を除外 (#1888)。
		q = q.Where(`NOT (` + pureRenoteCondSQL + `)`)
	}
	if f.ExcludeRepliesToOthers || (f.WithReplies != nil && !*f.WithReplies) {
		// upstream Misskey TS Home TL と完全一致: `replyId IS NULL` か、
		// reply の場合は self-thread (= replyUserId = note.userId) のみ残す
		// (#1047)。
		//
		// 自分が他人にした reply (= userId=self / replyUserId=other) はこの
		// DB fallback では除外される。ただし production の cache hit 経路では
		// fanout (= OnNoteCreated) が自分の HomeTL stream に push しているので
		// 通常運用は自分の reply も TL に表示される (= Redis 経路は pass-through)。
		// DB fallback は cache miss / 古い note の case のみ走るので user 体験
		// への影響は限定的、その時点で upstream と完全互換に倒す。
		//
		// 「他人 → 他人 reply」を per-followee で表示するかは fanout 層で
		// `following.withReplies` を見て push 制御する (= upstream 互換)。
		q = q.Where(`("replyId" IS NULL OR "replyUserId" = "note"."userId")`)
	}
	if f.IncludeMyRenotes != nil && !*f.IncludeMyRenotes && f.ViewerID != "" {
		// "userId" は JOIN を持つ呼び出し元 (ListByUserList の user_list_membership)
		// で membership.userId と衝突するため "note". で修飾する (#1498)。
		q = q.Where(`NOT (`+pureRenoteCondSQL+` AND "note"."userId" = ?)`, f.ViewerID)
	}
	if f.IncludeRenotedMyNotes != nil && !*f.IncludeRenotedMyNotes && f.ViewerID != "" {
		q = q.Where(`NOT (`+pureRenoteCondSQL+` AND "renoteUserId" = ?)`, f.ViewerID)
	}
	if f.IncludeLocalRenotes != nil && !*f.IncludeLocalRenotes {
		q = q.Where(`NOT (` + pureRenoteCondSQL + ` AND "renoteUserHost" IS NULL)`)
	}
	if f.HideFollowersOnlyReplyFromNonFollowee && f.ViewerID != "" {
		// 返信先が followers 限定なら、その投稿者を viewer がフォローしている
		// (または viewer 自身の投稿) 場合だけ残す。
		q = q.Where(`("replyId" IS NULL OR "replyUserId" IS NULL OR NOT EXISTS (
			SELECT 1 FROM "note" rp WHERE rp.id = "note"."replyId" AND rp.visibility = 'followers'
		) OR "replyUserId" = ? OR EXISTS (
			SELECT 1 FROM "following" fw WHERE fw."followerId" = ? AND fw."followeeId" = "note"."replyUserId"
		))`, f.ViewerID, f.ViewerID)
	}
	if len(f.MutedChannelIDs) > 0 {
		// channel_mutingに登録されたチャンネルのノートはタイムラインから除外する。
		// GORMは連続.WhereをANDで繋ぐがraw SQL側のORはデフォルトでは
		// 括弧で囲まれないので、明示的に囲まないとAND側の他フィルタを
		// バイパスしてしまう (SQL優先順位: AND > OR)。
		q = q.Where(`("channelId" IS NULL OR "channelId" NOT IN ?)`, f.MutedChannelIDs)
		// ミュートしたチャンネルのノートを「チャンネル外で」リノートされた場合、
		// リノート自身の channelId は NULL なので上の条件では抜けられない。
		// upstream は同じ理由で非正規化カラム note.renoteChannelId を用意し、
		// リノート先のチャンネルでも弾いている (users/notes.ts の
		// 「ミュートされたチャンネルのリノート対策」)。
		q = q.Where(`("renoteChannelId" IS NULL OR "renoteChannelId" NOT IN ?)`, f.MutedChannelIDs)
	}
	// muted user filter は 2 経路を持つ (#892 / #894):
	//   1. UseMutingSubquery=true: muting テーブルへの NOT EXISTS (production)。
	//      bind parameter が viewer 単位で 2 つ ($viewerID を 2 度) のみで、
	//      mute 件数に比例しないので heavy-mute viewer でも planning コスト
	//      が膨らまない。
	//   2. MutedUserIDs literal: 既存テスト互換のための override path
	//      (test や非 viewer 駆動経路で muteeID 集合を直接指定したいとき)。
	//      Subquery が使えるなら 1. を優先。
	// 共通の WHERE は: userId が active mute されておらず、かつ renoteUserId
	// が NULL でなければ renoteUserId も active mute されていないこと。
	if f.UseMutingSubquery && f.ViewerID != "" {
		// NOT EXISTS による anti-join (Postgres planner が semi-join 相当に
		// 最適化、IDX_muting_muterId_muteeId 複合 index で seek)。NOT IN
		// subquery より NULL 取り扱いがロバストかつ index 使用が明確。
		// active mute = expiresAt nil or future、mutingRepository.ListMuteeIDs
		// と同 semantics を SQL 内で表現。
		//
		// Postgres NOW() は transaction 開始時刻 (= query 内で constant)。
		// in-memory ApplyFilter (Redis cache 経路) は Go time.Now() を使う
		// が、mutingRepo.ListMuteeIDs と timeline 取得は同一 request 内 (=
		// サブ秒) で連続実行されるため、境界 expiresAt の片方に倒れて 2
		// 経路で挙動が分岐する race window は無視できる。
		notExistsForCol := func(col string) string {
			return `NOT EXISTS (SELECT 1 FROM "muting" m WHERE m."muterId" = ? AND m."muteeId" = ` + col + ` AND (m."expiresAt" IS NULL OR m."expiresAt" > NOW()))`
		}
		// note/renote/reply のいずれかの author が active mute なら除外
		// (upstream generateMutedUserQueryForNotes は 3 author すべて見る、#1681)。
		// reply著者mute は #1681 で追加 (旧実装は userId + renoteUserId のみ)。
		q = q.Where(
			notExistsForCol(`"note"."userId"`)+
				` AND ("renoteUserId" IS NULL OR `+notExistsForCol(`"note"."renoteUserId"`)+`)`+
				` AND ("replyUserId" IS NULL OR `+notExistsForCol(`"note"."replyUserId"`)+`)`,
			f.ViewerID, f.ViewerID, f.ViewerID,
		)
	} else if len(f.MutedUserIDs) > 0 {
		// "userId" は JOIN 併用時の曖昧性回避のため "note". で修飾 (#1498)。
		// reply著者も check (#1681、upstream parity)。
		q = q.Where(
			`"note"."userId" NOT IN ? AND ("renoteUserId" IS NULL OR "renoteUserId" NOT IN ?) AND ("replyUserId" IS NULL OR "replyUserId" NOT IN ?)`,
			f.MutedUserIDs, f.MutedUserIDs, f.MutedUserIDs,
		)
	}
	// blocked-by filter (#1681): viewer を block している user が note/reply/renote
	// のいずれかの author なら除外 (upstream generateBlockedUserQueryForNotes)。
	if len(f.BlockerIDs) > 0 {
		q = q.Where(
			`"note"."userId" NOT IN ? AND ("replyUserId" IS NULL OR "replyUserId" NOT IN ?) AND ("renoteUserId" IS NULL OR "renoteUserId" NOT IN ?)`,
			f.BlockerIDs, f.BlockerIDs, f.BlockerIDs,
		)
	}
	// instance-mute filter (#1681): viewer が mute した instance の note/reply/
	// renote author を除外 (upstream generateMutedUserQueryForNotes の
	// mutedInstances 分岐)。local user (host NULL) は常に通す。host は lowercase 前提。
	if len(f.MutedInstances) > 0 {
		// host は LOWER() で突合する。Redis path (hostInSet) が note host を
		// ToLower するため、SQL も case-insensitive にして 2 経路の結果を一致
		// させる (#1681 review)。MutedInstances は loader で lowercase 済。
		q = q.Where(
			`("note"."userHost" IS NULL OR LOWER("userHost") NOT IN ?) AND ("replyUserHost" IS NULL OR LOWER("replyUserHost") NOT IN ?) AND ("renoteUserHost" IS NULL OR LOWER("renoteUserHost") NOT IN ?)`,
			f.MutedInstances, f.MutedInstances, f.MutedInstances,
		)
	}
	// renote-mute filter (#903): 投稿者が renote-muted で **かつ** pure
	// renote の note のみ除外する。投稿者の plain note / quote renote は
	// そのまま通す。upstream Misskey TS の
	// generateMutedUserRelatedRenotesQuery と同 semantics。
	//
	// SQL は subquery 経路のみ実装 (UseRenoteMutingSubquery=true 必須):
	// renote_muting への NOT EXISTS で bind parameter を viewer 単位に固定
	// (#894 user-mute subquery と同 pattern)。user-mute のような literal
	// path (RenoteMutedUserIDs を直接 IN) は現状 test/production とも未使用
	// なので未実装。必要になったら同 file の MutedUserIDs literal 分岐と
	// 同 pattern で追加可能。
	// pure renote condition は pureRenoteCondSQL (text/cw/files/poll/reply 全空) を共有 (#1888)。
	if f.UseRenoteMutingSubquery && f.ViewerID != "" {
		q = q.Where(
			`NOT (EXISTS (SELECT 1 FROM "renote_muting" rm WHERE rm."muterId" = ? AND rm."muteeId" = "note"."userId") AND `+pureRenoteCondSQL+`)`,
			f.ViewerID,
		)
	}
	// suspended-user filter (#1686): note/reply/renote のいずれかの author が
	// suspended なら除外する (upstream generateSuspendedUserQueryForNote)。
	// timeline は viewer に依らず常に suspended user の note を隠すので無条件
	// 適用。isSuspended は user table への NOT EXISTS で確認し bind parameter を
	// 持たない。suspended user は post-suspension で fanout list に note ID が
	// 残るため、Redis 経路の ApplyFilter にも対応する suspended check がある。
	q = applySuspendedAuthorExclusion(q)
	return q
}

// applySuspendedAuthorExclusion excludes notes whose author, reply target, or
// renote target belongs to a suspended user (upstream
// generateSuspendedUserQueryForNote)。viewer に依らず常に適用する。
//
// **timeline 以外の読み取り経路からも呼ぶ (#2624)。** channel / hashtag の
// 一覧は applyTimelineFilter を通らないため、以前はここだけ凍結ユーザーの
// note が出ていた。streaming 側は publish 時に同じ 3 author を見て止めるので
// (internal/stream の suspended-author gate)、経路ごとに結果が食い違わない
// ようにここへ集約する。
func applySuspendedAuthorExclusion(q *gorm.DB) *gorm.DB {
	notExists := func(col string) string {
		return `NOT EXISTS (SELECT 1 FROM "user" su WHERE su."id" = ` + col + ` AND su."isSuspended" = true)`
	}
	return q.Where(
		notExists(`"note"."userId"`) +
			` AND ("replyUserId" IS NULL OR ` + notExists(`"note"."replyUserId"`) + `)` +
			` AND ("renoteUserId" IS NULL OR ` + notExists(`"note"."renoteUserId"`) + `)`,
	)
}

// ListHomeTimeline returns notes by the user and users they follow.
// DBフォールバック用。Redisが空のときに使う。
//
// channel note の扱い (#1686, upstream timeline.ts の followingChannelIds 分岐):
//   - followed user の note は `channelId IS NULL` のものだけ home に出す
//     (= followed user が channel に投稿しても、その channel を follow して
//     いなければ home には出さない)。
//   - filter.FollowedChannelIDs が非空なら、follow 中 channel の note を
//     `channelId IN (...)` で追加で含める (mute 済 channel は handler 側で
//     除外済)。
func (r *noteRepository) ListHomeTimeline(userID string, limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error) {
	q := preloadNoteRelations(r.db)
	if len(filter.FollowedChannelIDs) > 0 {
		q = q.Where(
			`(("userId" = ? OR "userId" IN (SELECT "followeeId" FROM "following" WHERE "followerId" = ?)) AND "channelId" IS NULL OR "channelId" IN ?)`,
			userID, userID, filter.FollowedChannelIDs,
		)
	} else {
		q = q.Where(
			`("userId" = ? OR "userId" IN (SELECT "followeeId" FROM "following" WHERE "followerId" = ?)) AND "channelId" IS NULL`,
			userID, userID,
		)
	}
	// #2106 L54: 旧 `visibility IN ('public','home','followers')` リテラルを applyViewerVisibility に
	// 置換し、followee が viewer 宛てに送った specified DM (visibleUserIds に viewer) も home TL の
	// DB fallback に surface させる (upstream timeline.ts の generateVisibilityQuery 互換)。read 経路は
	// N27 で upstream 化済で、main-stream realtime push の #1472 strict gate とは独立。
	q = applyViewerVisibility(q, userID)
	q = q.Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	q = applyTimelineFilter(q, filter)
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListLocalTimeline returns public/home notes by local users.
func (r *noteRepository) ListLocalTimeline(limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error) {
	q := preloadNoteRelations(r.db).
		Where(`"userHost" IS NULL AND "visibility" = 'public' AND "channelId" IS NULL`).
		Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	q = applyTimelineFilter(q, filter)
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListPublicNotes implements the upstream notes.ts query: public, non-localOnly
// notes time-ordered with optional local/reply/renote/withFiles/poll filters.
// Unlike ListGlobalTimeline this does NOT exclude channel notes (upstream notes.ts
// has no channelId filter) (#2106 L4 / #2215).
func (r *noteRepository) ListPublicNotes(filter model.PublicNotesFilter, limit int, sinceID, untilID string) ([]*model.Note, error) {
	q := preloadNoteRelations(r.db).
		Where(`"visibility" = 'public' AND "localOnly" = FALSE`).
		Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	if filter.Local {
		q = q.Where(`"userHost" IS NULL`)
	}
	if filter.Reply != nil {
		if *filter.Reply {
			q = q.Where(`"replyId" IS NOT NULL`)
		} else {
			q = q.Where(`"replyId" IS NULL`)
		}
	}
	if filter.Renote != nil {
		if *filter.Renote {
			q = q.Where(`"renoteId" IS NOT NULL`)
		} else {
			q = q.Where(`"renoteId" IS NULL`)
		}
	}
	if filter.WithFiles != nil {
		if *filter.WithFiles {
			q = q.Where(`"fileIds" != '{}'`)
		} else {
			q = q.Where(`"fileIds" = '{}'`)
		}
	}
	if filter.Poll != nil {
		if *filter.Poll {
			q = q.Where(`"hasPoll" = TRUE`)
		} else {
			q = q.Where(`"hasPoll" = FALSE`)
		}
	}
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// ListGlobalTimeline returns all public notes.
// channelId IS NULL でチャンネルノートを除外する (TS互換)。
func (r *noteRepository) ListGlobalTimeline(limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error) {
	q := preloadNoteRelations(r.db).
		Where(`"visibility" = 'public' AND "channelId" IS NULL`).
		Order(paginationOrder(sinceID, untilID, `"id"`)).Limit(limit)
	if sinceID != "" {
		q = q.Where(`"id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"id" < ?`, untilID)
	}
	q = applyTimelineFilter(q, filter)
	var notes []*model.Note
	if err := q.Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// noteRemovableExpr is the per-note removal criteria shared by the root
// selection and the inductive step of DeleteExpiredRemoteNotes.
//
// **両方で同じ式を使うこと。** 片方だけに掛けると、根に掛ければ「新しい子孫
// ごと消える」、枝に掛ければ「保護された根が LIMIT の枠を食う」。upstream も
// `removalCriteria` を 1 つ組んで両方で使う。`?` を 1 つ含むので、
// 呼び出し側は cutoff を式の出現回数ぶん渡すこと。
const noteRemovableExpr = `
			n."userHost" IS NOT NULL
			AND n.id < ?
			AND n."clippedCount" = 0
			AND n."pageCount" = 0
			AND NOT EXISTS (SELECT 1 FROM "clip_note" c WHERE c."noteId" = n.id)
			AND NOT EXISTS (SELECT 1 FROM "user_note_pining" p WHERE p."noteId" = n.id)
			AND NOT EXISTS (SELECT 1 FROM "note_favorite" f WHERE f."noteId" = n.id)
			AND NOT EXISTS (
				SELECT 1 FROM "note_reaction" rc
				INNER JOIN "user" u ON u.id = rc."userId"
				WHERE rc."noteId" = n.id AND u.host IS NULL)`

// DeleteExpiredRemoteNotes はリモートノート (userHost IS NOT NULL) のうち
// expiryDays より前に作成されたものを最大 batchSize 件削除し、実際に消えた
// 行数を返す。ループは呼び出し側 (CleanRemoteNotesProcessor) が sleep / ctx
// cancellation 付きで回す。
// ON DELETE CASCADE が reactions / replies 等に効く前提。
// mk-go の note テーブルには createdAt カラムが無い (aidx ID から時刻を
// 導出する設計) ため、ID 文字列の lexicographic 比較で時刻切り捨てを行う。
//
// 保持条件は upstream CleanRemoteNotesProcessorService の removalCriteria に
// 対応する (#2329)。ローカルユーザーが手を加えたリモートノートは、期限を過ぎても
// 消さない。これが無いと、クリップに入れた・ピン留めした・お気に入りにした投稿が
// 本人の操作と無関係に消える。
//
// `clip_note` を直接見るのは mk-go 固有の追加。upstream は非正規化カウンタの
// `clippedCount = 0` だけで判定する。mk-go も #1768 以降は clips/add-note と
// remove-note で `clippedCount` を増減しているが、それより前に mk-go で
// クリップした行はカウンタが 0 のまま残っているので、カウンタだけを見ると
// そうしたクリップを保護できない。`clippedCount` / `pageCount` の比較は
// カウンタが入っている行 (TS 由来や #1768 以降の行) のために併せて見る。
// `pageCount` は #3293 からページの作成・更新・削除で増減している。それより前に
// 作ったページが参照する行は migration 000107 で埋めた。
func (r *noteRepository) DeleteExpiredRemoteNotes(expiryDays, batchSize int) (int64, error) {
	deleted, _, _, err := r.DeleteExpiredRemoteNotesAfter(expiryDays, batchSize, "")
	return deleted, err
}

// DeleteExpiredRemoteNotesAfter is the cursor-based form of
// DeleteExpiredRemoteNotes.
//
// **根を id 順に、カーソルの後ろから見る (upstream 2026.9.1 #17957)。**
// 以前は `ORDER BY` も位置も無い `LIMIT` だけで根を選んでいたので、配下に
// 削除不可のノート (ローカルの返信など) を持つツリーの根が毎回同じ枠を食い、
// そうした根が batchSize 件以上あると掃除が一歩も進まなかった。カーソルを
// 進めれば、消せないツリーは 1 度見たら通り過ぎる。
//
// cutoff 以上のカーソル (期限を延ばした後など) は先頭からにする (upstream と同じ)。
func (r *noteRepository) DeleteExpiredRemoteNotesAfter(expiryDays, batchSize int, after string) (int64, int, string, error) {
	if batchSize <= 0 {
		batchSize = 100
	}
	cutoffID := aidxCutoffID(time.Now().Add(-time.Duration(expiryDays) * 24 * time.Hour))
	if after >= cutoffID {
		after = ""
	}
	// **ツリー単位で守る。**
	//
	// 単体の条件だけで消すと、**ローカル利用者が返信・引用・リノートした
	// リモートノートが、その利用者の投稿を残したまま消える**。返信先の無い
	// 返信や「削除されたノート」のリノートがタイムラインに残り、しかも
	// 不可逆 (相手サーバーから取り直す経路は無い)。
	//
	// upstream `CleanRemoteNotesProcessorService` と同じ 3 段にする —
	// (a) 起点をルート (`replyId IS NULL AND renoteId IS NULL`) に絞り、
	// (b) 再帰 CTE で `replyId` / `renoteId` を辿ってツリー全体を作り、
	// (c) 1 件でも削除不可のノートを含むツリーは丸ごと除外する。
	//
	// **`UNION` であって `UNION ALL` ではない。** `replyId` と `renoteId` が
	// 同じツリーの別ノートを指す形 (= 引用リプライ。Misskey の通常機能) だと
	// 1 ノードが 2 行から派生するので、`UNION ALL` では **Fibonacci 的に増殖**
	// する。実測では長さ 26 の連鎖で `tree` が 317,810 行 (0.90 秒)、31 で
	// 2 分 38 秒を超えた。連合相手が投稿を積むだけで誘発できる。`UNION` が
	// 重複排除と循環ガードを兼ねる (upstream も `UNION`)。
	//
	// **期限はノード単位でも見る。** cutoff を `roots` にしか掛けないと、
	// **たった今届いた返信がぶら下がっているだけのツリーが丸ごと消える**。
	// upstream は `removalCriteria` に `note."id" < :newestLimit` を含め、
	// それを帰納ステップの判定にも使う。
	//
	// **`roots` にも removability を掛ける。** 掛けないと削除不可のルートが
	// LIMIT の枠を食い、`deleted < batchSize` で break する processor が
	// 前へ進めなくなる (実測: 保護されたルートが 1 件あるだけで掃除が止まった)。
	//
	// **保護そのものは (c) の anti-join が担う。** (a) の「起点をルートに絞る」は
	// upstream に合わせた効率の話で、外しても結果は変わらない (子から辿っても
	// 同じツリーに到達するため)。変異検証でもそこは検出できない。
	var out struct {
		Deleted    int64
		Scanned    int
		LastRootID *string
	}
	res := r.db.Raw(`
		WITH RECURSIVE roots AS (
			SELECT n.id AS "rootId", n.id
			FROM "note" n
			WHERE n."replyId" IS NULL
			  AND n."renoteId" IS NULL
			  AND n.id > ?
			  AND (`+noteRemovableExpr+`)
			ORDER BY n.id
			LIMIT ?
		),
		tree AS (
			SELECT r."rootId", r.id FROM roots r
			UNION
			SELECT t."rootId", c.id
			FROM "note" c
			INNER JOIN tree t ON c."replyId" = t.id OR c."renoteId" = t.id
		),
		judged AS (
			SELECT t."rootId", t.id, (`+noteRemovableExpr+`) AS removable
			FROM tree t
			INNER JOIN "note" n ON n.id = t.id
		),
		deleted AS (
			DELETE FROM "note" WHERE id IN (
				SELECT j.id FROM judged j
				WHERE NOT EXISTS (
					SELECT 1 FROM judged k
					WHERE k."rootId" = j."rootId" AND k.removable = FALSE
				)
			)
			RETURNING 1
		)
		SELECT (SELECT count(*) FROM deleted) AS deleted,
		       (SELECT count(*) FROM roots) AS scanned,
		       (SELECT max(id) FROM roots) AS last_root_id`, after, cutoffID, batchSize, cutoffID).Scan(&out)
	if res.Error != nil {
		return 0, 0, "", res.Error
	}
	last := ""
	if out.LastRootID != nil {
		last = *out.LastRootID
	}
	return out.Deleted, out.Scanned, last, nil
}

func (r *noteRepository) DeleteByUserBatch(userID string, batchSize int) (int64, error) {
	if userID == "" {
		return 0, nil
	}
	if batchSize <= 0 {
		batchSize = 100
	}
	res := r.db.Exec(`
		DELETE FROM "note" WHERE id IN (
			SELECT id FROM "note"
			WHERE "userId" = ?
			LIMIT ?
		)`, userID, batchSize)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

// ListByUserList returns notes authored by members of the given user list
// with keyset pagination via paginationOrder. user_list_membership テーブルと
// INNER JOIN してメンバーのノートだけを返す。channel ノートは除外する (TS互換)。
func (r *noteRepository) ListByUserList(listID string, limit int, sinceID, untilID string, filter model.TimelineDBFilter) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 10
	}
	// #1452 (visibility) / #1496 (reply) push-down は viewer ID を使う。旧 viewerID
	// 引数を filter.ViewerID に畳み込んだ (#1498)。
	viewerID := filter.ViewerID
	q := preloadNoteRelations(r.db).
		Joins(`INNER JOIN "user_list_membership" m ON m."userId" = "note"."userId" AND m."userListId" = ?`, listID).
		Where(`"note"."channelId" IS NULL`)
	// visibility push-down: handler の post-fetch FilterVisible を SQL へ移し、
	// LIMIT 前に絞ることで under-fill と followers 判定 N+1 を解消する (#1452)。
	// 条件は FilterVisible (= CanSeeNote) と等価: public/home は常時、followers は
	// viewer が author 本人か follow 済みのときだけ。specified は list timeline には
	// 含めない既存挙動を維持する (DM 非表示 / upstream 準拠)。
	// JOIN した user_list_membership m も "userId" 列を持つため、note 側の列は
	// "note". で修飾して曖昧性を回避する。
	if viewerID == "" {
		q = q.Where(`"note"."visibility" IN ('public','home')`)
	} else {
		// upstream generateVisibilityQuery と同じ構造にする。
		//   public / home は常時
		//   自分の投稿
		//   自分宛て (visibleUserIds / mentions に自分が含まれる)
		//   followers 投稿で自分がフォロワー、または自分の投稿への返信
		//
		// 「自分宛て」は visibility を問わない。specified (DM) でも宛先に自分が
		// 入っていればリスト TL に出る。以前は specified を一律 drop していたが、
		// upstream にそのような特別扱いは無く、自分宛ての DM が欠落していた。
		q = q.Where(
			`("note"."visibility" IN ('public','home') `+
				`OR "note"."userId" = ? `+
				`OR ? = ANY("note"."visibleUserIds") `+
				`OR ? = ANY("note"."mentions") `+
				`OR ("note"."visibility" = 'followers' AND ("note"."userId" IN (`+
				`SELECT f."followeeId" FROM "following" f WHERE f."followerId" = ?) `+
				`OR "note"."replyUserId" = ?)))`,
			viewerID, viewerID, viewerID, viewerID, viewerID)
	}
	// reply 内包: user_list_membership.withReplies を尊重して返信を出し分ける
	// (#1496, upstream user-list-timeline getFromDb と一致)。返信でない / 自己への
	// 返信 / viewer 宛ての返信 / メンバーが withReplies=ON のときだけ返信を含め、
	// それ以外 (第三者宛ての返信) は既定で除外する。withReplies 列は JOIN した
	// membership 由来なので m. で参照する。
	// upstream は各 OR 分岐に `replyId IS NOT NULL AND` ガードを付けるが、非返信は
	// 先頭の `replyId IS NULL` 分岐が必ず拾うため、後続分岐のガードは冗長で省ける
	// (replyId NOT NULL の reply にのみ replyUserId / withReplies 条件が効く)。
	q = q.Where(
		`("note"."replyId" IS NULL `+
			`OR "note"."replyUserId" = "note"."userId" `+
			`OR "note"."replyUserId" = ? `+
			`OR m."withReplies" = true)`,
		viewerID)
	if sinceID != "" {
		q = q.Where(`"note"."id" > ?`, sinceID)
	}
	if untilID != "" {
		q = q.Where(`"note"."id" < ?`, untilID)
	}
	// withRenotes / includeMyRenotes / includeRenotedMyNotes / includeLocalRenotes
	// / withFiles を他 timeline と同じ SQL で適用する (#1498, upstream getFromDb と
	// 一致)。WithReplies は上の per-member 経路 (#1496) で処理済みなので filter には
	// 積まない (nil = applyTimelineFilter は skip)。
	// base-filter (user-mute / renote-mute / 被block / instance-mute / channel-mute)
	// は handler が filter に積むので applyTimelineFilter がここで適用する
	// (#1681、upstream user-list-timeline の generateBaseNoteFilteringQuery 相当)。
	q = applyTimelineFilter(q, filter)
	var notes []*model.Note
	if err := q.Order(paginationOrder(sinceID, untilID, `"note"."id"`)).Limit(limit).Find(&notes).Error; err != nil {
		return nil, err
	}
	return notes, nil
}

// recentReplyScanLimit は CountReplyTargets が集計対象にする直近の返信件数。
// upstream get-frequently-replied-users.ts の `limit(1000)` と同じ値。
const recentReplyScanLimit = 1000

func (r *noteRepository) CountReplyTargets(userID, viewerID string, limit int) ([]model.ReplyTargetCount, error) {
	if limit <= 0 {
		limit = 10
	}
	var rows []model.ReplyTargetCount
	// **直近 1000 件の窓を取ってから集計する。** upstream
	// (get-frequently-replied-users.ts) の recentNotesQuery が
	// `ORDER BY note.id DESC LIMIT 1000` を持つのと同じ。この endpoint は
	// upstream / mk-go とも **未認証で任意の userId に対して叩けて**、
	// レート制限の定義も無い。上限が無いと投稿数に比例して重くなり、
	// 下の相関 EXISTS が 1 行ごとの PK 探索になるので増幅する
	// (実測: 返信 20 万件で 119ms -> 813ms)。
	//
	// **窓の条件は upstream と同じにすること。** 自己返信の除外
	// (`"replyUserId" <> userID`) は mk-go 固有で upstream の窓には無い。これを
	// 内側に置くと、述語に当たらない行が続く限り LIMIT が満たされず、planner が
	// `Index Scan Backward using note_pkey` に倒れて **note テーブル全体**を走査する
	// (実測: 自己返信 200k・他者宛 0 のユーザーで 1.8M 行を全部読み 421ms。外へ
	// 出すと 1.15ms)。**上限を入れたつもりで別経路の DoS を開けることになる。**
	recent := r.db.Model(&model.Note{}).
		Select(`"id", "replyId", "replyUserId"`).
		Where(`"userId" = ? AND "replyId" IS NOT NULL`, userID)
	// visibility push-down: 集計対象 reply note のうち viewer が CanSeeNote で
	// 見られるものだけを残す。これが無いと第三者 viewer が author の
	// followers/specified reply の対人関係を集計値経由で観測できる (#1486)。
	// 条件は ListByUserIDFiltered / ListMentions / SearchByTag と同一。
	//
	// **絞り込みの前に掛ける。** 後に掛けると「見える 1000 件」ではなく
	// 「直近 1000 件のうち見えるもの」になり、upstream と件数が変わる。
	recent = applyViewerVisibility(recent, viewerID).Order(`"id" DESC`).Limit(recentReplyScanLimit)

	// replyUserId が NULL のもの (通常起こり得ないが防御) と自己返信は**窓を取った後**に
	// 除外する (上記のとおり窓の内側に置くと LIMIT が効かなくなる)。
	//
	// **返信先の note にも可視性 gate を掛ける。** 集計は非正規化列 `"replyUserId"`
	// を読むだけなので、これが無いと「返信元は見えるが返信先は見えない」組で
	// 相手の身元が集計値に出る。upstream も 2 つのクエリの両方に
	// generateVisibilityQuery を足している。
	q := r.db.Table(`(?) AS t`, recent).
		Select(`"replyUserId", COUNT(*) AS count`).
		Where(`t."replyUserId" IS NOT NULL AND t."replyUserId" <> ?`, userID)
	q = applyViewerVisibilityExists(q, `t."replyId"`, viewerID)
	err := q.Group(`"replyUserId"`).
		Order(`count DESC`).
		Limit(limit).
		Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}
