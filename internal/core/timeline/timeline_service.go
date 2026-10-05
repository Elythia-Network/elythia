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
// **off にすると件数は揃わない。** Redis が持っている分だけを返し、足りない分は
// 埋めない。Redis が空 (または sinceId が Redis の窓より古い) なら空を返す。
// upstream も同じで、`dbFallback` を空配列に差し替えるだけなので呼び出し側から
// 見ると「取れなかった」ことと区別が付かない。
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
// その帰結として、filterAndSort / mergeIDs / GetMerged の昇順分岐と
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
	ids, err := s.fanout.Get(ctx, HomeTimelineName(viewer.ID), untilID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	if !shouldFallbackToDB(ids, sinceID, untilID) {
		resolved, dangling, err := s.resolve(ctx, ids)
		if err != nil {
			return nil, err
		}
		s.pruneDangling(ctx, []Name{HomeTimelineName(viewer.ID)}, dangling)
		notes := ApplyFilter(resolved, viewer.ID, filter)
		if !filter.AllowPartial && len(notes) < limit && s.dbFallbackEnabled() {
			// **境界は filter 前の resolved から取る。** filter で落ちた note も
			// 「解決はできている」ので、DB fallback で引き直す必要は無い。
			fbSince, fbUntil := fallbackRange(resolved, sinceID, untilID)
			rest, err := s.noteRepo.ListHomeTimeline(viewer.ID, limit-len(notes), fbSince, fbUntil, dbFilter)
			if err != nil {
				return nil, err
			}
			return append(notes, rest...), nil
		}
		return notes, nil
	}
	if !s.dbFallbackEnabled() {
		return noDBFallback(), nil
	}
	return s.noteRepo.ListHomeTimeline(viewer.ID, limit, sinceID, untilID, dbFilter)
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
	dbFilter := toDBFilter(filter, viewerID)
	if filter.WithReplies == nil || !*filter.WithReplies {
		dbFilter.ExcludeRepliesToOthers = true
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
	ids, err := s.fanout.GetMerged(ctx, keys, untilID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	if !shouldFallbackToDB(ids, sinceID, untilID) {
		resolved, dangling, err := s.resolve(ctx, ids)
		if err != nil {
			return nil, err
		}
		s.pruneDangling(ctx, keys, dangling)
		notes := ApplyFilter(resolved, viewerID, filter)
		if !filter.AllowPartial && len(notes) < limit && s.dbFallbackEnabled() {
			fbSince, fbUntil := fallbackRange(resolved, sinceID, untilID)
			rest, err := s.noteRepo.ListLocalTimeline(limit-len(notes), fbSince, fbUntil, dbFilter)
			if err != nil {
				return nil, err
			}
			return append(notes, rest...), nil
		}
		return notes, nil
	}
	if !s.dbFallbackEnabled() {
		return noDBFallback(), nil
	}
	return s.noteRepo.ListLocalTimeline(limit, sinceID, untilID, dbFilter)
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
	ids, err := s.fanout.Get(ctx, GlobalTimeline, untilID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	if !shouldFallbackToDB(ids, sinceID, untilID) {
		resolved, dangling, err := s.resolve(ctx, ids)
		if err != nil {
			return nil, err
		}
		s.pruneDangling(ctx, []Name{GlobalTimeline}, dangling)
		notes := ApplyFilter(resolved, viewerID, filter)
		if !filter.AllowPartial && len(notes) < limit {
			fbSince, fbUntil := fallbackRange(resolved, sinceID, untilID)
			rest, err := s.noteRepo.ListGlobalTimeline(limit-len(notes), fbSince, fbUntil, toDBFilter(filter, viewerID))
			if err != nil {
				return nil, err
			}
			return append(notes, rest...), nil
		}
		return notes, nil
	}
	return s.noteRepo.ListGlobalTimeline(limit, sinceID, untilID, toDBFilter(filter, viewerID))
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
	multi, err := s.fanout.GetMulti(ctx, stlKeys, untilID, sinceID, limit)
	if err != nil {
		return nil, err
	}
	merged := mergeIDs(multi, limit, isAscending(sinceID, untilID))
	if !shouldFallbackToDB(merged, sinceID, untilID) {
		resolved, dangling, err := s.resolve(ctx, merged)
		if err != nil {
			return nil, err
		}
		s.pruneDangling(ctx, stlKeys, dangling)
		notes := ApplyFilter(resolved, viewer.ID, filter)
		if !filter.AllowPartial && len(notes) < limit && s.dbFallbackEnabled() {
			fbSince, fbUntil := fallbackRange(resolved, sinceID, untilID)
			rest, err := s.hybridDBFallback(viewer, fbUntil, fbSince, limit-len(notes), filter)
			if err != nil {
				return nil, err
			}
			return append(notes, rest...), nil
		}
		return notes, nil
	}
	if !s.dbFallbackEnabled() {
		return noDBFallback(), nil
	}
	return s.hybridDBFallback(viewer, untilID, sinceID, limit, filter)
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
	if filter.WithReplies == nil || !*filter.WithReplies {
		dbFilter.ExcludeRepliesToOthers = true
	}
	homeNotes, err := s.noteRepo.ListHomeTimeline(viewer.ID, limit, sinceID, untilID, dbFilter)
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
		// 件数不足判定が DB fallback に倒してくれる (ただし
		// `meta.enableFanoutTimelineDbFallback` が off なら倒れず、件数は
		// 足りないまま返る、#2762)。**dangling は返さない** —
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
// `!filter.AllowPartial` でゲートされていてクライアントが無効化でき、
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
