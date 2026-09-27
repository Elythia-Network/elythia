// Package antenna provides the user-facing antenna CRUD service plus a
// per-antenna Redis ZSET timeline of matching notes (#2465 で Stream から移行).
//
// #2743 以降は inbound Create / Announce からも呼ばれるのでローカル限定では
// ない (リレー経由だけ除外している。理由は internal/core/federation の
// AntennaHook の doc を参照)。
package antenna

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	corenote "github.com/shiroha-a/mk/internal/core/note"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"

	"github.com/shiroha-a/mk/internal/core/role"
)

// Errors returned by Service.
var (
	// ErrAntennaNotFound is returned when the requested antenna does not exist.
	ErrAntennaNotFound = errors.New("antenna not found")
	// ErrAntennaNameRequired is returned when name is empty on Create / Update.
	ErrAntennaNameRequired = errors.New("antenna name is required")
	// ErrTooManyAntennas is returned by Create when the user already owns
	// `antennaLimit` antennas (#1029, upstream tooManyAntennas)。
	ErrTooManyAntennas = errors.New("antenna limit exceeded")
	// ErrAccessDenied is returned when a user attempts to update / delete an
	// antenna they do not own.
	ErrAccessDenied = errors.New("not the owner of this antenna")
	// ErrInvalidSource is returned when src is not a known antenna source.
	ErrInvalidSource = errors.New("invalid antenna source")
	// ErrNoSuchUserList is returned when src=list references a user list the
	// caller does not own / does not exist (upstream noSuchUserList、#1544)。
	ErrNoSuchUserList = errors.New("no such user list")
)

// AllKeywordsEmpty reports whether every entry of a DNF keyword set is the
// empty string (= upstream `keywords.flat().every(x => x === ”)`)。空配列も
// vacuously true を返す。EMPTY_KEYWORD 検査 (endpoint 層) で用いる。
func AllKeywordsEmpty(kw [][]string) bool {
	for _, row := range kw {
		for _, s := range row {
			if s != "" {
				return false
			}
		}
	}
	return true
}

// validateUserList enforces the upstream noSuchUserList check: when src=list
// references a userListId, the list must exist and be owned by ownerID.
// userListRepo 未配線時は skip (= list source を使わない構成)。
func (s *Service) validateUserList(ownerID string, userListID *string) error {
	if s.userListRepo == nil || userListID == nil || *userListID == "" {
		return nil
	}
	list, err := s.userListRepo.FindByID(*userListID)
	// **DB 障害を not-found に丸めない** (#2799)。
	if err != nil && !repository.IsNotFound(err) {
		return err
	}
	if err != nil || list == nil || list.UserID != ownerID {
		return ErrNoSuchUserList
	}
	return nil
}

// MaxNotesPerAntenna caps how many notes are kept per antenna in the Redis
// zset. pushNote が毎回 ZRemRangeByRank で古い側を落とす (#2465 で Stream から
// ZSET へ移行したので XADD MAXLEN ~ ではない)。
const MaxNotesPerAntenna = 200

// streamKey returns the Redis key for an antenna's note timeline.
//
// # 構造 (#2465 で Stream から ZSET へ変更)
//
// 以前は Redis Stream で、entry id に **push 時刻**を使っていた。一方カーソルの
// 境界は `idGen.ParseTime(untilID)` = **ノートの作成時刻**から作っていたため、
// この 2 つがずれるとページ送りでノートが落ちた。ノート作成と antenna への
// push は別の瞬間なので、近い値にはなっても順序が入れ替わりうる。
//
// 現在は **score を揃えた ZSET に noteID を入れ、辞書順 (= 時刻順) で引く**。
// カーソルの境界がノート ID そのものになるので、push のタイミングは順序に
// 影響しない。ID が辞書順で時刻順になる前提は aid / aidx / meid / objectid /
// ulid の全方式で成り立ち、SQL 側のページングも同じ前提で `id < ?` を使っている。
//
// キー名は据え置く。型が変わるので旧 Stream が残っている環境では WRONGTYPE に
// なるが、読み書きの両方で検出して張り替える (キー名を変えると旧キーが孤児に
// なって残り続ける)。
func streamKey(antennaID string) string {
	return "antennaTimeline:" + antennaID
}

// isWrongType reports whether err is Redis' WRONGTYPE error.
//
// 旧 Stream から ZSET へ張り替える一度きりの移行で使う。
func isWrongType(err error) bool {
	return err != nil && strings.Contains(err.Error(), "WRONGTYPE")
}

// StreamingPublisher publishes a matched antenna note to the WebSocket
// streaming pub/sub topic so that live `antenna` channel subscribers receive a
// push. パッケージ間の循環依存を避けるため interface で受け取る (実装は
// internal/stream.NotePublisher)。topic は streamKey(antennaID) で生成する
// "antennaTimeline:<id>" 論理名で、AntennaChannel が Subscribe する topic と一致する。
type StreamingPublisher interface {
	PublishNote(topic string, n *model.Note, author *model.User)
}

// Service provides antenna CRUD plus matchNote / OnNoteCreated push.
type Service struct {
	repo          repository.AntennaRepository
	userRepo      repository.UserRepository
	followingRepo repository.FollowingRepository
	userListRepo  repository.UserListRepository
	unreadRepo    repository.AntennaNoteUnreadRepository
	client        *redis.Client
	idGen         id.Generator
	clock         func() time.Time
	// rolePolicyProvider は Create で antennaLimit gate に使う (#1029)。
	// nil 時は gate skip (旧挙動互換)。
	rolePolicyProvider RolePolicyProvider
	// publisher は match した note を pubsub topic へ publish する (#1573)。
	// nil 時は ZSET への追加のみ行い realtime 配信は skip (旧挙動)。
	publisher StreamingPublisher
	// sensitiveChannels は excludeNotesInSensitiveChannel の判定に使う。
	// nil なら判定を skip する (= 除外しない)。
	sensitiveChannels SensitiveChannelLookup
	// primaryNotes は PruneDangling が「本当に DB に無いか」を primary で
	// 確かめるために使う。nil なら prune しない (fail-safe)。
	primaryNotes PrimaryNoteExistence
}

// PrimaryNoteExistence confirms which note ids exist, reading from the primary
// DB even when read replicas are configured.
type PrimaryNoteExistence interface {
	ExistingNoteIDsOnPrimary(ids []string) ([]string, error)
}

// SetPrimaryNoteExistence wires the primary-pinned existence check used by
// PruneDangling. 配線しないと prune は一切走らない (#2719)。
func (s *Service) SetPrimaryNoteExistence(p PrimaryNoteExistence) {
	s.primaryNotes = p
}

