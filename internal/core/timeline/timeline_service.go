package timeline

import (
	"context"
	"errors"
	"log/slog"
	"sort"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// MaxTimelineLength caps the number of IDs kept in each Redis timeline list.
// Misskey本家のデフォルトと同じ200。
const MaxTimelineLength = 200

// Errors returned by Service.
var (
	// ErrUnauthenticated is returned by Home/Hybrid timelines when no user is provided.
	ErrUnauthenticated = errors.New("user is required for this timeline")
)

// NoteSource is the minimum interface required by Service to resolve note IDs
// and to fall back to a database scan when Redis is empty.
type NoteSource interface {
	FindManyByIDsWithUser(ids []string) ([]*model.Note, error)
}

// Service exposes the four timeline endpoints (home/local/global/hybrid).
// Reads always go through Redis first; on a miss it falls back to a direct
// repository query.
type Service struct {
	fanout        *FanoutTimelineService
	noteRepo      repository.NoteRepository
	followingRepo repository.FollowingRepository
	// ephemeral はリレー経由でしか観測しない投稿の置き場 (#2332)。DB に無い
	// ID をここから補う。nil なら従来どおり DB のみ。
	ephemeral EphemeralNoteLookup
	// fanoutToggle は meta.enableFanoutTimeline。nil なら常に有効扱い。
	fanoutToggle FanoutToggleProvider
	// dbFallbackToggle は meta.enableFanoutTimelineDbFallback。nil なら常に有効扱い。
	dbFallbackToggle DBFallbackToggleProvider
}

// DBFallbackToggleProvider reports whether meta.enableFanoutTimelineDbFallback
// is currently on.
//
// FanoutToggleProvider と分けてあるのは、**効く相手が違う**ため。
// `enableFanoutTimeline` は push (fanout hook) と read の両方を止めるが、
// こちらは read 側の DB fallback だけを止める。push 側は無関係なので、
// FanoutHook に実装義務を持たせない。
type DBFallbackToggleProvider interface {
	FanoutTimelineDBFallbackEnabled() bool
}

// EphemeralNoteLookup resolves notes that live only in Redis (#2332).
// Implemented by ephemeral.Store; ここで narrow interface にしておくことで
// timeline のテストが Redis 無しで書ける。
type EphemeralNoteLookup interface {
	GetNotes(ctx context.Context, ids []string) ([]*model.Note, error)
}

// NewService creates a new timeline Service.
func NewService(fanout *FanoutTimelineService, noteRepo repository.NoteRepository, followingRepo repository.FollowingRepository) *Service {
	return &Service{fanout: fanout, noteRepo: noteRepo, followingRepo: followingRepo}
}

// SetEphemeralLookup attaches the ephemeral note store so timelines can show
// relay-delivered notes that were never written to the database (#2332).
// Optional — nil keeps the database-only behaviour.
func (s *Service) SetEphemeralLookup(l EphemeralNoteLookup) {
	s.ephemeral = l
}

// SetFanoutToggle attaches meta.enableFanoutTimeline so that reads bypass Redis
// entirely while FTT is off (upstream timeline endpoint の
// `if (!serverSettings.enableFanoutTimeline) return getFromDb()` 相当)。
//
// gate が無いと、FTT を切った後も Redis に残った ID が読まれ続ける。push 側も
// 止まっているので古い ID が押し出されることも無く、タイムラインが過去の内容で
// 固まってしまう。
func (s *Service) SetFanoutToggle(p FanoutToggleProvider) {
	s.fanoutToggle = p
}

// fanoutEnabled reports whether FTT is on. Provider 未配線なら有効扱い。
func (s *Service) fanoutEnabled() bool {
	if s.fanoutToggle == nil {
		return true
	}
	return s.fanoutToggle.FanoutTimelineEnabled()
}

// SetDBFallbackToggle attaches meta.enableFanoutTimelineDbFallback so that the
// database fallback can be switched off while FTT itself stays on
// (upstream FanoutTimelineEndpointService の
// `if (!ps.useDbFallback) ps.dbFallback = () => Promise.resolve([])` 相当)。
//
// **これは負荷を止めるためのつまみ。** #2720 で sinceId を含むページングが
// 必ず DB へ倒れるようになり、frontend の paginator は fetchNewer で sinceId を
// 投げるので、その経路が全て PostgreSQL に落ちる。timeline の JSON キャッシュは
// cursor 無しのみが対象なので緩和されない。upstream が用意している逃げ道が
// これしかない。
func (s *Service) SetDBFallbackToggle(p DBFallbackToggleProvider) {
	s.dbFallbackToggle = p
}

// dbFallbackEnabled reports whether the database fallback may run.
// Provider 未配線なら有効扱い (既定値 true と揃える)。
//
// **off にすると件数は揃わない。** DB では埋めず、Redis の窓の奥を読み進めても
// 足りなければ、その分だけを返す (readFanout)。Redis が空 (または sinceId を含む
// ページング) なら空を返す。upstream も同じで、`dbFallback` を空配列に
// 差し替えるだけなので、呼び出し側から見ると「取れなかった」ことと区別が付かない。
func (s *Service) dbFallbackEnabled() bool {
	if s.dbFallbackToggle == nil {
		return true
	}
	return s.dbFallbackToggle.FanoutTimelineDBFallbackEnabled()
}

// noDBFallback is what home / local / hybrid return in place of a database
// query while the fallback is off. **global は gate していない**ので到達しない
// (GlobalTimeline の doc を参照)。
//
// **非 nil の空 slice を返す。** 現状の呼び出し元 (internal/api/notes の
// handler) は `entity.PackNotes` を通り、そこが `make([]NoteEntity, 0, ...)` で
// 受けるので nil でも JSON は `[]` になる。それに寄りかからないという契約。
// nil を返す実装は「取得できなかった」と区別が付かない。
func noDBFallback() []*model.Note { return []*model.Note{} }

// shouldFallbackToDB reports whether the Redis result must be discarded and the
// whole page served from the DB.
//
// upstream FanoutTimelineEndpointService の
// `shouldFallbackToDb = noteIds.length === 0 || (sinceId != null && sinceId < oldestNoteId)`
// と同じ判定 (#2720)。
//
// **sinceId が Redis の持つ最古 ID より古いなら、その間の範囲を Redis は
// 持っていない。** そのまま Redis の結果を返すと cursor 直後の note を飛ばして
// 先のページを返すことになる。DB へ丸ごと倒せば連続したページになる。
//
// **実際には sinceId が非空なら常に true になる。** Get / GetMulti が
// `id > sinceId` で絞るので、返る ID は必ず sinceId より新しい。upstream の
// 条件も `ps.sinceId != null && ps.sinceId < oldestNoteId` で untilId の有無を
// 見ないので、同じく常に true。つまり **sinceId を含むページングは、
// sinceId 単独か sinceId + untilId かに関わらず必ず DB が処理する**。
//
// これは #2720 以前からの挙動ではない。以前は sinceId + untilId も Redis
// 経路を通っていた (順序も継ぎ足しも正しかった)。upstream に揃えた結果として
// DB へ寄る。
//
// **負荷はここに乗る。** frontend の paginator は fetchNewer で sinceId を
// 投げるので、その経路が全て PostgreSQL に落ちる。timeline の JSON キャッシュ
// (internal/api/notes) は cursor 無しのみが対象なので緩和されない。
//
// これを止めるつまみが `meta.enableFanoutTimelineDbFallback` (#2762)。off に
// すると、この判定が真になった経路は **DB を引かずに空を返す** —
// ただし global timeline は gate から外してある (GlobalTimeline の doc を参照)。
//
// その帰結として、filterAndSort / mergeIDs の昇順分岐と
// fallbackRange の sinceId 側スワップは **endpoint 経路からは到達しない**
// (resolve は昇順分岐を持たず、入力順を保つだけ)。upstream の FanoutTimelineService.get も
// sinceId 単独で ASC を返すので実装としては揃えてあり、ユニットテストで
// 個別に固定してある。判定を緩めるとそれらが一斉に効き始めるので、
// 順序が正しいことが前提になる。
//
// ids は filterAndSort / mergeIDs が向きを決めて返したもの。昇順なら先頭、
// 降順なら末尾が最古。
func shouldFallbackToDB(ids []string, sinceID, untilID string) bool {
	if len(ids) == 0 {
		return true
	}
	if sinceID == "" {
		return false
	}
	oldest := ids[len(ids)-1]
	if isAscending(sinceID, untilID) {
		oldest = ids[0]
	}
	return sinceID < oldest
}

// fallbackRange narrows the database fallback to the range *beyond* what was
// already **resolved**, so the fan-out result is topped up instead of being
// thrown away.
//
// upstream FanoutTimelineEndpointService は Redis から取れた分を残したまま、
// 最後に読んだ ID を境界にして「足りない分だけ」を DB から継ぎ足す
// (`dbUntil = noteIds[noteIds.length - 1]` → `[...redisTimeline, ...gotFromDb]`)。
// mk-go は以前、件数が足りないと同じ範囲を DB で引き直して Redis 側の結果ごと
// 置き換えていたため、fanout が配った note (per-follow withReplies を反映した
// 返信など) が DB query の条件で消える現象が起きていた。
//
// sinceID のみ指定された昇順ページングでは境界が逆になるので、返す since/until
// を入れ替える。
//
// **境界は「Redis から取れた ID」ではなく「実際に解決できた note」から取る**
// (#2715)。ephemeral の TTL が切れると note の実体だけが消えて ID が list に残る。
// その ID を境界にすると、**解決できない古い ID より新しい投稿が DB にあっても
// 返らなくなる** — 実測で home timeline の ID の約半分が解決できない状態になって
// おり、リレー由来でない投稿まで出てこなくなっていた。解決 0 件なら境界を使わず、
// 呼び出し側の since/until をそのまま渡す (= 最新から返す)。
func fallbackRange(notes []*model.Note, sinceID, untilID string) (fbSince, fbUntil string) {
	if len(notes) == 0 {
		return sinceID, untilID
	}
	boundary := notes[len(notes)-1].ID
	if sinceID != "" && untilID == "" {
		return boundary, untilID
	}
	return sinceID, boundary
}

// fanoutRead describes how one timeline reads its fan-out lists and its
// database fallback. readFanout drives it.
type fanoutRead struct {
	// keys are the lists the ids come from. Unresolvable ids are pruned from
	// every one of them.
	keys []Name
	// ids returns every id the lists hold inside the caller's cursor window,
	// de-duplicated and ordered by the cursor direction, and the coverage
	// cutoff of the lists ("" for a single list; see coverageCutoff).
	// readFanout calls it once per request.
	ids func() (ids []string, cutoff string, err error)
	// db queries the database for up to n notes between since and until.
	db func(since, until string, n int) ([]*model.Note, error)
	// gated reports whether meta.enableFanoutTimelineDbFallback applies.
	// global だけ false (GlobalTimeline の doc を参照)。
	gated bool
}

// defaultTimelineLimit is the page size used when the caller passes none.
const defaultTimelineLimit = 20

// fanoutScanFactor sets how many ids each extra round hydrates, as a multiple
// of the notes still missing. scanOlder never reads fewer than limit ids.
//
// upstream は `Math.ceil(remainingToRead * Math.min(1.1 / lastSuccessfulRate, 3))`
// で読む量を決める。ここへ来るのは解決できた note が足りない (成功率が低い) ときなので、
// 上限の 3 倍に固定する。
//
// **下限を limit に置くのは意図的な差。** upstream の式は残りが 1 件なら 3 件ずつ
// しか読まないので、mute で落ちる ID が長く続くと回数が窓の長さに比例して増える。
// 1 回ごとに hydrate・ephemeral・primary 確認・リノート先の入れ子検査が走るので、
// 回数は抑える。多めに読んだ分は scanOlder が limit で切る。
//
// **1 ページ目は limit 件ちょうど読む。** upstream は初回も同じ式で
// `ceil(limit * 1.1)` 件を読む。fallback が有効なら、足りない分は DB が同じ範囲から
// 埋めるので結果は変わらない。
const fanoutScanFactor = 3

// readFanout serves one page from the fan-out lists, falling back to (or
// topping up from) the database where upstream FanoutTimelineEndpointService
// does.
//
// **allowPartial でも 0 件のときは返さない (#3448)。** upstream は
// `ps.allowPartial ? redisTimeline.length !== 0 : redisTimeline.length >= ps.limit`
// を満たすまで Redis の ID を読み進め、読み切っても 0 件なら DB へ倒す。
// 空のページを返すと frontend の paginator は「終端」と判断して以後読まないので、
// 宙吊りの ID (TTL の切れたリレー由来の note) や mute で 1 ページ分が全て消えると、
// 古い note が残っているのにそこで止まっていた。0 件かどうかは、note を落としうる
// filter を全て通した後で判定する (TimelineFilter.PageFilter を参照)。
//
// **Redis の list は 1 リクエストで 1 回だけ読む。** upstream も getMulti で
// 窓の中の ID を全て取ってから、メモリ上で切り出して hydrate する。回ごとに
// LRANGE し直すと、list の長さ L に対して O(L²/limit) の転送になる。
//
// 足りないときの埋め方は DB fallback の有無で分ける。
//   - 使えるとき: 1 ページ目の結果を境界にして DB から埋める (従来どおり)。
//     解決 0 件なら呼び出し側の cursor から引き直す (#2715、fallbackRange の doc)。
//     upstream はその前に Redis を読み進めるが、それをすると Redis に無い
//     新しい note (#2715 で問題になった形) を飛ばすので、DB に任せる。
//   - 切ってあるとき: Redis が唯一の取得元なので、upstream と同じく Redis を
//     読み進める (scanOlder)。読み切っても足りなければ、upstream が
//     `dbFallback` を空配列に差し替えたのと同じく持ち分だけを返す。
//
// 複数の list を混ぜるときは、fallback が使えるなら、全ての list が持っている
// 範囲より古い ID を先に捨てる (#3449、coverageCutoff)。
func (s *Service) readFanout(ctx context.Context, r fanoutRead, viewerID, untilID, sinceID string, limit int, filter TimelineFilter) ([]*model.Note, error) {
	// 公開メソッドが既定値を入れてから呼ぶので通常は到達しないが、0 以下だと
	// scanOlder の読み取り量が 0 になり進まなくなる。ここでも閉じておく。
	if limit <= 0 {
		limit = defaultTimelineLimit
	}
	dbEnabled := !r.gated || s.dbFallbackEnabled()
	all, cutoff, err := r.ids()
	if err != nil {
		return nil, err
	}
	// **cutoff より古い ID は、DB fallback が使えるときだけ捨てる (#3449)。**
	// 捨てた範囲は以下の DB fallback が埋める。切ってあるときは、捨てても
	// 取り戻す先が無く、深い list の古い note まで見えなくなるだけなので残す
	// (coverageCutoff の doc)。
	//
	// **cutoff で切れて 1 ページに満たないときは、allowPartial でも DB で埋める。**
	// 返せる分だけを返すと、次のページは cutoff より古いので必ず DB へ行く。
	// ここで埋めても DB の読み取りの回数は同じで、短いページ (若い list が
	// 1 本あるだけで 1 ページ目が数件になる) を返さずに済む。
	allowPartial := filter.AllowPartial
	if dbEnabled {
		trusted := withinCoverage(all, cutoff)
		if len(trusted) < len(all) && len(trusted) < limit {
			allowPartial = false
		}
		all = trusted
	}
	page := all
	if len(page) > limit {
		page = page[:limit]
	}
	if shouldFallbackToDB(page, sinceID, untilID) {
		if !dbEnabled {
			return noDBFallback(), nil
		}
		return r.db(sinceID, untilID, limit)
	}
	resolved, notes, err := s.resolvePage(ctx, r.keys, page, viewerID, filter)
	if err != nil {
		return nil, err
	}
	if !needsMore(notes, limit, allowPartial) {
		return notes, nil
	}
	if !dbEnabled {
		return s.scanOlder(ctx, r.keys, all[len(page):], notes, limit, viewerID, filter)
	}
	// **境界は filter 前の resolved から取る。** filter で落ちた note も
	// 「解決はできている」ので、DB fallback で引き直す必要は無い。
	fbSince, fbUntil := fallbackRange(resolved, sinceID, untilID)
	rest, err := r.db(fbSince, fbUntil, limit-len(notes))
	if err != nil {
		return nil, err
	}
	return append(notes, rest...), nil
}

// coverageCutoff returns the oldest id that every one of several merged lists
// still covers: the newest of their tails. Ids older than it must not be
// served from Redis. It returns "" (no cutoff) for fewer than two lists.
//
// **複数の list を混ぜる timeline (social / ログイン中の local) の取りこぼしを
// 防ぐ (#3449)。** list ごとに上限の件数と流量が違うので、遡れる深さが違う。
// 浅い list が尽きた先でも深い list の ID だけで 1 ページが埋まるので DB へ倒れず、
// 浅い list にあったはずの期間 (例: フォロー中の人の、ローカルに流れない投稿) が
// 抜けていた。全ての list が持っている範囲 (一番浅い list の tail 以降) だけを
// Redis から返し、そこから先は DB に任せる。
//
// **そこから先を DB が返すには、DB fallback のクエリが list の中身を再現して
// いる必要がある。** list にあった返信 (自分宛て、ホームに配られた返信) を DB が
// 返さないと、cutoff を越えた所から消える。LocalTimeline / hybridDBFallback が
// KeepRepliesToViewer / KeepHomeFanoutReplies を付けるのはこのため。
//
// 本家の未マージの修正 misskey-dev/misskey#13495 も、cutoff (各 list の窓の中の
// 最古のうち一番新しいもの) と DB クエリの拡張 (hybrid に `replyUserId = me`) の
// 組み合わせ。**違うのは次の点。**
//   - list が無い (key が存在しない): cutoff に数えない。#13495 は DB へ倒す
//     (「空と確認済み」を印すダミー ID は、DB の結果が limit 未満の 1 ページ目で
//     しか積まない)。`localTimelineWithReplyTo:<viewer>` は返信を受けたことの無い
//     利用者では常に無いので、そのまま倒すとログイン中の social / local の
//     ほぼ全ての読み取りが DB へ行く。Redis にダミーを積む形は、TS 版と key を
//     共有している (drop-in) ことと、宙吊り ID の除去 (pruneDangling) が消して
//     しまうことから採らない
//   - 窓の中は空だが list はある (tail が untilId 以降): その list は untilId より
//     古い範囲を何も持っていない。cutoff が untilId 以降になり、Redis から何も
//     返さず DB へ倒れる。#13495 と同じ結果
//   - tail は窓の中の最古 (= 最小値) ではなく、位置としての末尾
//     (GetMultiWithTails の doc)
//   - cutoff で切れて 1 ページに満たないときは、allowPartial でも DB で埋める
//     (readFanout)
//   - DB クエリの拡張は hybrid の home 側にも掛け、local (ログイン中) にも掛ける
//
// **list が無いのを数えないことで取りこぼす**のは、他の list が持っている期間の
// 中に、その list に積まれるべき note が DB にだけあるとき。push は全ての list に
// 同時に効くので、起きるのは key だけが失われたとき (Redis の eviction など)。
// Redis 全体を消したときも、全ての list が同じ時点から積み直されるので普通は
// 起きないが、**空の key へは古い ID も無条件に積まれる** (Push の
// 「空のタイムラインなら追加してOK」) ので、最初に積まれたのが遅れて届いた古い
// note だと、その list は実際より深く見え、間が抜けうる。
func coverageCutoff(tails []string) string {
	if len(tails) < 2 {
		return ""
	}
	cutoff := ""
	for _, t := range tails {
		if t > cutoff {
			cutoff = t
		}
	}
	return cutoff
}

// withinCoverage drops the ids older than cutoff, keeping the order. An empty
// cutoff keeps everything.
//
// 降順なら末尾、昇順なら先頭が削れる。昇順 (sinceId 単独) で先頭が削れると
// sinceId との間が空くが、shouldFallbackToDB が sinceId 付きを全て DB へ倒すので
// Redis の結果は使われない。
func withinCoverage(ids []string, cutoff string) []string {
	if cutoff == "" {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id >= cutoff {
			out = append(out, id)
		}
	}
	return out
}

// needsMore reports whether a page still has to be filled: with allowPartial
// any note is enough, otherwise the page must reach limit.
func needsMore(notes []*model.Note, limit int, allowPartial bool) bool {
	if allowPartial {
		return len(notes) == 0
	}
	return len(notes) < limit
}

// resolvePage hydrates ids, prunes the unresolvable ones from keys and applies
// every filter that can drop a note. resolved is the pre-filter result (the
// fallback boundary), notes the post-filter one.
func (s *Service) resolvePage(ctx context.Context, keys []Name, ids []string, viewerID string, filter TimelineFilter) (resolved, notes []*model.Note, err error) {
	resolved, dangling, err := s.resolve(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	s.pruneDangling(ctx, keys, dangling)
	notes = ApplyFilter(resolved, viewerID, filter)
	if filter.PageFilter != nil {
		if notes, err = filter.PageFilter(notes); err != nil {
			return nil, nil, err
		}
	}
	return resolved, notes, nil
}

// scanOlder keeps hydrating the ids left over from the first page while the
// page still needs notes. It is used only while the database fallback is off.
//
// rest は readFanout が 1 回だけ読んだ ID の残りで、cursor の向きに並んでいる。
// ここでは Redis を読み直さず、先頭から切り出していく。
func (s *Service) scanOlder(ctx context.Context, keys []Name, rest []string, notes []*model.Note, limit int, viewerID string, filter TimelineFilter) ([]*model.Note, error) {
	for len(rest) > 0 && needsMore(notes, limit, filter.AllowPartial) {
		// limit > 0 (readFanout が保証) なので n は正になる。
		n := min(max((limit-len(notes))*fanoutScanFactor, limit), len(rest))
		_, more, err := s.resolvePage(ctx, keys, rest[:n], viewerID, filter)
		if err != nil {
			return nil, err
		}
		notes = append(notes, more...)
		rest = rest[n:]
	}
	// notes は ApplyFilter の戻り値から始まるので、空でも非 nil (noDBFallback の契約)。
	// PageFilter も非 nil を返す前提 (TimelineFilter.PageFilter の doc)。
	if len(notes) > limit {
		notes = notes[:limit]
	}
	return notes, nil
}

// HomeTimeline returns the timeline for a logged-in user. The home timeline
// shows notes by users they follow plus their own notes.
func (s *Service) HomeTimeline(ctx context.Context, viewer *model.User, untilID, sinceID string, limit int, filter TimelineFilter) ([]*model.Note, error) {
	if viewer == nil {
		return nil, ErrUnauthenticated
	}
	if limit <= 0 {
		limit = 20
	}
	// upstream notes/timeline の getFromDb は withReplies を持たず、常に
	// 「返信ではない or 自己スレッド」だけを返す。返信を出すかどうかは
	// `following.withReplies` を見る fanout (push) 側の責務。
	dbFilter := toDBFilter(filter, viewer.ID)
	dbFilter.ExcludeRepliesToOthers = true
	if !s.fanoutEnabled() {
		return s.noteRepo.ListHomeTimeline(viewer.ID, limit, sinceID, untilID, dbFilter)
	}
	keys := []Name{HomeTimelineName(viewer.ID)}
	return s.readFanout(ctx, fanoutRead{
		keys: keys,
		ids: func() ([]string, string, error) {
			ids, err := s.fanout.Get(ctx, keys[0], untilID, sinceID, 0)
			return ids, "", err
		},
		db: func(since, until string, n int) ([]*model.Note, error) {
			return s.noteRepo.ListHomeTimeline(viewer.ID, n, since, until, dbFilter)
		},
		gated: true,
	}, viewer.ID, untilID, sinceID, limit, filter)
}

// LocalTimeline returns notes posted by local users with public/home visibility.
func (s *Service) LocalTimeline(ctx context.Context, viewer *model.User, untilID, sinceID string, limit int, filter TimelineFilter) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 20
	}
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	// upstream は LTL を返信の有無で 3 本に分けて持ち、取得時に合流させる。
	//   withReplies=true  -> localTimeline + localTimelineWithReplies
	//   viewer あり        -> localTimeline + localTimelineWithReplyTo:<viewer>
	//   viewer なし        -> localTimeline のみ
	// これで「他人の他人への返信は出ない」「自分宛ての返信は出る」が両立する。
	// upstream local-timeline の getFromDb は withReplies パラメータ (既定
	// false) が偽なら「返信ではない or 自己スレッド」に絞る。mk-go は未指定
	// (nil) を false と同じ扱いにする必要がある。
	//
	// ただし自分宛ての返信は残す (#3449)。fanout は
	// `localTimelineWithReplyTo:<viewer>` に積んで Redis からは出すので、DB が
	// 返さないと、その list の範囲より古い分だけが出なくなる。本家の
	// misskey-dev/misskey#13495 も同じ条件を足している (あちらは hybrid のみ)。
	// FTT を切った経路でも同じ結果になるよう、ここで付ける。
	dbFilter := toDBFilter(filter, viewerID)
	if filter.WithReplies == nil || !*filter.WithReplies {
		dbFilter.ExcludeRepliesToOthers = true
		dbFilter.KeepRepliesToViewer = true
	}
	if !s.fanoutEnabled() {
		return s.noteRepo.ListLocalTimeline(limit, sinceID, untilID, dbFilter)
	}
	keys := []Name{LocalTimeline}
	switch {
	case filter.WithReplies != nil && *filter.WithReplies:
		keys = append(keys, LocalTimelineWithReplies)
	case viewerID != "":
		keys = append(keys, LocalTimelineWithReplyToName(viewerID))
	}
	return s.readFanout(ctx, fanoutRead{
		keys: keys,
		ids: func() ([]string, string, error) {
			return s.mergedWithCutoff(ctx, keys, untilID, sinceID)
		},
		db: func(since, until string, n int) ([]*model.Note, error) {
			return s.noteRepo.ListLocalTimeline(n, since, until, dbFilter)
		},
		gated: true,
	}, viewerID, untilID, sinceID, limit, filter)
}

