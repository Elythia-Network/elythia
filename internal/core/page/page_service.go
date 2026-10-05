// Package page provides the user-facing Page CRUD service plus Like / Unlike
// and featured listing operations. Misskey 互換のローカル限定機能で、
// ActivityPub 連携は持たない。
package page

import (
	"errors"
	"regexp"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// pageNameRe mirrors upstream pageNameSchema (Page.ts): 1-256 文字で、空白 /
// 制御文字 / 予約記号 (:/?#[]@!$&'()*+,;=\%) を含まない (#1548)。
var pageNameRe = regexp.MustCompile(`^[^\s:/?#\[\]@!$&'()*+,;=\\%\x00-\x20]{1,256}$`)

// Errors returned by Service.
var (
	// ErrPageNotFound is returned when the requested page does not exist.
	ErrPageNotFound = errors.New("page not found")
	// ErrPageNameRequired is returned when name is empty on Create / Update.
	ErrPageNameRequired = errors.New("page name is required")
	// ErrPageNameInvalid is returned when name violates pageNameSchema
	// (禁止文字 / 長さ超過、#1548)。
	ErrPageNameInvalid = errors.New("page name has invalid characters")
	// ErrPageTitleRequired is returned when title is empty on Create / Update.
	ErrPageTitleRequired = errors.New("page title is required")
	// ErrAccessDenied is returned when the requester cannot view or mutate
	// the page (e.g. private page accessed by non-owner, or update by
	// non-owner).
	ErrAccessDenied = errors.New("not allowed for this page")
	// ErrAlreadyLiked is returned when a user attempts to Like a page they
	// have already liked.
	ErrAlreadyLiked = errors.New("page is already liked")
	// ErrYourPage is returned when a user attempts to Like their own page.
	// upstream Misskey TS pages/like の yourPage (28800466-...) に対応する
	// (#1438)。自分の page への like を禁止する。
	ErrYourPage = errors.New("cannot like your own page")
	// ErrNotLiked is returned when a user attempts to Unlike a page they
	// have not liked.
	ErrNotLiked = errors.New("page is not liked")
	// ErrPageNameConflict is returned when (userId, name) already exists on
	// Create.
	ErrPageNameConflict = errors.New("page name already in use")
)

// Service provides Page CRUD plus Like / Unlike / Featured operations.
type Service struct {
	repo     repository.PageRepository
	likeRepo repository.PageLikeRepository
	idGen    id.Generator
	clock    func() time.Time
}

// NewService constructs a Page Service.
func NewService(
	repo repository.PageRepository,
	likeRepo repository.PageLikeRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		repo:     repo,
		likeRepo: likeRepo,
		idGen:    idGen,
		clock:    time.Now,
	}
}

// SetClock overrides the time source. Intended for tests.
func (s *Service) SetClock(now func() time.Time) {
	if now != nil {
		s.clock = now
	}
}

// CreateInput is the parameter set for Service.Create. Content / Variables /
// Script はサーバ側では解釈せず、入力をそのまま保存する。
type CreateInput struct {
	OwnerID             string
	Title               string
	Name                string
	Summary             *string
	AlignCenter         bool
	HideTitleWhenPinned bool
	Font                string
	EyeCatchingImageID  *string
	Content             []byte
	Variables           []byte
	Script              string
	Visibility          model.PageVisibility
}

// Create persists a new page and returns it.
func (s *Service) Create(in CreateInput) (*model.Page, error) {
	if in.OwnerID == "" {
		return nil, errors.New("ownerId is required")
	}
	if in.Title == "" {
		return nil, ErrPageTitleRequired
	}
	if in.Name == "" {
		return nil, ErrPageNameRequired
	}
	if !pageNameRe.MatchString(in.Name) {
		return nil, ErrPageNameInvalid
	}
	if _, err := s.repo.FindByUserAndName(in.OwnerID, in.Name); err == nil {
		return nil, ErrPageNameConflict
	}
	now := s.clock()
	if in.Visibility == "" {
		in.Visibility = model.PageVisibilityPublic
	}
	if in.Font == "" {
		in.Font = "sans-serif"
	}
	if len(in.Content) == 0 {
		in.Content = []byte("[]")
	}
	if len(in.Variables) == 0 {
		in.Variables = []byte("[]")
	}
	p := &model.Page{
		ID:                  s.idGen.Generate(now),
		UpdatedAt:           now,
		Title:               in.Title,
		Name:                in.Name,
		Summary:             in.Summary,
		AlignCenter:         in.AlignCenter,
		HideTitleWhenPinned: in.HideTitleWhenPinned,
		Font:                in.Font,
		UserID:              in.OwnerID,
		EyeCatchingImageID:  in.EyeCatchingImageID,
		Content:             in.Content,
		Variables:           in.Variables,
		Script:              in.Script,
		Visibility:          in.Visibility,
	}
	if err := s.repo.Create(p); err != nil {
		return nil, err
	}
	return p, nil
}