// SensitiveChannelLookup reports whether a channel is flagged sensitive.
// note は channel を埋めて持たないので、判定のたびに channelId から引く。
type SensitiveChannelLookup interface {
	IsSensitiveChannel(channelID string) bool
}

// SetSensitiveChannelLookup wires the lookup used by
// excludeNotesInSensitiveChannel. 未配線なら除外は行われない。
func (s *Service) SetSensitiveChannelLookup(l SensitiveChannelLookup) {
	s.sensitiveChannels = l
}

// RolePolicyProvider abstracts role-policy lookup for antenna count limits (#1029).
type RolePolicyProvider interface {
	GetUserPolicies(userID string) map[string]any
}

// SetRolePolicyProvider wires a RolePolicyProvider so Create enforces the
// `antennaLimit` role policy (#1029).
func (s *Service) SetRolePolicyProvider(p RolePolicyProvider) {
	s.rolePolicyProvider = p
}

// NewService constructs an antenna Service. userRepo は現状未使用だが、将来
// 拡張のため受け取る (nil 許容)。home / list source を有効にするには、別途
// SetFollowingRepo / SetUserListRepo で repository を注入する必要がある。
func NewService(
	repo repository.AntennaRepository,
	userRepo repository.UserRepository,
	client *redis.Client,
	idGen id.Generator,
) *Service {
	return &Service{
		repo:     repo,
		userRepo: userRepo,
		client:   client,
		idGen:    idGen,
		clock:    time.Now,
	}
}

// SetFollowingRepo enables the `home` antenna source by providing a
// way to check whether the antenna owner follows the note author.
// nil でも antenna service は動くが `home` source は常に不一致扱いになる
// (matchSource 実装と揃える)。
func (s *Service) SetFollowingRepo(r repository.FollowingRepository) {
	s.followingRepo = r
}

// SetUserListRepo enables the `list` antenna source. nil なら list sources は
// 登録/マッチともに reject される。
func (s *Service) SetUserListRepo(r repository.UserListRepository) {
	s.userListRepo = r
}

// SetUnreadRepo attaches an AntennaNoteUnreadRepository. When set, each
// note pushed to an antenna also creates an unread row for the antenna
// owner so /api/i can populate hasUnreadAntenna. Optional — nil disables
// unread tracking (Redis timeline is unaffected).
func (s *Service) SetUnreadRepo(r repository.AntennaNoteUnreadRepository) {
	s.unreadRepo = r
}

// SetStreamingPublisher attaches a StreamingPublisher invoked alongside the
// ZSET write in OnNoteCreated so that live `antenna` channel subscribers
// receive a push (#1573). Without it the per-antenna ZSET is still
// written (so REST `antennas/notes` works) but no pub/sub publish happens and
// realtime delivery is silently disabled. nil-safe: unset disables only the
// pub/sub publish.
func (s *Service) SetStreamingPublisher(p StreamingPublisher) {
	s.publisher = p
}

// SetClock overrides the time source. Intended for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// CreateInput is the parameter set for Service.Create.
type CreateInput struct {
	OwnerID                        string
	Name                           string
	Src                            model.AntennaSource
	UserListID                     *string
	Users                          []string
	Keywords                       [][]string
	ExcludeKeywords                [][]string
	CaseSensitive                  bool
	ExcludeBots                    bool
	WithReplies                    bool
	WithFile                       bool
	LocalOnly                      bool
	ExcludeNotesInSensitiveChannel bool
}

// Create persists a new antenna and returns it.
func (s *Service) Create(in CreateInput) (*model.Antenna, error) {
	if in.Name == "" {
		return nil, ErrAntennaNameRequired
	}
	if in.OwnerID == "" {
		return nil, errors.New("ownerId is required")
	}
	if !validSource(in.Src) {
		return nil, ErrInvalidSource
	}
	// antennaLimit role policy gate (#1029)。policy 経由で取得した上限と
	// 現在保有数を比較。provider 未配線 / policy 不在は gate skip (旧挙動)。
	if s.rolePolicyProvider != nil {
		// float のまま比較する。int に丸めると 0.5 が 0 になり、`>= 0` の
		// ガードは通っても上限が 0 になって挙動が変わる (#2613)。
		if limit, ok := role.PolicyNumber(s.rolePolicyProvider.GetUserPolicies(in.OwnerID)["antennaLimit"]); ok && limit >= 0 {
			count, err := s.repo.CountByUser(in.OwnerID)
			if err != nil {
				return nil, err
			}
			if float64(count) >= limit {
				return nil, ErrTooManyAntennas
			}
		}
	}
	// upstream create.ts: src='list' で userListId 指定時、自分が所有する list か検証。
	// **検証したときだけ保存する** (upstream も `userList ? userList.id : null`)。
	// src が list 以外のときに未検証の値を保存すると、後の update で src だけ
	// list へ切り替えて他人の list を参照できてしまう。
	var userListID *string
	if in.Src == model.AntennaSourceList {
		if err := s.validateUserList(in.OwnerID, in.UserListID); err != nil {
			return nil, err
		}
		if in.UserListID != nil && *in.UserListID != "" {
			userListID = in.UserListID
		}
	}
	// Marshal of [][]string never fails, so we ignore the error.
	// upstream は送られてきた DNF をそのまま保存する。空文字を含む行を
	// 落とすと `excludeKeywords: [[""]]` のような入力が `[]` に化けて、
	// クライアントが保存した内容を読み戻せない。空文字は matchKeywords の
	// strings.Contains が常に true になるので、マッチ挙動は upstream と同じ。
	keywords, _ := json.Marshal(in.Keywords)
	exclude, _ := json.Marshal(in.ExcludeKeywords)
	now := s.clock()
	a := &model.Antenna{
		ID:                             s.idGen.Generate(now),
		LastUsedAt:                     now,
		UserID:                         in.OwnerID,
		Name:                           in.Name,
		Src:                            in.Src,
		UserListID:                     userListID,
		Users:                          in.Users,
		Keywords:                       keywords,
		ExcludeKeywords:                exclude,
		CaseSensitive:                  in.CaseSensitive,
		ExcludeBots:                    in.ExcludeBots,
		WithReplies:                    in.WithReplies,
		WithFile:                       in.WithFile,
		LocalOnly:                      in.LocalOnly,
		ExcludeNotesInSensitiveChannel: in.ExcludeNotesInSensitiveChannel,
		IsActive:                       true,
	}
	if err := s.repo.Create(a); err != nil {
		return nil, err
	}
	return a, nil
}

