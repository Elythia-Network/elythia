// Package channel provides the user-facing channel CRUD, follow, and timeline
// services. Misskey 互換のローカル限定機能で、ActivityPub 連携は持たない。
package channel

import (
	"errors"
	"time"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// Errors returned by Service.
var (
	// ErrChannelNotFound is returned when the requested channel does not exist.
	ErrChannelNotFound = errors.New("channel not found")
	// ErrChannelNameRequired is returned when name is empty on Create / Update.
	ErrChannelNameRequired = errors.New("channel name is required")
	// ErrAccessDenied is returned when a user attempts to update a channel
	// they do not own.
	ErrAccessDenied = errors.New("not the owner of this channel")
	// ErrAlreadyFollowing is returned when a user already follows the channel.
	ErrAlreadyFollowing = errors.New("already following this channel")
	// ErrNotFollowing is returned when there is no follow record to delete.
	ErrNotFollowing = errors.New("not following this channel")
)

// ModeratorChecker reports whether a user holds a moderator/admin role.
// Update consumes it so moderators can edit channels they do not own,
// matching upstream channels/update (roleService.isModerator). Defined here
// to avoid a hard dep on core/role (circular import). nil disables the
// bypass (owner-only edit).
type ModeratorChecker interface {
	IsModerator(userID string) bool
}

// Service provides channel CRUD / follow / timeline operations.
type Service struct {
	repo             repository.ChannelRepository
	followingRepo    repository.ChannelFollowingRepository
	noteRepo         repository.NoteRepository
	unreadRepo       repository.ChannelNoteUnreadRepository
	moderatorChecker ModeratorChecker
	idGen            id.Generator
	clock            func() time.Time
}

// NewService constructs a channel Service. noteRepo は Timeline / OnNotePosted
// で使用する。
func NewService(
	repo repository.ChannelRepository,
	followingRepo repository.ChannelFollowingRepository,
	noteRepo repository.NoteRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		repo:          repo,
		followingRepo: followingRepo,
		noteRepo:      noteRepo,
		idGen:         idGen,
		clock:         time.Now,
	}
}

// SetClock overrides the time source. Intended for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// SetModeratorChecker wires a ModeratorChecker so Update lets moderators
// edit channels they do not own (upstream channels/update parity). nil keeps
// owner-only edit semantics.
func (s *Service) SetModeratorChecker(m ModeratorChecker) {
	s.moderatorChecker = m
}

// SetUnreadRepo attaches a ChannelNoteUnreadRepository so OnNotePosted can
// fan out per-follower unread rows. Optional — nil disables unread
// tracking (lastNotedAt / notesCount updates still run).
func (s *Service) SetUnreadRepo(r repository.ChannelNoteUnreadRepository) {
	s.unreadRepo = r
}

// CreateInput is the parameter set for Service.Create.
type CreateInput struct {
	OwnerID     string
	Name        string
	Description *string
	Color       string
	IsSensitive bool
	BannerID    *string
	// AllowRenoteToExternal は nil で upstream の `?? true` 既定に従う。
	// 明示的に false を渡せば外部への renote を禁止する channel になる。
	AllowRenoteToExternal *bool
}

