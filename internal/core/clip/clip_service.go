// Package clip provides the user-facing clip CRUD service plus add/remove
// note operations. Misskey 互換のローカル限定機能で、ActivityPub 連携は
// 持たない。
package clip

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
	"gorm.io/gorm"

	"github.com/shiroha-a/mk/internal/core/role"
)

// Errors returned by Service.
var (
	// ErrClipNotFound is returned when the requested clip does not exist.
	ErrClipNotFound = errors.New("clip not found")
	// ErrClipNameRequired is returned when name is empty on Create / Update.
	ErrClipNameRequired = errors.New("clip name is required")
	// ErrNoteNotFound is returned when the target note for AddNote does not
	// exist.
	ErrNoteNotFound = errors.New("note not found")
	// ErrAlreadyClipped is returned when a note is already attached to the
	// clip.
	ErrAlreadyClipped = errors.New("note is already clipped")
	// ErrTooManyClips は Create で clipLimit 超過 (#1029、upstream
	// tooManyClips)。
	ErrTooManyClips = errors.New("clip limit exceeded")
	// ErrTooManyClipNotes は AddNote で noteEachClipsLimit 超過 (#1029、
	// upstream tooManyClipNotes)。
	ErrTooManyClipNotes = errors.New("clip note limit exceeded")
)

// Service provides clip CRUD plus AddNote / RemoveNote / Notes operations.
type Service struct {
	repo     repository.ClipRepository
	noteRepo repository.ClipNoteRepository
	notes    repository.NoteRepository
	idGen    id.Generator
	clock    func() time.Time
	// rolePolicyProvider は clipLimit / noteEachClipsLimit の gate に使う
	// (#1029)。nil 時は gate skip。
	rolePolicyProvider role.PolicyProvider
	// materializer はリレー由来で DB に無いノートを昇格させる (#2332)。
	// clip_note.noteId が note への外部キー。
	materializer NoteMaterializer
}

// SetRolePolicyProvider wires a role policy source so Create / AddNote
// enforce the clipLimit / noteEachClipsLimit role policies (#1029).
func (s *Service) SetRolePolicyProvider(p role.PolicyProvider) {
	s.rolePolicyProvider = p
}