// Show returns the antenna by id, with an ownership check.
func (s *Service) Show(ownerID, antennaID string) (*model.Antenna, error) {
	a, err := s.repo.FindByID(antennaID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrAntennaNotFound
	}
	if a.UserID != ownerID {
		return nil, ErrAccessDenied
	}
	return a, nil
}

// UpdateInput holds the editable fields of an antenna. nil の field は更新
// されない。
type UpdateInput struct {
	Name                           *string
	Src                            *model.AntennaSource
	UserListID                     *string
	Users                          *[]string
	Keywords                       *[][]string
	ExcludeKeywords                *[][]string
	CaseSensitive                  *bool
	ExcludeBots                    *bool
	WithReplies                    *bool
	WithFile                       *bool
	LocalOnly                      *bool
	IsActive                       *bool
	ExcludeNotesInSensitiveChannel *bool
}

// Update applies the non-nil fields to an antenna owned by ownerID.
func (s *Service) Update(ownerID, antennaID string, in UpdateInput) (*model.Antenna, error) {
	a, err := s.repo.FindByID(antennaID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrAntennaNotFound
	}
	if a.UserID != ownerID {
		return nil, ErrAccessDenied
	}
	// 更新後の src が list なら、更新後に参照する userListId (新値、未指定なら
	// 既存値) が自分の list か検証する。upstream update.ts は「新 src か既存 src
	// が list で userListId 指定時」だけ検証するが、それだと src だけを list へ
	// 切り替えたときに既存の (未検証の) userListId がそのまま有効になる。
	finalSrc := a.Src
	if in.Src != nil {
		finalSrc = *in.Src
	}
	finalListID := a.UserListID
	if in.UserListID != nil {
		finalListID = in.UserListID
	}
	if finalSrc == model.AntennaSourceList {
		if err := s.validateUserList(ownerID, finalListID); err != nil {
			return nil, err
		}
	}
	fields := map[string]any{}
	if in.Name != nil {
		if *in.Name == "" {
			return nil, ErrAntennaNameRequired
		}
		fields["name"] = *in.Name
	}
	if in.Src != nil {
		if !validSource(*in.Src) {
			return nil, ErrInvalidSource
		}
		fields["src"] = *in.Src
	}
	switch {
	case finalSrc != model.AntennaSourceList:
		// list 以外の src では userListId を保存しない (upstream update.ts も
		// 検証していない userListId は null で書く)。既存値も残さない — 残すと
		// 後で src だけ list へ戻したときに参照が復活する。上の検証があるので
		// 復活しても他人の list は通らないが、未検証の値を持ち続ける理由が無い。
		if a.UserListID != nil || in.UserListID != nil {
			fields["userListId"] = nil
		}
	case in.UserListID != nil:
		// #2106 L48 (documented limitation): Go の *string は JSON の null と
		// absent を区別できないため、mk-go は「空文字列で clear」の convention を
		// 採る (JSON null での clear は未対応)。
		if *in.UserListID == "" {
			fields["userListId"] = nil
		} else {
			fields["userListId"] = in.UserListID
		}
	}
	if in.Users != nil {
		// model.Antenna.Users は model.StringArray なので plain []string で
		// 渡すと GORM が空 slice を NULL に倒す drift がある (#896 と同 pattern)。
		// Create と同じく model.StringArray で wrap して空配列 '{}' として
		// 書き込ませる。
		fields["users"] = model.StringArray(*in.Users)
	}
	if in.Keywords != nil {
		// Marshal of [][]string never fails.
		raw, _ := json.Marshal(*in.Keywords)
		fields["keywords"] = []byte(raw)
	}
	if in.ExcludeKeywords != nil {
		raw, _ := json.Marshal(*in.ExcludeKeywords)
		fields["excludeKeywords"] = []byte(raw)
	}
	if in.CaseSensitive != nil {
		fields["caseSensitive"] = *in.CaseSensitive
	}
	if in.ExcludeBots != nil {
		fields["excludeBots"] = *in.ExcludeBots
	}
	if in.WithReplies != nil {
		fields["withReplies"] = *in.WithReplies
	}
	if in.WithFile != nil {
		fields["withFile"] = *in.WithFile
	}
	if in.LocalOnly != nil {
		fields["localOnly"] = *in.LocalOnly
	}
	if in.ExcludeNotesInSensitiveChannel != nil {
		fields["excludeNotesInSensitiveChannel"] = *in.ExcludeNotesInSensitiveChannel
	}
	// 本家 update.ts は編集のたびに isActive=true へ復帰させる (deactivate された
	// antenna は編集で再び有効化される)。mk-go は isActive をユーザー param として
	// 公開する拡張があるため、明示指定された場合はそれを尊重し、未指定の編集では
	// 本家同様 isActive=true へ復帰させる (#1604)。
	if in.IsActive != nil {
		fields["isActive"] = *in.IsActive
	} else {
		fields["isActive"] = true
	}
	// 本家 update.ts と同じく、更新のたびに lastUsedAt を bump する (#1604)。これに
	// より最近編集された antenna は clean cron で未使用扱いされず deactivate されな
	// い。
	fields["lastUsedAt"] = s.clock()
	if err := s.repo.UpdateFields(antennaID, fields); err != nil {
		return nil, err
	}
	return s.repo.FindByID(antennaID)
}

// Delete removes an antenna owned by ownerID. Redis の ZSET も併せて削除する。
func (s *Service) Delete(ownerID, antennaID string) error {
	a, err := s.repo.FindByID(antennaID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrAntennaNotFound
	}
	if a.UserID != ownerID {
		return ErrAccessDenied
	}
	if err := s.repo.Delete(a); err != nil {
		return err
	}
	_ = s.client.Del(context.Background(), streamKey(antennaID)).Err()
	return nil
}

// missingIDs returns the ids that resolved to nothing, preserving input order.
//
// #2718 が internal/core/timeline に持つものと同じ。**共有していない。**
//
// この関数自体は Redis の型にもキー形式にも依存しない純関数なので共有できる。
// **切り出さないのは方針判断で、技術的な障害があるからではない。**
//
// 既存の置き場所候補 `internal/core/notesfilter` は core/note /
// core/notification / entity に依存するため timeline には 12 パッケージ増える
// (antenna は 2 つで済む。両者で重さが違う)。`internal/model` しか要らない
// 新パッケージを作る手もあるが、20 行に満たない関数のために 1 パッケージを
// 増やす判断は
// 2 経路では取らない。3 経路目が出たらそちらへ切り出すこと。
func missingIDs(ids []string, notes []*model.Note) []string {
	if len(notes) == 0 {
		return append([]string(nil), ids...)
	}
	found := make(map[string]struct{}, len(notes))
	for _, n := range notes {
		found[n.ID] = struct{}{}
	}
	// cap は計算しない。len(notes) > len(ids) は現状到達しないが、cap が負だと
	// panic するので証明に依存しない形にしておく (#2718 review LOW-1)。
	var out []string
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			out = append(out, id)
		}
	}
	return out
}