// Create persists a new channel and returns it. Color が空文字なら Misskey
// 既定の `#86b300` を入れる。
func (s *Service) Create(in CreateInput) (*model.Channel, error) {
	if in.Name == "" {
		return nil, ErrChannelNameRequired
	}
	if in.OwnerID == "" {
		return nil, errors.New("ownerId is required")
	}
	owner := in.OwnerID
	now := s.clock()
	color := in.Color
	if color == "" {
		color = "#86b300"
	}
	// upstream create.ts:93 は `ps.allowRenoteToExternal ?? true`。未指定 (nil)
	// なら true、明示値があればそれを保存する。
	allowRenoteToExternal := true
	if in.AllowRenoteToExternal != nil {
		allowRenoteToExternal = *in.AllowRenoteToExternal
	}
	c := &model.Channel{
		ID:                    s.idGen.Generate(now),
		Name:                  in.Name,
		Description:           in.Description,
		Color:                 color,
		UserID:                &owner,
		IsSensitive:           in.IsSensitive,
		BannerID:              in.BannerID,
		AllowRenoteToExternal: allowRenoteToExternal,
	}
	if err := s.repo.Create(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Show returns the channel by id.
func (s *Service) Show(channelID string) (*model.Channel, error) {
	c, err := s.repo.FindByID(channelID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrChannelNotFound
	}
	return c, nil
}

// UpdateInput holds the editable fields of a channel.
type UpdateInput struct {
	Name        *string
	Description **string
	Color       *string
	IsArchived  *bool
	IsSensitive *bool
	// BannerID: nil = 変更なし、非 nil で "" = banner 解除(null)、値 = 設定。
	BannerID *string
	// PinnedNoteIDs: nil = 変更なし、非 nil = その配列で置換。
	PinnedNoteIDs *[]string
	// AllowRenoteToExternal: nil = 変更なし、非 nil でその値に更新。
	AllowRenoteToExternal *bool
}

// Update applies the non-nil fields to a channel. The caller (userID) must own
// the channel, or be a moderator — upstream channels/update grants moderators
// edit privilege on channels they do not own (update.ts:91-94).
func (s *Service) Update(userID, channelID string, in UpdateInput) (*model.Channel, error) {
	c, err := s.repo.FindByID(channelID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrChannelNotFound
	}
	// owner でなくても moderator なら編集を許可する (upstream parity)。
	// moderatorChecker 未配線時は owner-only にフォールバックする。
	isOwner := c.UserID != nil && *c.UserID == userID
	isModerator := s.moderatorChecker != nil && s.moderatorChecker.IsModerator(userID)
	if !isOwner && !isModerator {
		return nil, ErrAccessDenied
	}
	fields := map[string]any{}
	if in.Name != nil {
		if *in.Name == "" {
			return nil, ErrChannelNameRequired
		}
		fields["name"] = *in.Name
	}
	if in.Description != nil {
		fields["description"] = *in.Description
	}
	if in.Color != nil && *in.Color != "" {
		fields["color"] = *in.Color
	}
	if in.IsArchived != nil {
		fields["isArchived"] = *in.IsArchived
	}
	if in.IsSensitive != nil {
		fields["isSensitive"] = *in.IsSensitive
	}
	if in.BannerID != nil {
		// "" は banner 解除 (null)、それ以外は設定。なお upstream の TS
		// update.ts は bannerId:null を送っても解除しない (banner は外せない既知
		// 制約) ので、"" 解除は mk-go 独自の利便機能。stock frontend は解除時
		// bannerId:null を送るが、Go の *string は null/absent を区別できず
		// 「変更なし」に落ちるため、この "" 解除分岐は API 直叩き専用。
		if *in.BannerID == "" {
			fields["bannerId"] = nil
		} else {
			fields["bannerId"] = *in.BannerID
		}
	}
	if in.PinnedNoteIDs != nil {
		// model.StringArray でラップしないと空 slice が NULL 化して NOT NULL 制約
		// 違反になる (aliases #729 と同型)。
		fields["pinnedNoteIds"] = model.StringArray(*in.PinnedNoteIDs)
	}
	// upstream update.ts:119 は boolean のときのみ更新する (`typeof === 'boolean'`)。
	// nil は無変更、明示値ならその値で上書きする。
	if in.AllowRenoteToExternal != nil {
		fields["allowRenoteToExternal"] = *in.AllowRenoteToExternal
	}
	if err := s.repo.UpdateFields(channelID, fields); err != nil {
		return nil, err
	}
	return s.repo.FindByID(channelID)
}

// Follow records that user follows the channel.
func (s *Service) Follow(userID, channelID string) error {
	if _, err := s.repo.FindByID(channelID); err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrChannelNotFound
	}
	if exists, err := s.followingRepo.Exists(userID, channelID); err != nil {
		return err
	} else if exists {
		return ErrAlreadyFollowing
	}
	f := &model.ChannelFollowing{
		ID:         s.idGen.Generate(s.clock()),
		FollowerID: userID,
		FolloweeID: channelID,
	}
	if err := s.followingRepo.Create(f); err != nil {
		return err
	}
	_ = s.repo.IncrementCount(channelID, "usersCount", 1)
	return nil
}

// Unfollow removes the follow relationship.
func (s *Service) Unfollow(userID, channelID string) error {
	f, err := s.followingRepo.FindByPair(userID, channelID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotFollowing
	}
	if err := s.followingRepo.Delete(f); err != nil {
		return err
	}
	_ = s.repo.IncrementCount(channelID, "usersCount", -1)
	return nil
}

// ListFollowed returns the channels followed by the user. 結果には channel の
// 実体を解決して返す (followingRepo の戻り値は中間テーブル)。
// frontend Paginator は channel_following.id ベースの cursor で叩いてくる
// ので sinceID / untilID もそのまま forward する。
//
// 各 follow row の channel 実体は FindByIDs で一括解決して N+1 を回避する。
// 元 followings の order は frontend cursor 整合のため保持する必要があるため
// channelByID map を介して並び順を保つ。
func (s *Service) ListFollowed(userID, sinceID, untilID string, limit, offset int) ([]*model.Channel, error) {
	rows, err := s.followingRepo.ListFollowed(userID, sinceID, untilID, limit, offset)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(rows))
	for _, f := range rows {
		ids = append(ids, f.FolloweeID)
	}
	channels, err := s.repo.FindByIDs(ids)
	if err != nil {
		return nil, err
	}
	channelByID := make(map[string]*model.Channel, len(channels))
	for _, c := range channels {
		channelByID[c.ID] = c
	}
	out := make([]*model.Channel, 0, len(rows))
	for _, f := range rows {
		if c, ok := channelByID[f.FolloweeID]; ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// ListOwned returns channels owned by the user with cursor (sinceID/untilID)
// or offset pagination.
func (s *Service) ListOwned(userID, sinceID, untilID string, limit, offset int) ([]*model.Channel, error) {
	// upstream owned.ts は isArchived=FALSE を AND し、cursor 無し時は id DESC で
	// 並べる (makePaginationQuery, #1540)。
	notArchived := false
	return s.repo.List(model.ChannelListFilter{
		OwnerID:    userID,
		IsArchived: &notArchived,
		SortBy:     "-id",
		Limit:      limit,
		Offset:     offset,
		SinceID:    sinceID,
		UntilID:    untilID,
	})
}

// ListFeatured returns the upstream channels/featured set: channels that have
// been posted to (lastNotedAt IS NOT NULL) and are not archived, ordered by
// lastNotedAt DESC, capped at a fixed 10. featured には pagination が無い (#1540)。
func (s *Service) ListFeatured() ([]*model.Channel, error) {
	notArchived := false
	return s.repo.List(model.ChannelListFilter{
		IsArchived:         &notArchived,
		LastNotedAtNotNull: true,
		SortBy:             "-lastNotedAt",
		Limit:              10,
	})
}

// Search returns channels whose name matches the query.
// Search finds channels by name (and description when searchType is
// "nameAndDescription", the upstream default). "nameOnly" matches name only.
func (s *Service) Search(query, searchType, sinceID, untilID string, limit, offset int) ([]*model.Channel, error) {
	// upstream channels/search は常に isArchived=FALSE を AND し、cursor 無し時は
	// id DESC で並べる (makePaginationQuery, #1540)。
	notArchived := false
	return s.repo.List(model.ChannelListFilter{
		Query:             query,
		SearchDescription: searchType != "nameOnly",
		IsArchived:        &notArchived,
		SortBy:            "-id",
		Limit:             limit,
		Offset:            offset,
		SinceID:           sinceID,
		UntilID:           untilID,
	})
}

// Timeline returns notes posted to the channel, newest first. viewerID は
// 閲覧者 ID (匿名は空文字)。public channel は誰でも見られるが、そこに投稿
// された followers / specified note は viewer に応じて SQL で除外しないと
// 非フォロワーへ流出する (#1440)。localOnly は投稿者が local のノートだけに
// LIMIT 前で絞る (匿名 visitor への ugcVisibilityForVisitor = local)。
func (s *Service) Timeline(channelID, viewerID, untilID, sinceID string, limit int, localOnly bool) ([]*model.Note, error) {
	if _, err := s.repo.FindByID(channelID); err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrChannelNotFound
	}
	if localOnly {
		return s.noteRepo.ListLocalByChannelID(channelID, viewerID, untilID, sinceID, limit)
	}
	return s.noteRepo.ListByChannelID(channelID, viewerID, untilID, sinceID, limit)
}

// OnNotePosted updates lastNotedAt / notesCount and, when unreadRepo is
// wired, fans out per-follower unread rows so /api/i can populate
// hasUnreadChannel. Best-effort: errors are ignored.
//
// 呼び出し元: NoteCreateService 経由 (ChannelHook interface 経由で疎結合)。
// note.CreateService は safeGo でこのメソッドを呼ぶため、フォロワー数が
// 多いチャンネルでもループ全体が note 作成のレスポンスをブロックしない。
// authorID は自身の投稿を自身の未読にしないための除外キー。
func (s *Service) OnNotePosted(channelID, noteID, authorID string) {
	now := s.clock()
	_ = s.repo.UpdateFields(channelID, map[string]any{"lastNotedAt": &now})
	_ = s.repo.IncrementCount(channelID, "notesCount", 1)
	if s.unreadRepo == nil || noteID == "" {
		return
	}
	// cursor-based pagination で全フォロワーを走査する (#320)。1 page ごとに
	// bulk INSERT ... ON CONFLICT DO NOTHING するため、popular channel でも
	// O(followers / batchSize) 回のラウンドトリップで fanout 完了する。
	const batchSize = 500
	cursor := ""
	for {
		ids, next, err := s.followingRepo.ListFollowerIDsPage(channelID, cursor, batchSize)
		if err != nil {
			return
		}
		if len(ids) == 0 {
			return
		}
		rows := make([]*model.ChannelNoteUnread, 0, len(ids))
		for _, fid := range ids {
			if fid == authorID {
				continue
			}
			rows = append(rows, &model.ChannelNoteUnread{
				ID:        s.idGen.Generate(now),
				ChannelID: channelID,
				NoteID:    noteID,
				UserID:    fid,
			})
		}
		if len(rows) > 0 {
			_ = s.unreadRepo.CreateMany(rows)
		}
		// 最終ページ (limit 未満) に達したら終了。
		if len(ids) < batchSize {
			return
		}
		cursor = next
	}
}