// GlobalTimeline returns all public notes including federated remotes.
//
// **ここだけ `enableFanoutTimelineDbFallback` で gate しない** (#2762)。upstream の
// `global-timeline` は `FanoutTimelineEndpointService` を通らず常に SQL を引くので、
// このつまみの対象外になっている。mk-go が GTL を fanout 経路にしているのは性能上の
// 拡張であって、つまみの意味論を変える理由にはならない。
//
// gate すると実害がある。同梱 frontend のセットアップウィザードは
// `enableFanoutTimelineDbFallback: q_use === 'single'` を送るので、**group / open で
// 立てたインスタンスは既定 off**。そこで GTL を gate すると、誰も設定を触っていない
// のに Redis list の窓 (`MaxTimelineLength` 固定の 200 件。local / global は
// 専用の meta 列を持たないので変えられない) を超えて遡れなくなる。upstream で同じ設定に
// したインスタンスの GTL は無傷なので、食い違いは mk-go 側の退行として出る。
func (s *Service) GlobalTimeline(ctx context.Context, viewer *model.User, untilID, sinceID string, limit int, filter TimelineFilter) ([]*model.Note, error) {
	if limit <= 0 {
		limit = 20
	}
	viewerID := ""
	if viewer != nil {
		viewerID = viewer.ID
	}
	if !s.fanoutEnabled() {
		return s.noteRepo.ListGlobalTimeline(limit, sinceID, untilID, toDBFilter(filter, viewerID))
	}
	dbFilter := toDBFilter(filter, viewerID)
	return s.readFanout(ctx, fanoutRead{
		keys: []Name{GlobalTimeline},
		ids: func() ([]string, string, error) {
			ids, err := s.fanout.Get(ctx, GlobalTimeline, untilID, sinceID, 0)
			return ids, "", err
		},
		db: func(since, until string, n int) ([]*model.Note, error) {
			return s.noteRepo.ListGlobalTimeline(n, since, until, dbFilter)
		},
		gated: false,
	}, viewerID, untilID, sinceID, limit, filter)
}