// NewService constructs a clip Service.
func NewService(
	repo repository.ClipRepository,
	noteRepo repository.ClipNoteRepository,
	notes repository.NoteRepository,
	idGen id.Generator,
) *Service {
	return &Service{
		repo:     repo,
		noteRepo: noteRepo,
		notes:    notes,
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

// CreateInput is the parameter set for Service.Create.
type CreateInput struct {
	OwnerID     string
	Name        string
	Description *string
	IsPublic    bool
}

// Create persists a new clip and returns it.
func (s *Service) Create(in CreateInput) (*model.Clip, error) {
	if in.Name == "" {
		return nil, ErrClipNameRequired
	}
	if in.OwnerID == "" {
		return nil, errors.New("ownerId is required")
	}
	// clipLimit role policy gate (#1029)。
	if s.rolePolicyProvider != nil {
		if limit, ok := role.PolicyNumber(s.rolePolicyProvider.GetUserPolicies(in.OwnerID)["clipLimit"]); ok && limit >= 0 {
			count, err := s.repo.CountByUser(in.OwnerID)
			if err != nil {
				return nil, err
			}
			if float64(count) >= limit {
				return nil, ErrTooManyClips
			}
		}
	}
	now := s.clock()
	c := &model.Clip{
		ID:          s.idGen.Generate(now),
		UserID:      in.OwnerID,
		Name:        in.Name,
		Description: in.Description,
		IsPublic:    in.IsPublic,
	}
	if err := s.repo.Create(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Show returns a clip by id. requesterID は閲覧者で、空文字なら anonymous
// 扱い。private clip は所有者だけがアクセスでき、他人/匿名には missing と
// 同じ ErrClipNotFound を返して存在を秘匿する (#1562、upstream show.ts は
// !isPublic && 非所有者を noSuchClip にする。ErrAccessDenied だと 403/404 の
// 差で非公開 clip の存在 enumeration oracle になる)。
func (s *Service) Show(requesterID, clipID string) (*model.Clip, error) {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrClipNotFound
	}
	if !c.IsPublic && c.UserID != requesterID {
		return nil, ErrClipNotFound
	}
	return c, nil
}

// Exists reports whether a clip row exists, regardless of visibility. clips/
// unfavorite の存在チェック用 (#1562、upstream unfavorite.ts は findOneBy({id})
// のみで visibility を見ない — favorite 後に非公開化された clip も解除可能に
// するため)。not-found 以外の driver error は caller へ返す (fail-closed)。
func (s *Service) Exists(clipID string) (bool, error) {
	if _, err := s.repo.FindByID(clipID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Get returns a clip by ID WITHOUT a visibility gate. Used by
// clips/my-favorites which mirrors upstream (favorite 行に紐づく clip を
// visibility 無検査で全件返す: 公開 clip を favorite した後に所有者が非公開化
// しても自分の favorites には残す、#1830)。可視性判定が必要な経路は Show を使う。
func (s *Service) Get(clipID string) (*model.Clip, error) {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrClipNotFound
	}
	return c, nil
}

// UpdateInput holds the editable fields of a clip.
type UpdateInput struct {
	Name        *string
	Description **string
	IsPublic    *bool
}

// Update applies the non-nil fields to a clip owned by ownerID. 非 owner は
// upstream ClipService.update の findOneBy({id, userId}) と同じく missing と
// 区別不能な ErrClipNotFound (#1562、存在 enumeration oracle 封じ)。
func (s *Service) Update(ownerID, clipID string, in UpdateInput) (*model.Clip, error) {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrClipNotFound
	}
	if c.UserID != ownerID {
		return nil, ErrClipNotFound
	}
	fields := map[string]any{}
	if in.Name != nil {
		if *in.Name == "" {
			return nil, ErrClipNameRequired
		}
		fields["name"] = *in.Name
	}
	if in.Description != nil {
		fields["description"] = *in.Description
	}
	if in.IsPublic != nil {
		fields["isPublic"] = *in.IsPublic
	}
	if err := s.repo.UpdateFields(clipID, fields); err != nil {
		return nil, err
	}
	return s.repo.FindByID(clipID)
}

// Delete removes a clip owned by ownerID. clip_note は CASCADE で削除される。
// 非 owner は Update と同じく ErrClipNotFound (#1562)。
func (s *Service) Delete(ownerID, clipID string) error {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrClipNotFound
	}
	if c.UserID != ownerID {
		return ErrClipNotFound
	}
	return s.repo.Delete(c)
}

// ListByUser returns the clips owned by userID with cursor (sinceID/untilID)
// or offset pagination. cursor 指定時は offset 無視。
func (s *Service) ListByUser(userID, sinceID, untilID string, limit, offset int) ([]*model.Clip, error) {
	return s.repo.ListByUser(userID, sinceID, untilID, limit, offset)
}

// AddNote attaches a note to the clip. 非 owner は upstream add-note.ts の
// findOneBy({id, userId}) と同じく ErrClipNotFound (#1562)。
func (s *Service) AddNote(ownerID, clipID, noteID string) error {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrClipNotFound
	}
	if c.UserID != ownerID {
		return ErrClipNotFound
	}
	_, nerr := s.notes.FindByID(noteID)
	if s.materializeIfMissing(noteID, nerr) {
		_, nerr = s.notes.FindByID(noteID)
	}
	if nerr != nil {
		// **DB 障害を not-found に丸めない** (#2799)。materialize の再試行後も
		// 種別は残るので、ここで分ける。
		if !repository.IsNotFound(nerr) {
			return nerr
		}
		return ErrNoteNotFound
	}
	// **DB 障害で重複チェックを skip しない** (#2799)。`err == nil` だけを見ると、
	// 接続断のときに「まだ clip していない」と判断して二重に足す。
	dup, derr := s.noteRepo.FindByPair(clipID, noteID)
	if derr != nil && !repository.IsNotFound(derr) {
		return derr
	}
	if derr == nil && dup != nil {
		return ErrAlreadyClipped
	}
	// noteEachClipsLimit role policy gate (#1029)。clip 内 note 数の上限を
	// owner の policy で評価する (clip 自体は owner-only mutation なので
	// ownerID と一致する)。
	if s.rolePolicyProvider != nil {
		if limit, ok := role.PolicyNumber(s.rolePolicyProvider.GetUserPolicies(ownerID)["noteEachClipsLimit"]); ok && limit >= 0 {
			count, err := s.noteRepo.CountByClip(clipID)
			if err != nil {
				return err
			}
			if float64(count) >= limit {
				return ErrTooManyClipNotes
			}
		}
	}
	now := s.clock()
	cn := &model.ClipNote{
		ID:     s.idGen.Generate(now),
		ClipID: clipID,
		NoteID: noteID,
	}
	if err := s.noteRepo.Create(cn); err != nil {
		return err
	}
	// clip.notesCount への increment は行わない。同列は upstream に存在せず
	// TS 製 DB では生えないため、件数は CountNotes で clip_note を数える (#2243)。
	// upstream ClipService.addNote:131 は note の clippedCount を increment する
	// (#1768。これまで未維持で常に 0 だった)。
	_ = s.notes.IncrementCount(noteID, "clippedCount", 1)
	_ = s.repo.UpdateFields(clipID, map[string]any{"lastClippedAt": &now})
	return nil
}

// RemoveNote detaches a note from the clip. 非 owner は ErrClipNotFound (#1562)。
func (s *Service) RemoveNote(ownerID, clipID, noteID string) error {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return err
		}
		return ErrClipNotFound
	}
	if c.UserID != ownerID {
		return ErrClipNotFound
	}
	// 本家 2026.10.0 (4682d44cae) の removeNote は note の存在確認をやめ、
	// clip_note の DELETE が 0 行なら NoSuchNote を投げる。note が無い場合も
	// clip に入っていない場合も同じ NO_SUCH_NOTE になる (以前の mk-go は後者を
	// 204 にしていた)。件数は DELETE の RowsAffected で見るので、同じ組への
	// 並行な remove でも clippedCount は 1 回しか減らない。
	deleted, err := s.noteRepo.DeleteByPair(clipID, noteID)
	if err != nil {
		// **DB 障害を「もう入っていない」にしない** (#2799)。NO_SUCH_NOTE に
		// 丸めると、接続断中の remove-note が 4xx になり原因が見えなくなる。
		return err
	}
	if deleted == 0 {
		return ErrNoteNotFound
	}
	// notesCount は非正規化しない (AddNote 側のコメント参照、#2243)。
	// upstream ClipService.removeNote は消せたときだけ clippedCount を decrement する。
	_ = s.notes.IncrementCount(noteID, "clippedCount", -1)
	return nil
}