// FindByID returns a page by id without any visibility check. Intended
// for endpoints that need raw access regardless of visibility (e.g.
// /api/page-push, which matches the TS original's behaviour of skipping
// permission checks). Returns ErrPageNotFound when the row is missing.
func (s *Service) FindByID(pageID string) (*model.Page, error) {
	p, err := s.repo.FindByID(pageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。全部 ErrPageNotFound に
		// すると、接続断が「そんなページは無い」として 4xx で返る。
		if repository.IsNotFound(err) {
			return nil, ErrPageNotFound
		}
		return nil, err
	}
	return p, nil
}

// Show returns a page by id. requesterID は閲覧者で、空文字なら anonymous
// 扱い。followers / specified visibility のページは所有者だけがアクセスできる
// (簡易実装。フォロー判定や visibleUserIds の評価はフェーズ後半で拡張)。
func (s *Service) Show(requesterID, pageID string) (*model.Page, error) {
	p, err := s.repo.FindByID(pageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。呼び出し側は
		// ErrPageNotFound を 4xx にするので、ここで潰すと接続断が
		// 「そんなページは無い」として返る。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrPageNotFound
	}
	if p.Visibility != model.PageVisibilityPublic && p.UserID != requesterID {
		return nil, ErrAccessDenied
	}
	return p, nil
}

// ShowByName resolves a page via (userId, name) which is the primary
// user-facing identity in the Misskey UI.
func (s *Service) ShowByName(requesterID, ownerID, name string) (*model.Page, error) {
	p, err := s.repo.FindByUserAndName(ownerID, name)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。呼び出し側は
		// ErrPageNotFound を 4xx にするので、ここで潰すと接続断が
		// 「そんなページは無い」として返る。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrPageNotFound
	}
	if p.Visibility != model.PageVisibilityPublic && p.UserID != requesterID {
		return nil, ErrAccessDenied
	}
	return p, nil
}

// UpdateInput holds the editable fields of a Page.
type UpdateInput struct {
	Title               *string
	Name                *string
	Summary             **string
	AlignCenter         *bool
	HideTitleWhenPinned *bool
	Font                *string
	EyeCatchingImageID  **string
	Content             []byte
	Variables           []byte
	Script              *string
	Visibility          *model.PageVisibility
}

// Update applies the non-nil fields to a page owned by ownerID.
func (s *Service) Update(ownerID, pageID string, in UpdateInput) (*model.Page, error) {
	p, err := s.repo.FindByID(pageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。呼び出し側は
		// ErrPageNotFound を 4xx にするので、ここで潰すと接続断が
		// 「そんなページは無い」として返る。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrPageNotFound
	}
	if p.UserID != ownerID {
		return nil, ErrAccessDenied
	}
	fields := map[string]any{}
	if in.Title != nil {
		if *in.Title == "" {
			return nil, ErrPageTitleRequired
		}
		fields["title"] = *in.Title
	}
	if in.Name != nil {
		if *in.Name == "" {
			return nil, ErrPageNameRequired
		}
		if !pageNameRe.MatchString(*in.Name) {
			return nil, ErrPageNameInvalid
		}
		// 名前変更時は (userId, name) 一意制約と衝突しないことを確認する。
		if *in.Name != p.Name {
			// **DB 障害で重複チェックを skip しない** (#2792)。`err == nil` だけを見ると、
			// 接続断のときに「重複なし」と判断して先へ進む。`IDX_page_userId_name` が
			// UNIQUE なので重複行は残らないが、`NAME_ALREADY_EXISTS` を返すべき場面で
			// insert が落ちて 500 になる。
			if dup, err := s.repo.FindByUserAndName(ownerID, *in.Name); err != nil && !repository.IsNotFound(err) {
				return nil, err
			} else if err == nil && dup != nil {
				return nil, ErrPageNameConflict
			}
		}
		fields["name"] = *in.Name
	}
	if in.Summary != nil {
		fields["summary"] = *in.Summary
	}
	if in.AlignCenter != nil {
		fields["alignCenter"] = *in.AlignCenter
	}
	if in.HideTitleWhenPinned != nil {
		fields["hideTitleWhenPinned"] = *in.HideTitleWhenPinned
	}
	if in.Font != nil {
		fields["font"] = *in.Font
	}
	if in.EyeCatchingImageID != nil {
		fields["eyeCatchingImageId"] = *in.EyeCatchingImageID
	}
	// **jsonb 列は `string` へキャストしてから渡す (#3037)。** `[]byte` の
	// まま `Updates` に載せると driver が **bytea** として送り、jsonb 列への
	// 代入が SQLSTATE 22P02 `invalid input syntax for type json` で落ちる。
	// つまり `pages/update` の `content` / `variables` は**どんな値でも 500**
	// だった (実 DB で実測)。`internal/core/user/user_service.go` は同じ罠を
	// 知っていて 6 箇所で `string(...)` を明示しており、ここだけ抜けていた。
	if in.Content != nil {
		fields["content"] = string(in.Content)
	}
	if in.Variables != nil {
		fields["variables"] = string(in.Variables)
	}
	if in.Script != nil {
		fields["script"] = *in.Script
	}
	if in.Visibility != nil {
		fields["visibility"] = *in.Visibility
	}
	fields["updatedAt"] = s.clock()
	if err := s.repo.UpdateFields(pageID, fields); err != nil {
		return nil, err
	}
	return s.repo.FindByID(pageID)
}