// HybridTimeline merges home and local timelines into a single feed.
func (s *Service) HybridTimeline(ctx context.Context, viewer *model.User, untilID, sinceID string, limit int, filter TimelineFilter) ([]*model.Note, error) {
	if viewer == nil {
		return nil, ErrUnauthenticated
	}
	if limit <= 0 {
		limit = 20
	}
	// LTL 側は LocalTimeline 系列を返信の有無で使い分ける (LocalTimeline と
	// 同じ規則)。素の localTimeline には返信が入っていないので、ここで合流させ
	// ないと withReplies=true でも返信が出てこない。
	if !s.fanoutEnabled() {
		return s.hybridDBFallback(viewer, untilID, sinceID, limit, filter)
	}
	stlKeys := []Name{HomeTimelineName(viewer.ID), LocalTimeline}
	if filter.WithReplies != nil && *filter.WithReplies {
		stlKeys = append(stlKeys, LocalTimelineWithReplies)
	} else {
		stlKeys = append(stlKeys, LocalTimelineWithReplyToName(viewer.ID))
	}
	return s.readFanout(ctx, fanoutRead{
		keys: stlKeys,
		ids: func() ([]string, string, error) {
			return s.mergedWithCutoff(ctx, stlKeys, untilID, sinceID)
		},
		db: func(since, until string, n int) ([]*model.Note, error) {
			return s.hybridDBFallback(viewer, until, since, n, filter)
		},
		gated: true,
	}, viewer.ID, untilID, sinceID, limit, filter)
}