// confirmMissingOnPrimary narrows candidates to those the primary also lacks.
//
// 未配線 / 問い合わせ失敗のときは空を返す (= prune しない)。生きている note の
// ID を消すと戻せないので、疑わしければ何もしない側に倒す。
func (s *Service) confirmMissingOnPrimary(ctx context.Context, candidates []string) ([]string, error) {
	if s.primaryNotes == nil {
		// **配線漏れを黙って飲み込まない (#2719)。** 未配線だと prune が丸ごと
		// no-op になるが、それを検出するテストは書けない (fail-safe なので
		// 何も起きないのが正常系と同じに見える)。ここで警告を出すことだけが、
		// SetPrimaryNoteExistence を消したときに気付く手掛かりになる。
		slog.WarnContext(ctx, "antenna: prune skipped, primary note existence check is not wired",
			"candidates", len(candidates))
		return nil, nil
	}
	existing, err := s.primaryNotes.ExistingNoteIDsOnPrimary(candidates)
	if err != nil {
		// 呼び出し元は err を握り潰すので、理由はここで残す。
		slog.WarnContext(ctx, "antenna: primary existence check failed, skipping prune", "err", err)
		return nil, err
	}
	if len(existing) == 0 {
		return candidates, nil
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
	return out, nil
}

// PruneDangling removes ids that resolve to nothing from the antenna's zset
// (#2719). Best-effort: 失敗しても読み取りには影響しない。
//
// **antenna には TTL が無く、押し出しは新規 push に依存する。** pushNote は
// 毎回 ZRemRangeByRank で上限 200 を維持するので、マッチが続いている antenna
// なら宙吊り ID は 200 件ぶんの新着で流れる。逆に**マッチが止まった antenna
// では残り続ける** (キーワードが狭いものほど効く)。
//
// timeline との本当の非対称は押し出しではなく **DB fallback の有無**。
// timeline は件数不足時に hybridDBFallback で DB から拾い直せるが (ただし
// `meta.enableFanoutTimelineDbFallback` を off にすると止まる、#2762)、antenna は
// 読み取りが Redis の ID だけで完結する。しかも読み取りは overFetch = limit*2
// の範囲しか見ないので、その窓が全部宙吊りだとページが空で返り、クライアントは
// 次の untilId を得られず**行き止まり**になる。
//
// **解消は 1 リクエストあたり overFetch 件ずつ。** 窓の外は見ないので、
// 200 件すべてが宙吊りなら復帰まで複数回のフェッチが要る。
//
// 呼び出し側は **filter を掛ける前の ids と notes** を渡すこと。visibility /
// mute / block で落ちた note は生きているので、filter 後の集合から差分を
// 取ると消してはいけない ID を消す (#2718 と同じ不変条件)。
//
// **消す前に primary で確かめる。** mk-go はリードレプリカを対応しており
// (`dbReplications`、既定 false)、`FindManyByIDsWithUser` はレプリカに振られる。
// antenna への fan-out は primary への commit 直後に走るので、複製が追いつく
// 前に読むと「生きているのに引けない」。そこで prune すると **その note は
// zset から恒久的に消え、戻す経路が無い**。
//
// ID に埋め込まれた時刻での猶予判定は**使えない**。リモート note の ID は
// AP の `published` から発番される (`federation/resolver.go` が ID を作り、
// 許容範囲は `federation/published_time.go` の publishedPastFloor) ので、
// 「たった今 INSERT されたが ID の時刻は数時間前」が普通に起きる。しかも
// リモート note が antenna に入るのは #2743 からで、守るべきクラスと一致する。
//
// primaryNotes が未配線、または問い合わせが失敗したときは prune しない。
//
// **notification / gallery / featured には同じ自己修復を入れていない (#2719)。**
// notification は Redis Stream の `MAXLEN 300` で常時押し出しがあり、featured /
// gallery は window ごとの別キー + TTL (window x3) で読むのは 2 窓だけなので、
// 宙吊りは 6-14 日で自然に消える。恒久的に枠を占めるのは TTL も押し出しの
// 契機も無い antenna だけ。
//
// **timeline (#2715 / PR #2718) 側も同じ確認を通す** (#2757)。向こうも
// `resolve()` → `pruneDangling()` で同じ経路を持ち、レプリカ構成では同じ穴が
// 開いていた。DB fallback があるから安全、とは言えない — fallback は Hybrid
// 以外では別メソッド (`ListHomeTimeline` 等) で、いずれも `AllowPartial` で
// クライアントが無効化でき、global を除く 3 経路は
// `meta.enableFanoutTimelineDbFallback` を off にすれば運用側でも止まり、
// しかも Redis list から消えた ID は戻らない。
//
// **antenna は ephemeral store を読まない。** timeline 側 (#2718) は ephemeral
// の lookup 失敗時に何も返さない段を持つが、antenna の zset に ephemeral ID が
// 入る経路が無い (リレー由来は #2743 で hook ごと外してある) ため持っていない。
// hydration に ephemeral reader を足すならこの前提が崩れる。
// (リレー経路からは入らない、という意味。#2743 の hook が !viaRelay で gate する)
//
// 所有権は呼び出し元 (Notes) が確認済みなので、ここでは検査しない。
func (s *Service) PruneDangling(ctx context.Context, antennaID string, ids []string, notes []*model.Note) {
	if s.client == nil || antennaID == "" {
		return
	}
	dangling := missingIDs(ids, notes)
	if len(dangling) == 0 {
		return
	}
	// **primary 確認より前で ctx を切り離す。** 今の ExistingNoteIDsOnPrimary は
	// ctx を取らないので実害は無いが、将来取るようになったとき、切断で確認が
	// 失敗して fail-safe が働き自己修復が恒久的に空振りする (timeline 側と同じ)。
	ctx = context.WithoutCancel(ctx)
	confirmed, err := s.confirmMissingOnPrimary(ctx, dangling)
	if err != nil || len(confirmed) == 0 {
		return
	}
	dangling = confirmed
	members := make([]any, 0, len(dangling))
	for _, id := range dangling {
		members = append(members, id)
	}
	slog.DebugContext(ctx, "antenna: pruning unresolvable ids", "antennaId", antennaID, "count", len(dangling))
	err = s.client.ZRem(ctx, streamKey(antennaID), members...).Err()
	if isWrongType(err) {
		// 旧 Stream。次の push で張り替わるので消す対象が無いのと同じ扱い。
		return
	}
	if err != nil {
		slog.WarnContext(ctx, "antenna: pruning unresolvable ids failed", "antennaId", antennaID, "err", err)
	}
}

// RemoveNote removes a specific note from an antenna's timeline (upstream
// antennas/remove-note、#2069 #17463)。該当ノートが無い場合は no-op で成功
// (upstream も同様)。
//
// ZSET の member が noteID そのものなので ZREM 一発で消せる (#2465)。Stream
// だった頃は entry id が push 時刻ベースで noteID と対応しないため、全件走査
// して照合する必要があった。
func (s *Service) RemoveNote(ownerID, antennaID, noteID string) error {
	a, err := s.repo.FindByID(antennaID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrAntennaNotFound
	}
	if a.UserID != ownerID {
		return ErrAccessDenied
	}
	err = s.client.ZRem(context.Background(), streamKey(antennaID), noteID).Err()
	if isWrongType(err) {
		// 旧 Stream。次の push で張り替わるので消す対象が無いのと同じ扱い。
		return nil
	}
	return err
}

// ListByUser returns the antennas owned by userID.
func (s *Service) ListByUser(userID string) ([]*model.Antenna, error) {
	return s.repo.ListByUser(userID)
}

// Notes returns note ids matched by the antenna, newest first, optionally
// constrained by `untilID` (strict upper bound — return notes strictly older)
// or `sinceID` (strict lower bound — return notes strictly newer)。
// limit <= 0 ならデフォルト 10、上限 100。
//
// ZSET は score を 0 に揃えてあるので、範囲指定は member (= note id) の
// 辞書順に対する `ZRangeArgs` (`BYLEX`) で行う。aidx は時刻順に単調増加するため、
// untilID / sinceID をそのまま排他境界 (`(<id>`) に使える (#2465)。
//
// untilID / sinceID が空なら全 range を見て最新 N 件を返す (旧挙動互換)。
// パース失敗時は安全側に bound を緩める (= 全 range)。
func (s *Service) Notes(ctx context.Context, ownerID, antennaID string, limit int, sinceID, untilID string) ([]string, error) {
	a, showErr := s.Show(ownerID, antennaID)
	if showErr != nil {
		return nil, showErr
	}
	// 本家 antennas/notes.ts は notes 取得のたびに isActive=true + lastUsedAt=now
	// を bump し、使用中の antenna が clean cron (#1604) で deactivate されないよう
	// にする。非アクティブだった antenna はここで再活性化される。best-effort write
	// で、失敗しても timeline 取得は続行する (この file の他の best-effort write と
	// 同様)。
	//
	// **isActive は非アクティブだったときだけ書く** (#2752)。書き込みは
	// CachedAntennaRepository の invalidate を撃つので、無条件に書くと
	// **antenna タイムラインを開くたびに全 worker の cache が落ちる**。それは
	// 「antenna が実際に使われている構成」= cache が最も要る構成でこそ効く。
	// upstream も同じ理由で `needPublishEvent = !antenna.isActive` の条件付きで
	// しか `antennaUpdated` を publish しない (antennas/notes.ts:89-98)。
	fields := map[string]any{"lastUsedAt": s.clock()}
	if !a.IsActive {
		fields["isActive"] = true
	}
	_ = s.repo.UpdateFields(antennaID, fields)
	// この antenna を読んだので未読行を消す (#2406)。
	//
	// **これが無いと `hasUnreadAntenna` が一度 true になると永久に true のまま**
	// になり、`antenna_note_unread` も単調増加する。matchNote 側が行を作る一方で
	// 消す経路がどこにも無かった。
	//
	// upstream は `getHasUnreadAntenna` が `return false; // TODO` で未読機能ごと
	// 止まっているのでこの問題が起きない。mk-go は未読を実際に計算する分、
	// 既読化も自前で持つ必要がある (docs/divergence.md)。
	//
	// 既存の best-effort write と同じ扱いで、失敗しても timeline 取得は続行する。
	// 消し漏れても次回の閲覧で再試行されるだけで、未読が過剰に出る方向にしか
	// ずれない (既読の note を未読と誤表示することはあっても、その逆は無い)。
	if s.unreadRepo != nil {
		_ = s.unreadRepo.DeleteByAntennaUser(ownerID, antennaID)
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	// 境界は **ノート ID そのもの** (#2465)。以前は ID から時刻を取り出して
	// stream の entry id (= push 時刻) と突き合わせていたため、作成時刻と push
	// 時刻がずれるとページ送りでノートが落ちた。ID を直接使えばこのずれは
	// 原理的に発生しない。
	//
	// `(` は排他境界。FE は最後に表示した note の id を渡してくるので、その
	// note 自身を含めると無限ループする。これは重要な不変条件。
	min := "-"
	max := "+"
	if untilID != "" {
		max = "(" + untilID
	}
	if sinceID != "" {
		min = "(" + sinceID
	}

	// upstream notes.ts: sinceId 単独 (fetch-newer / scroll-up) は昇順
	// (oldest-first) で sinceId 直後の最古 limit 件を返す。それ以外 (untilId /
	// 無指定) は降順 (newest-first)。常に newest-first だと sinceId 単独ページの
	// 並びと集合が逆になる (#1778)。
	key := streamKey(antennaID)
	// **`ZRangeByLex` / `ZRevRangeByLex` は Redis 6.2 で非推奨** になったので
	// `ZRangeArgs` へ寄せる。**`Rev` のときは Start に大きい方を置く** — Redis の
	// `ZRANGE ... BYLEX REV` は `start > stop` を要求する (逆にすると範囲が空になる)。
	//
	// **go-redis v9.21.0 で契約が逆になった** (redis/go-redis#3751)。v9.18.0 までは
	// `appendArgs` が Rev のとき `Stop, Start` の順に並べ替えていたので、呼び出し側は
	// 常に「小さい方が Start」で渡していた。v9.22.0 へ上げたときに、この入れ替えを
	// 呼び出し側へ移した (mkq v1.1.1 への更新で依存が上がり、`TestNotes_*` が
	// 実 Redis で空を返して落ちた)。go-redis を上げるときはここを見直すこと。
	args := redis.ZRangeArgs{Key: key, Start: min, Stop: max, ByLex: true, Count: int64(limit)}
	if sinceID == "" || untilID != "" {
		args.Rev = true
		args.Start, args.Stop = max, min
	}
	out, err := s.client.ZRangeArgs(ctx, args).Result()
	if err != nil {
		if isWrongType(err) {
			// 旧 Stream が残っている環境。次の push で張り替わるので、
			// ここでは空を返す (読み取りで消すと、書き込みの無い
			// antenna が読むたびに DEL を撃つことになる)。
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}

// OnNoteCreated walks every active antenna, evaluates matchNote, and pushes
// matching notes to the per-antenna Redis ZSET. Best-effort: list / push の
// 失敗はログだけで上には伝搬しない (呼び出し元はノート作成成功の prerequisite を
// 既に満たしているため)。
func (s *Service) OnNoteCreated(n *model.Note, author *model.User) {
	if n == nil || author == nil {
		return
	}
	rows, err := s.repo.ListAllActive()
	if err != nil {
		return
	}
	now := s.clock()
	// 同じ note を全アンテナで評価するので、owner ごとの follow 判定と
	// list メンバーシップは 1 回引いて使い回す。アンテナ数に比例して DB を
	// 叩くと push が note 作成に対して大きく遅れ、直後に antennas/notes を
	// 読むと取りこぼす。
	memo := newMatchMemo()
	for _, a := range rows {
		if !s.matchNote(a, n, author, memo) {
			continue
		}
		_ = s.pushNote(context.Background(), a.ID, n.ID, now)
		// realtime 配信 (#1573): ZSET への追加 (= REST `antennas/notes`
		// 用) とは別に、pubsub topic "antennaTimeline:<id>" へ packed note を
		// publish する。ZSET と Pub/Sub は別プリミティブで、AntennaChannel は
		// 後者を Subscribe しているため、これが無いと WS antenna channel に
		// live note が1件も届かない。本家 AntennaService.addNoteToAntenna が
		// redisForTimelines.xadd と globalEventService.publishAntennaStream の
		// 両系統を呼ぶのと一致させる。可視性は push 時点で matchNote ->
		// CanSeeNote (#1464) で owner 視点 gate 済みだが、subscriber 側でも
		// stale-narrowing を OnRedisEvent で re-filter する (#1573 課題2)。
		if s.publisher != nil {
			s.publisher.PublishNote(streamKey(a.ID), n, author)
		}
		// unread tracking: antenna の所有者に対して未読 row を挿入する。
		// Self-authored note は未読扱いしない (TS本家の挙動に合わせる)。
		//
		// ここでは ephemeral note の materialize を **あえて行わない** (#2332)。
		// antenna_note_unread.noteId は note への外部キーなので、DB に行が無い
		// ノートでは Upsert が失敗して未読が付かない。それでも materialize
		// しないのは、広い条件の antenna を 1 つ作るだけで全リレー投稿が DB に
		// 落ち、ephemeral の目的が消えるため。
		//
		// **ただし #2743 以降、リレー由来の note はそもそもここに来ない。**
		// federation 側の antenna hook はリレー配送を除外している
		// (processor.go の handleCreate / handleAnnounce が viaRelay /
		// isRelayDelivery で gate し、publishRelayDeliveredNote では発火しない)。
		// この分岐が効くのは、将来リレー以外の経路で ephemeral note が
		// antenna に載るようになった場合の保険。
		if s.unreadRepo != nil && a.UserID != author.ID {
			_ = s.unreadRepo.Create(&model.AntennaNoteUnread{
				ID:        s.idGen.Generate(now),
				AntennaID: a.ID,
				NoteID:    n.ID,
				UserID:    a.UserID,
			})
		}
	}
}

// pushNote appends a note id to the antenna's Redis ZSET and trims the oldest
// entries back to MaxNotesPerAntenna.
//
// score は全 entry で 0 に揃え、順序は member (= note id) の辞書順に委ねる。
// aidx は時刻順に単調増加するので、これで時系列順になる (#2465)。
// 旧 Stream が残っている場合は一度捨てて張り替える。
func (s *Service) pushNote(ctx context.Context, antennaID, noteID string, _ time.Time) error {
	key := streamKey(antennaID)
	if err := s.zaddNote(ctx, key, noteID); err != nil {
		if !isWrongType(err) {
			return err
		}
		// 旧 Stream が残っている。一度だけ捨てて張り替える (#2465)。ここで
		// 消えるのは高々 MaxNotesPerAntenna 件の timeline entry で、ノート
		// 本体ではない。次の push から正しい構造で積み上がる。
		if delErr := s.client.Del(ctx, key).Err(); delErr != nil {
			return delErr
		}
		if err := s.zaddNote(ctx, key, noteID); err != nil {
			return err
		}
	}
	// 上限を維持する。score が全て同じなので rank は辞書順 = 時刻順。
	// 新しい方 (末尾) を残すので、古い側の余剰を落とす。
	return s.client.ZRemRangeByRank(ctx, key, 0, -int64(MaxNotesPerAntenna)-1).Err()
}

// zaddNote inserts the note ID with a constant score so ZRANGEBYLEX orders by
// the ID itself.
//
// **score は必ず 0 で揃える。** バラつくと ZRANGEBYLEX の結果が未定義になる。
func (s *Service) zaddNote(ctx context.Context, key, noteID string) error {
	return s.client.ZAdd(ctx, key, redis.Z{Score: 0, Member: noteID}).Err()
}

// matchNote evaluates whether the note satisfies the antenna's filter set.
// 評価順 (短絡):
//  1. visibility が antenna owner から見える (= CanSeeNote 相当) かを check
//  2. localOnly でリモート著者なら false
//  3. excludeBots で bot 著者なら false
//  4. withFile が真でファイル添付がなければ false
//  5. withReplies が偽で reply なら false
//  6. source 別に user フィルタを適用
//  7. keywords (DNF) のいずれかにマッチしなければ false
//  8. excludeKeywords (DNF) のいずれかにマッチすれば false
//
// 旧実装は visibility を一切見ずに matched 判定で antenna stream へ push して
// いたため、`src=all` / `src=users` 等の broad source antenna が「antenna owner が
// follow していない author の followers / specified note」を pickup して
// content leak する IDOR があった (#1464)。CanSeeNote 相当を push 段で
// 1 回 gate することで REST `antennas/notes` と WS `antenna` channel の両方で
// 漏洩を断つ。
func (s *Service) matchNote(a *model.Antenna, n *model.Note, author *model.User, memo *matchMemo) bool {
	// visibility gate: antenna owner を viewer とみなして CanSeeNote 判定する。
	// followingRepo 未配線時は CanSeeNote の semantics 通り `followers` を
	// 投稿者本人以外には見せない fail-closed (= matchSource `home` と同じ方針)。
	//
	// **follow 判定は memo 経由で渡す** (#2752)。repo を直接渡すと、followers
	// 可視の note では**アクティブ antenna 数ぶんの `Exists` が逐次で飛ぶ**
	// (memo が効くのは下の matchSource 側だけだった)。同じ note の fan-out 中は
	// owner ごとに 1 回に畳む。
	owner := &model.User{ID: a.UserID}
	if !corenote.CanSeeNoteFunc(owner, n, memo.followsFunc(s)) {
		return false
	}
	// followers visibility かつ owner != author で CanSeeNote を通った場合、
	// 内部で `followingRepo.Exists(owner.ID, author.ID) == true` が確定している。
	// `home` source は同じ pair の follow を再 query するため、ヒントとして
	// matchSource に渡し重複呼び出しを避ける (#1467 review nit)。
	ownerFollowsAuthor := n.Visibility == model.NoteVisibilityFollowers && a.UserID != author.ID
	if a.LocalOnly && author.Host != nil && *author.Host != "" {
		return false
	}
	if a.ExcludeBots && author.IsBot {
		return false
	}
	// upstream AntennaService:117 と同じ gate。channel が sensitive なら
	// excludeNotesInSensitiveChannel のアンテナからは落とす。
	if a.ExcludeNotesInSensitiveChannel && n.ChannelID != nil && *n.ChannelID != "" &&
		s.sensitiveChannels != nil && s.sensitiveChannels.IsSensitiveChannel(*n.ChannelID) {
		return false
	}
	if a.WithFile && len(n.FileIDs) == 0 {
		return false
	}
	if !a.WithReplies && n.ReplyID != nil {
		return false
	}
	if !s.matchSource(a, author, ownerFollowsAuthor, memo) {
		return false
	}
	text := noteText(n)
	if !s.matchKeywords(text, a.Keywords, a.CaseSensitive, true) {
		return false
	}
	if s.matchKeywords(text, a.ExcludeKeywords, a.CaseSensitive, false) {
		return false
	}
	return true
}

// matchMemo caches the per-note lookups shared across antennas.
type matchMemo struct {
	follows map[string]bool
	lists   map[string]map[string]struct{}
}

func newMatchMemo() *matchMemo {
	return &matchMemo{follows: map[string]bool{}, lists: map[string]map[string]struct{}{}}
}

// followsFunc exposes ownerFollows as the predicate CanSeeNoteFunc takes.
//
// followingRepo 未配線なら nil を返す。CanSeeNoteFunc 側はそれを「判定不能」と
// して followers note を fail-closed にするので、repo を直接渡していたときと
// 同じ挙動になる。
//
// **この nil 分岐は二重の防御で、外しても挙動は変わらない** (等価変異)。
// ownerFollows 自身が followingRepo 未配線で false を返すため。契約を明示する
// ために残している。
func (m *matchMemo) followsFunc(s *Service) func(ownerID, authorID string) bool {
	if s.followingRepo == nil {
		return nil
	}
	return func(ownerID, authorID string) bool {
		return m.ownerFollows(s, ownerID, authorID)
	}
}

// ownerFollows reports whether ownerID follows authorID, caching the answer for
// the lifetime of one note fan-out.
func (m *matchMemo) ownerFollows(s *Service, ownerID, authorID string) bool {
	if s.followingRepo == nil {
		return false
	}
	if m == nil {
		ok, err := s.followingRepo.Exists(ownerID, authorID)
		return err == nil && ok
	}
	// **キーは (owner, author) の対にする** (#2752)。以前は ownerID だけで
	// 足りていたが、それは唯一の呼び出し元 (matchSource) が常に author.ID を
	// 渡していたため。可視性 gate から `n.UserID` を渡すようになったので、
	// **同じキーに 2 種類の質問が載る**形になった。今はどの呼び出し元でも
	// `author.ID == n.UserID` だが、そこは型で守られていない。
	key := ownerID + "\x00" + authorID
	if v, hit := m.follows[key]; hit {
		return v
	}
	ok, err := s.followingRepo.Exists(ownerID, authorID)
	v := err == nil && ok
	m.follows[key] = v
	return v
}

// listContains reports whether authorID belongs to listID, caching the member
// set for the lifetime of one note fan-out. The list must be owned by ownerID
// (the antenna owner); otherwise nothing matches.
func (m *matchMemo) listContains(s *Service, ownerID, listID, authorID string) bool {
	if s.userListRepo == nil || listID == "" {
		return false
	}
	fetch := func() map[string]struct{} {
		// 保存時の所有者検証 (validateUserList) に加えて照合時にも見る。
		// 検証が入る前に保存された他人の userListId が残っていても、他人の
		// list のメンバー構成をアンテナ経由で覗けないようにする。
		list, err := s.userListRepo.FindByID(listID)
		if err != nil || list == nil || list.UserID != ownerID {
			return nil
		}
		members, err := s.userListRepo.ListMembers(listID)
		if err != nil {
			return nil
		}
		set := make(map[string]struct{}, len(members))
		for _, mem := range members {
			set[mem.UserID] = struct{}{}
		}
		return set
	}
	if m == nil {
		_, ok := fetch()[authorID]
		return ok
	}
	// 所有者の判定を含むのでキーは (owner, list) の対にする。
	key := ownerID + "\x00" + listID
	set, hit := m.lists[key]
	if !hit {
		set = fetch()
		m.lists[key] = set
	}
	_, ok := set[authorID]
	return ok
}

// matchSource applies the antenna's source filter.
//
//   - users: whitelist — author.username が a.Users に含まれていれば match
//   - users_blacklist: blacklist — 含まれていなければ match
//   - home: a の owner がフォロー中のユーザーのみ match (followingRepo 必須)
//   - list: a.UserListID に紐づいた UserListMembership に author が含まれる
//   - all: すべて match
//
// followingRepo / userListRepo が未注入のときは対応ソースを match 不成立
// とみなす (all へのフォールバックではなく、設定ミスが検出しやすい側)。
//
// ownerFollowsAuthor は matchNote 内 visibility gate (CanSeeNote) で既に
// `Exists(owner, author) == true` が確定している場合 true。`home` source の
// 重複 Exists 呼び出しをスキップするヒント (#1467 review nit)。
func (s *Service) matchSource(a *model.Antenna, author *model.User, ownerFollowsAuthor bool, memo *matchMemo) bool {
	switch a.Src {
	case model.AntennaSourceUsers:
		return matchesAntennaAcct(a.Users, author)
	case model.AntennaSourceUsersBlacklist:
		return !matchesAntennaAcct(a.Users, author)
	case model.AntennaSourceHome:
		if ownerFollowsAuthor {
			return true
		}
		if s.followingRepo == nil {
			return false
		}
		return memo.ownerFollows(s, a.UserID, author.ID)
	case model.AntennaSourceList:
		if s.userListRepo == nil || a.UserListID == nil || *a.UserListID == "" {
			return false
		}
		return memo.listContains(s, a.UserID, *a.UserListID, author.ID)
	default:
		// all
		return true
	}
}

// matchesAntennaAcct reports whether the note author is listed in the antenna's
// `users` set.
//
// upstream は保存値を Acct.parse して `username@host` (local は host 無し) に
// 正規化してから比較する。frontend が送るのは `@bob` / `@bob@remote.example`
// の acct 形式なので、username と素で比較すると 1 件も一致しない。
// 大文字小文字は upstream 同様無視する。
func matchesAntennaAcct(users []string, author *model.User) bool {
	if author == nil {
		return false
	}
	want := strings.ToLower(author.Username)
	if author.Host != nil && *author.Host != "" {
		want += "@" + strings.ToLower(*author.Host)
	}
	for _, u := range users {
		if strings.ToLower(strings.TrimPrefix(strings.TrimSpace(u), "@")) == want {
			return true
		}
	}
	return false
}

// matchKeywords evaluates a DNF (Disjunctive Normal Form) keyword set:
// outer is OR, inner is AND. emptyMatches=true なら空セットを「常にマッチ」、
// false なら「常に非マッチ」として扱う (keywords は emptyMatches=true、
// excludeKeywords は emptyMatches=false)。
func (s *Service) matchKeywords(text string, raw []byte, caseSensitive bool, emptyMatches bool) bool {
	groups, err := decodeKeywords(raw)
	if err != nil {
		return emptyMatches
	}
	// upstream AntennaService は判定の直前に空文字の要素と空になった行を
	// 落とす (`map(xs => xs.filter(x => x !== '')).filter(xs => xs.length > 0)`)。
	// 保存値は素通しなので、`[[""]]` のような入力は「キーワード指定なし」と
	// 同じ扱いになる。ここで落とさないと空文字が全 note に部分一致して
	// 全件マッチ (keywords) / 全件除外 (excludeKeywords) になる。
	groups = cleanKeywordGroups(groups)
	if len(groups) == 0 {
		return emptyMatches
	}
	target := text
	if !caseSensitive {
		target = strings.ToLower(text)
	}
	for _, group := range groups {
		if len(group) == 0 {
			continue
		}
		matched := true
		for _, kw := range group {
			needle := kw
			if !caseSensitive {
				needle = strings.ToLower(kw)
			}
			if !strings.Contains(target, needle) {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

// noteText returns the note's text + cw concatenated for keyword search.
func noteText(n *model.Note) string {
	var sb strings.Builder
	if n.CW != nil {
		sb.WriteString(*n.CW)
		sb.WriteString(" ")
	}
	if n.Text != nil {
		sb.WriteString(*n.Text)
	}
	return sb.String()
}

// cleanKeywordGroups drops empty entries and rows that become empty, mirroring
// upstream AntennaService's clean-up right before matching.
func cleanKeywordGroups(groups [][]string) [][]string {
	out := make([][]string, 0, len(groups))
	for _, row := range groups {
		clean := make([]string, 0, len(row))
		for _, kw := range row {
			if kw != "" {
				clean = append(clean, kw)
			}
		}
		if len(clean) > 0 {
			out = append(out, clean)
		}
	}
	return out
}

// decodeKeywords parses the JSON-encoded DNF keyword groups.
func decodeKeywords(raw []byte) ([][]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var groups [][]string
	if err := json.Unmarshal(raw, &groups); err != nil {
		return nil, err
	}
	return groups, nil
}

// validSource reports whether s is one of the allowed antenna source values.
func validSource(s model.AntennaSource) bool {
	switch s {
	case model.AntennaSourceAll, model.AntennaSourceHome,
		model.AntennaSourceUsers, model.AntennaSourceUsersBlacklist,
		model.AntennaSourceList:
		return true
	}
	return false
}

// OnMoveAccount appends dst's acct to every antenna whose `users` set lists src.
//
// upstream AntennaService.onMoveAccount 相当 (#2419)。アカウント移行時に
// 「src をアンテナに登録していた人」のアンテナへ dst を**追記**する。
//
// **src は消さない。** 移行の引き継ぎは一貫して「旧側を消さずに新側を足す」
// 方向で、旧アカウントがまだ投稿しうる以上そちらの購読も残す。
//
// best-effort。呼び出し時点で移行そのものは確定しているので、失敗しても
// エラーを返さず log に落とす。
func (s *Service) OnMoveAccount(src, dst *model.User) {
	if src == nil || dst == nil || s.repo == nil {
		return
	}
	// #2752 でキャッシュを挟んだが、**通常経路では取りこぼさない**。antenna の
	// 作成・更新・削除はすべて invalidate + cross-worker publish を通るので、
	// 直前に作られた antenna もローカルでは確実に、他 worker でも publish 伝播後
	// には見える。古いスナップショットを見るのは pubsub を落としたときと FK
	// cascade 経由の削除だけで、そこは TTL (1 分) が上界になる。
	antennas, err := s.repo.ListAllActive()
	if err != nil {
		slog.Warn("antenna: list for move failed", "srcID", src.ID, "dstID", dst.ID, "err", err)
		return
	}
	dstAcct := "@" + acctOf(dst)
	for _, a := range antennas {
		// users / usersBlacklist のどちらも `users` 配列を使うので、src を
		// 挙げているものは source の種別を問わず追随させる (upstream も
		// antenna.users だけを見て src を含むか判定している)。
		if !matchesAntennaAcct(a.Users, src) {
			continue
		}
		// 既に dst が入っているアンテナは触らない (再実行しても増えない)。
		if matchesAntennaAcct(a.Users, dst) {
			continue
		}
		next := append(append(model.StringArray{}, a.Users...), dstAcct)
		if err := s.repo.UpdateFields(a.ID, map[string]any{"users": next}); err != nil {
			slog.Warn("antenna: append moved account failed",
				"antennaID", a.ID, "dstID", dst.ID, "err", err)
			continue
		}
		a.Users = next
	}
}

// acctOf renders `username` (local) or `username@host` (remote), lower-cased to
// match matchesAntennaAcct's comparison.
func acctOf(u *model.User) string {
	acct := strings.ToLower(u.Username)
	if u.Host != nil && *u.Host != "" {
		acct += "@" + strings.ToLower(*u.Host)
	}
	return acct
}

// HasRolePolicyProvider reports whether the role policy provider was wired.
//
// 未配線だと `antennaLimit` が効かず、アンテナを無制限に作れる。起動時検査に
// 使う (#2683)。
func (s *Service) HasRolePolicyProvider() bool { return s.rolePolicyProvider != nil }