// Delete removes a page owned by ownerID. page_like は CASCADE で削除される。
func (s *Service) Delete(ownerID, pageID string) error {
	p, err := s.repo.FindByID(pageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。呼び出し側は
		// ErrPageNotFound を 4xx にするので、ここで潰すと接続断が
		// 「そんなページは無い」として返る。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrPageNotFound
	}
	if p.UserID != ownerID {
		return ErrAccessDenied
	}
	return s.repo.Delete(p)
}

// ListByUser returns the pages owned by userID with cursor (sinceID/untilID)
// or offset pagination. Cursor 指定時は offset を無視。
func (s *Service) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	return s.repo.ListByUser(userID, sinceID, untilID, limit, offset)
}

// Featured returns the public pages ordered by likedCount desc, or by id
// when cursor (sinceID/untilID) is supplied (frontend Paginator対応)。
func (s *Service) Featured(sinceID, untilID string, limit, offset int) ([]*model.Page, error) {
	return s.repo.ListFeatured(sinceID, untilID, limit, offset)
}

// Like attaches a Like row from userID to pageID. Public pages can be liked
// by anyone; followers/specified pages by the owner only (mirrors Show
// access rules).
func (s *Service) Like(userID, pageID string) error {
	if userID == "" {
		return errors.New("userId is required")
	}
	p, err := s.repo.FindByID(pageID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。呼び出し側は
		// ErrPageNotFound を 4xx にするので、ここで潰すと接続断が
		// 「そんなページは無い」として返る。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrPageNotFound
	}
	if p.Visibility != model.PageVisibilityPublic && p.UserID != userID {
		return ErrAccessDenied
	}
	// 自分の page は like できない (upstream TS pages/like の yourPage, #1438)。
	// TS の順序 (noSuchPage -> 可視性 -> yourPage -> alreadyLiked) に合わせて
	// 可視性ゲートの後・already-liked チェックの前に置く。owner は可視性ゲートを
	// 常に素通りするため、ここでの判定が owner への唯一の gate になる。
	if p.UserID == userID {
		return ErrYourPage
	}
	if exists, err := s.likeRepo.Exists(userID, pageID); err != nil {
		return err
	} else if exists {
		return ErrAlreadyLiked
	}
	now := s.clock()
	pl := &model.PageLike{
		ID:     s.idGen.Generate(now),
		UserID: userID,
		PageID: pageID,
	}
	if err := s.likeRepo.Create(pl); err != nil {
		return err
	}
	_ = s.repo.IncrementCount(pageID, "likedCount", 1)
	return nil
}

// Unlike removes a Like row.
func (s *Service) Unlike(userID, pageID string) error {
	if userID == "" {
		return errors.New("userId is required")
	}
	if _, err := s.repo.FindByID(pageID); err != nil {
		// **DB 障害を not-found に丸めない** (#2792)。呼び出し側は
		// ErrPageNotFound を 4xx にするので、ここで潰すと接続断が
		// 「そんなページは無い」として返る。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrPageNotFound
	}
	pl, err := s.likeRepo.FindByPair(userID, pageID)
	if err != nil {
		// **DB 障害を「like していない」にしない** (#2792)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrNotLiked
	}
	if err := s.likeRepo.Delete(pl); err != nil {
		return err
	}
	_ = s.repo.IncrementCount(pageID, "likedCount", -1)
	return nil
}

// HasLiked reports whether `userID` has already liked `pageID`. Used by
// /api/pages/show to populate the upstream-compatible `isLiked` field
// (#1134). Returns false on lookup error so the response stays renderable
// even when the page_like store is transiently unavailable.
func (s *Service) HasLiked(userID, pageID string) bool {
	if userID == "" || pageID == "" {
		return false
	}
	exists, err := s.likeRepo.Exists(userID, pageID)
	if err != nil {
		return false
	}
	return exists
}

// LikedPageIDs returns the set of pageIDs that userID has liked, in one query.
// Used by list endpoints (pages/featured) to batch-populate `isLiked` (#1773).
// Empty userID / pageIDs returns an empty set without querying.
func (s *Service) LikedPageIDs(userID string, pageIDs []string) (map[string]bool, error) {
	set := map[string]bool{}
	if userID == "" || len(pageIDs) == 0 {
		return set, nil
	}
	ids, err := s.likeRepo.ListLikedPageIDs(userID, pageIDs)
	if err != nil {
		return set, err
	}
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}