// mergedWithCutoff reads every id the lists hold inside the cursor window,
// merged in the cursor direction, together with their coverage cutoff.
//
// LocalTimeline と HybridTimeline が使う。list が 1 本なら cutoff は空で、
// 単独の list を読むのと同じになる。
func (s *Service) mergedWithCutoff(ctx context.Context, keys []Name, untilID, sinceID string) ([]string, string, error) {
	lists, tails, err := s.fanout.GetMultiWithTails(ctx, keys, untilID, sinceID, 0)
	if err != nil {
		return nil, "", err
	}
	return mergeIDs(lists, 0, isAscending(sinceID, untilID)), coverageCutoff(tails), nil
}

// hybridDBFallback queries the home and local timelines from the database and
// merges them. upstream Misskey TS と同 semantics: hybrid (= social) timeline
// は home (followee + 自分) と local (= 同 instance の public) の和集合を返す。
//
// 旧実装は ListHomeTimeline のみを呼んでおり、follow 関係が無い viewer に
// とって local public note (= 同 instance の他 user の public) が落ちていた
// (#819 で Playwright spec が detect)。本 helper は両 query 結果を ID 単位で
// dedup → cursor の向きで sort → limit 截断する。pagination (sinceID/untilID) は両
// query に同じ値を渡すので merged 結果の boundary は upstream と一致する。
//
// 各 query は単独で limit 件まで返すので merged 後の最大件数は 2*limit、
// dedup と truncate を経て最終的に <= limit 件。逆に両 query の和が limit
// に届かなければ best-effort で limit 未満の結果を返す (= upstream parity、
// pagination は keyset 方式で次 page 取得時に補完される設計)。
func (s *Service) hybridDBFallback(viewer *model.User, untilID, sinceID string, limit int, filter TimelineFilter) ([]*model.Note, error) {
	dbFilter := toDBFilter(filter, viewer.ID)
	// upstream hybrid-timeline も withReplies (既定 false) が偽なら
	// 「返信ではない or 自己スレッド」に絞る。未指定 (nil) は false 扱い。
	//
	// ただし、fanout が list に積む返信は残す (#3449)。混ぜる list の範囲より
	// 古い分は DB だけが返すので、DB が list の中身を再現しないと、Redis からは
	// 出ていた返信がそこから先だけ出なくなる。
	//   - local 側: 自分宛ての返信 (`localTimelineWithReplyTo:<viewer>`)
	//   - home 側: 加えて、自分の返信、自分への mention を含む返信、
	//     `withReplies` 付きでフォローしている人の返信、フォロー中のチャンネルの
	//     返信 (ホームの list)
	// local 側に home 側の条件を付けないのは、フォローしていない人の返信まで
	// 出してしまうため (どの list にも無い)。
	if filter.WithReplies == nil || !*filter.WithReplies {
		dbFilter.ExcludeRepliesToOthers = true
		dbFilter.KeepRepliesToViewer = true
	}
	homeFilter := dbFilter
	// **フォロー一覧が読めたときだけ付ける (fail closed)。** fanout はフォロー
	// していない人の followers 限定の投稿への返信を配らない
	// (fanoutToFollowersAndStream の followers-only gate)。SQL 側の同じ判定
	// (HideFollowersOnlyReplyFromNonFollowee) は FollowingIDs が読めたときしか
	// 掛からないので、読めなかったときに home 側の例外だけを広げると、その返信が
	// DB から出てしまう。
	homeFilter.KeepHomeFanoutReplies = homeFilter.HideFollowersOnlyReplyFromNonFollowee
	homeNotes, err := s.noteRepo.ListHomeTimeline(viewer.ID, limit, sinceID, untilID, homeFilter)
	if err != nil {
		return nil, err
	}
	localNotes, err := s.noteRepo.ListLocalTimeline(limit, sinceID, untilID, dbFilter)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{}, len(homeNotes)+len(localNotes))
	out := make([]*model.Note, 0, len(homeNotes)+len(localNotes))
	for _, n := range homeNotes {
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		out = append(out, n)
	}
	for _, n := range localNotes {
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		out = append(out, n)
	}
	// 向きは cursor に従う (#2720)。aidx は時系列で単調増加するので ID 文字列の
	// lexicographic 比較で十分。
	//
	// **無条件 DESC にしてはいけない。** 昇順ページング (sinceId 単独) で
	// 降順に並べてから truncate すると、cursor の直後ではなく**最新 N 件**を
	// 返す。upstream の hybrid は単一クエリ + makePaginationQuery なので
	// 最古 N 件が返る。ここは home / local の 2 クエリを Go 側でマージする
	// mk-go 固有の形なので、向きを自分で持つ必要がある。
	//
	// 取りこぼしは順序だけでは済まない。frontend の paginator は fetchNewer で
	// sinceId に手持ちの最新 ID を渡すので、最新側から返すと間の note が
	// **二度と取得されない**。
	ascending := isAscending(sinceID, untilID)
	sort.Slice(out, func(i, j int) bool {
		if ascending {
			return out[i].ID < out[j].ID
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// toDBFilter converts a TimelineFilter to a model.TimelineDBFilter.
func toDBFilter(f TimelineFilter, viewerID string) model.TimelineDBFilter {
	return model.TimelineDBFilter{
		WithFiles:             f.WithFiles,
		WithRenotes:           f.WithRenotes,
		WithReplies:           f.WithReplies,
		IncludeMyRenotes:      f.IncludeMyRenotes,
		IncludeRenotedMyNotes: f.IncludeRenotedMyNotes,
		IncludeLocalRenotes:   f.IncludeLocalRenotes,
		ViewerID:              viewerID,
		MutedChannelIDs:       f.MutedChannelIDs,
		// FollowingIDs が配線されている経路 (HTL / STL) でだけ SQL 側の gate も
		// 有効にする。post-fetch だけだと DB fallback がすり抜ける。
		HideFollowersOnlyReplyFromNonFollowee: f.FollowingIDs != nil,
		// production の SQL 経路では muting テーブルへの subquery で filter
		// する (#894)。viewer 単位で bind parameter 数が固定 (2) なので
		// heavy-mute viewer (>1000 mute) でも planning コストが膨らまない。
		// Redis cache 経路 (in-memory ApplyFilter) は引き続き
		// TimelineFilter.MutedUserIDs (loadMutedUserIDs で事前取得) を使う。
		// MutedUserIDs literal は test override 用に残す (本 toDBFilter から
		// は伝搬しない)。
		UseMutingSubquery: viewerID != "",
		// renote-mute も同様に subquery 経路を使う (#903)。pure renote 条件
		// は applyTimelineFilter 側で組み立てる。
		UseRenoteMutingSubquery: viewerID != "",
		// 被block / instance-mute は loader で取得した literal list を両経路に
		// 渡す (#1681)。anon viewer では空。
		BlockerIDs:     f.BlockerIDs,
		MutedInstances: f.MutedInstances,
		// followed channel (mute 済除外) を home DB fallback に渡す (#1686)。
		// home 以外の timeline では handler が空のまま渡す。
		FollowedChannelIDs: f.FollowedChannelIDs,
		// Redis 経路 (ApplyFilter) だけで落とすと、件数が欠けた分を埋める DB
		// fallback がリモートのノートを持ってくるので、SQL 側にも渡す。
		LocalUsersOnly: f.LocalUsersOnly,
	}
}

// resolve fetches notes from the repository preserving id ordering, and reports
// which ids resolved to nothing at all.
//
// **dangling を返すのは、呼び出し側が list から消せるようにするため** (#2715)。
// ephemeral の TTL が切れると note の実体だけが消えて ID が list に残り、以後
// 永久に解決できない。黙って落とすだけだと汚染が溜まり続ける。
//
// **確実に消えていると判った ID だけを返す。** ephemeral の lookup が失敗した
// ときは何も返さない — Redis の一時障害で生きている note の ID を消すと、
// 取り返しがつかない。
func (s *Service) resolve(ctx context.Context, ids []string) (notes []*model.Note, dangling []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	notes, err = s.noteRepo.FindManyByIDsWithUser(ids)
	if err != nil {
		return nil, nil, err
	}
	if len(notes) == len(ids) {
		return notes, nil, nil
	}
	if s.ephemeral == nil {
		// ephemeral を使わない構成では、DB に無い = 消えている。
		return notes, missingIDs(ids, notes), nil
	}

	// DB に無かった ID は ephemeral (リレー由来で未 materialize) かもしれない。
	found := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		found[n.ID] = struct{}{}
	}
	missing := make([]string, 0, len(ids)-len(notes))
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}

	eph, err := s.ephemeral.GetNotes(ctx, missing)
	if err != nil {
		// Redis 障害で timeline 全体を落とさない。DB 分だけ返せば、呼び出し側の
		// 件数不足判定が DB fallback に倒してくれる (ただし allowPartial で 1 件でも
		// 残れば倒れず (#3448)、`meta.enableFanoutTimelineDbFallback` が off なら
		// 倒れず、件数は足りないまま返る、#2762)。**dangling は返さない** —
		// 生きている note の ID を消しかねない。
		slog.WarnContext(ctx, "timeline: ephemeral lookup failed", "err", err)
		return notes, nil, nil
	}
	if len(eph) == 0 {
		return notes, missing, nil
	}

	// **入力 ids の順序を復元する。** 無条件に DESC で並べると、昇順ページング
	// (sinceId 単独) で Redis が ASC で渡してきた順序を壊す (#2720)。ids は
	// filterAndSort が向きを決めて返したものなので、それに従う。
	pos := make(map[string]int, len(ids))
	for i, id := range ids {
		pos[id] = i
	}
	// ids に無い ID は末尾へ回す。map の zero value (0) をそのまま使うと
	// **先頭**に来てしまう。現状 notes / eph は ids からしか作られないので
	// 到達しないが、順序の全順序性を実装内で閉じさせておく。
	rank := func(id string) int {
		if i, ok := pos[id]; ok {
			return i
		}
		return len(ids)
	}
	merged := append(notes, eph...)
	sort.Slice(merged, func(i, j int) bool { return rank(merged[i].ID) < rank(merged[j].ID) })
	return merged, missingIDs(ids, merged), nil
}

// missingIDs returns the ids that have no corresponding note, preserving the
// input order.
func missingIDs(ids []string, notes []*model.Note) []string {
	if len(notes) == 0 {
		return append([]string(nil), ids...)
	}
	found := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		found[n.ID] = struct{}{}
	}
	// cap は計算しない。len(notes) > len(ids) は現状到達しないが、cap が負だと
	// panic するので、証明に依存しない形にしておく (#2718 review LOW-1)。
	var out []string
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// confirmMissingOnPrimary narrows candidates to those the primary DB also lacks.
//
// 問い合わせが失敗したときは空を返す (= prune しない)。生きている note の ID を
// list から消すと戻せないので、疑わしければ何もしない側に倒す。
//
// `noteRepo` の nil 検査はしない。呼び出し元は必ず resolve() を先に通り、
// そこで `noteRepo.FindManyByIDsWithUser` を無条件に呼ぶ。dangling が空でない
// なら ids も空でないので、nil ならここへ来る前に panic する。
//
// #2719 の antenna 側にも同じ目的の関数がある。**共有していない理由は
// `missingIDs` と同じで、antenna 側の doc に書いてある** (両方に書くと
// 片側だけ古くなるため、片方に寄せている)。
func (s *Service) confirmMissingOnPrimary(ctx context.Context, candidates []string) []string {
	existing, err := s.noteRepo.ExistingNoteIDsOnPrimary(candidates)
	if err != nil {
		slog.WarnContext(ctx, "timeline: primary existence check failed, skipping prune", "err", err)
		return nil
	}
	if len(existing) == 0 {
		return candidates
	}
	alive := make(map[string]struct{}, len(existing))
	for _, id := range existing {
		alive[id] = struct{}{}
	}
	var out []string
	for _, id := range candidates {
		if _, ok := alive[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// pruneDangling removes ids that resolve to nothing from the timelines they
// came from (#2715)。best-effort で、失敗しても読み取りには影響しない。
//
// **消す前に primary で存在を確かめる (#2757)。** mk-go はリードレプリカを
// 対応しており (`dbReplications`、既定 `false`)、`FindManyByIDsWithUser` は
// レプリカに振られる。
// fanout は primary への commit 直後に走るので、複製が追いつく前に読むと
// 「生きているのに引けない」。そこで prune すると **その note は list から
// 消え、戻す経路が無い**。
//
// DB fallback があるから安全、とは言えない。4 経路とも fallback は
// `filter.AllowPartial` で 1 件でも解決できたページでは走らず (#3448 で
// 0 件のときだけは走るようにした)、
// global を除く 3 経路は `meta.enableFanoutTimelineDbFallback` を off に
// すれば運用側でも止まり (#2762)、しかも一度 list から消えた ID を戻す経路が無い。
func (s *Service) pruneDangling(ctx context.Context, names []Name, dangling []string) {
	if s.fanout == nil || len(names) == 0 || len(dangling) == 0 {
		return
	}
	// **リクエストの ctx を持ち込まない。** クライアントが切断すると ctx が
	// キャンセルされ、pipeline が落ちて自己修復が空振りする。**症状が出るのは
	// リロード時 = 前のリクエストを中断する操作**なので、直したい場面ほど
	// 空振りしやすい (#2718 review MEDIUM-4)。
	//
	// **primary 確認より前で切り離す。** 今の ExistingNoteIDsOnPrimary は ctx を
	// 取らないので実害は無いが、将来取るようになったとき、切断で確認が失敗して
	// fail-safe が働き自己修復が恒久的に空振りする。
	ctx = context.WithoutCancel(ctx)
	dangling = s.confirmMissingOnPrimary(ctx, dangling)
	if len(dangling) == 0 {
		return
	}
	slog.DebugContext(ctx, "timeline: pruning unresolvable ids", "count", len(dangling))
	if err := s.fanout.RemoveMany(ctx, names, dangling); err != nil {
		slog.WarnContext(ctx, "timeline: pruning unresolvable ids failed", "err", err)
	}
}

// mergeIDs flattens multiple ID slices, deduplicates, sorts by the cursor
// direction and caps.
func mergeIDs(slices [][]string, limit int, ascending bool) []string {
	seen := make(map[string]struct{})
	var all []string
	for _, s := range slices {
		for _, id := range s {
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			all = append(all, id)
		}
	}
	// 向きは呼び出し側の cursor に従う。昇順ページング (sinceId 単独) で
	// 降順に並べると、最古 N 件ではなく最新 N 件を切り出してしまう (#2720)。
	sort.Slice(all, func(i, j int) bool {
		if ascending {
			return all[i] < all[j]
		}
		return all[i] > all[j]
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}