// CountNotes returns how many notes the clip holds. upstream
// ClipEntityService.pack と同じく clip_note をその場で数える (#2243)。
//
// mk-go は以前 clip.notesCount 列に非正規化していたが、この列は upstream の
// Clip entity に無く TS 製 DB では生えない。加えて TS ↔ mk-go を行き来すると
// TS 側での add/remove がカウンタに反映されず恒久的にずれるため、実カウントに
// 寄せている。notesCount は owner 閲覧時しか出さないので呼び出し頻度も低い。
func (s *Service) CountNotes(clipID string) (int, error) {
	if s.noteRepo == nil {
		return 0, nil
	}
	n, err := s.noteRepo.CountByClip(clipID)
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// Notes returns the notes attached to the clip newest first. requesterID は
// 閲覧者で、private clip は owner 以外には missing と同じ ErrClipNotFound を
// 返して存在を秘匿する (Show と同じ規則、#1562)。search は空白区切り各語を
// note.text / note.cw に ILIKE AND で適用する (upstream notes.ts:103-110)。
func (s *Service) Notes(requesterID, clipID, untilID, sinceID string, limit int, search string) ([]*model.Note, error) {
	c, err := s.repo.FindByID(clipID)
	if err != nil {
		// **DB 障害を not-found に丸めない** (#2799)。
		if !repository.IsNotFound(err) {
			return nil, err
		}
		return nil, ErrClipNotFound
	}
	if !c.IsPublic && c.UserID != requesterID {
		return nil, ErrClipNotFound
	}
	// upstream は search.trim().split(' ')。連続空白が生む空語は ILIKE '%%'
	// となり text/cw が NULL でない note には全一致なので、空語を落とす
	// strings.Fields でほぼ等価になる (tab 等の whitespace も区切る点と、
	// text/cw 両方 NULL の note の扱いのみ微差、実用上の影響なし)。
	searchWords := strings.Fields(search)
	// visibility は repository 側で LIMIT 前に push down する (#1418 review)。
	// clip は author 混在のため post-fetch filter だと per-note の follow 判定
	// N+1 + ページ過少充填になる。
	rows, err := s.noteRepo.ListByClipVisible(clipID, requesterID, untilID, sinceID, limit, searchWords)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.NoteID)
	}
	return s.notes.FindManyByIDsWithUser(ids)
}

// NoteMaterializer promotes a relay-delivered note out of the ephemeral store
// into a real database row (#2332)。実装は core/ephemeral.Materializer。
type NoteMaterializer interface {
	EnsureNote(ctx context.Context, noteID string) (*model.Note, error)
}

// SetNoteMaterializer attaches the ephemeral-note materializer. Optional.
func (s *Service) SetNoteMaterializer(m NoteMaterializer) {
	s.materializer = m
}

// materializeIfMissing promotes an ephemeral note only when the database
// lookup already failed.
//
// 通常のノートでは Redis を一切引かないので、ホットパスに追加コストが無い。
func (s *Service) materializeIfMissing(noteID string, lookupErr error) bool {
	if lookupErr == nil || s.materializer == nil {
		return false
	}
	_, err := s.materializer.EnsureNote(context.Background(), noteID)
	return err == nil
}

// HasRolePolicyProvider reports whether the role policy provider was wired.
//
// 未配線だと `clipLimit` と `noteEachClipsLimit` が両方とも効かない。
// 起動時検査に使う (#2683)。
func (s *Service) HasRolePolicyProvider() bool { return s.rolePolicyProvider != nil }
