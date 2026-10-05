package ephemeral

import (
	"context"
	"errors"
	"fmt"

	"github.com/elythia-network/elythia/internal/model"
)

// ErrNoteNotFound is returned when a note exists neither in the database nor
// in the ephemeral store (typically because its TTL elapsed).
var ErrNoteNotFound = errors.New("ephemeral: note not found")

// NoteWriter is the database side of materialization. Implemented by
// repository.NoteRepository.
type NoteWriter interface {
	FindByID(id string) (*model.Note, error)
	Create(n *model.Note) error
}

// ActorMaterializer creates the database row for a remote actor, **reusing the
// ID the ephemeral store already assigned**. Implemented by
// federation.Resolver (MaterializeActor).
//
// ここを新規採番に任せてはいけない。ミュートで著者を materialize したときに
// ID が変わると、Redis に残っている既存ノートが古い ID を指したままになり、
// ミュートしたのにタイムラインから消えない状態が TTL 切れまで続く。
type ActorMaterializer interface {
	MaterializeActor(uri, preassignedID string) (*model.User, error)
}

// Materializer promotes ephemeral notes into real database rows.
//
// 呼ぶのは「これから外部キーで参照する」場面に限る (#2332)。`note.replyId` /
// `note_reaction.noteId` などは挿入時に参照先の実在を要求するため、親を DB に
// 起こさないと INSERT 自体が失敗する。逆に閲覧では呼ばない。リンクを踏まれる
// たびに永続化されると、DB を膨らませない目的が崩れる。
type Materializer struct {
	store *Store
	notes NoteWriter
	actor ActorMaterializer
}

// NewMaterializer constructs a Materializer. store が nil なら EnsureNote は
// 単なる DB 参照に縮退する (機能未使用時)。
func NewMaterializer(store *Store, notes NoteWriter, actor ActorMaterializer) *Materializer {
	return &Materializer{store: store, notes: notes, actor: actor}
}

// EnsureNote returns the database row for noteID, promoting it out of the
// ephemeral store when necessary.
//
// 既に DB にある通常のノートでは Redis を一切引かないので、ホットパスの追加
// コストは無い。
func (m *Materializer) EnsureNote(ctx context.Context, noteID string) (*model.Note, error) {
	if m == nil || m.notes == nil || noteID == "" {
		return nil, ErrNoteNotFound
	}
	if n, err := m.notes.FindByID(noteID); err == nil && n != nil {
		return n, nil
	}
	if m.store == nil {
		return nil, ErrNoteNotFound
	}

	eph, err := m.store.GetNote(ctx, noteID)
	if err != nil {
		return nil, err
	}
	if eph == nil {
		return nil, ErrNoteNotFound
	}

	// 著者を先に起こす。note.userId は user への外部キーなので、著者が居ない
	// 状態でノートを INSERT すると失敗する。
	if err := m.ensureAuthorOf(eph); err != nil {
		return nil, err
	}

	// ID は ephemeral 時のものをそのまま使う。FTT のリストやストリーミングで
	// 配った ID が有効であり続ける。
	stored := *eph
	stored.User = nil
	if err := m.notes.Create(&stored); err != nil {
		// 競合で先に別経路が作っていたら、それを正として返す。
		if existing, ferr := m.notes.FindByID(noteID); ferr == nil && existing != nil {
			return existing, nil
		}
		return nil, fmt.Errorf("ephemeral: materialize note %s: %w", noteID, err)
	}
	// DB が正になったので ephemeral 側は落とす。残すと timeline の合成で
	// 二重に出る。
	uri := ""
	if eph.URI != nil {
		uri = *eph.URI
	}
	_ = m.store.DropNote(ctx, noteID, uri)

	stored.User = eph.User
	return &stored, nil
}

// EnsureUser returns the database row for userID, promoting the ephemeral
// author when necessary.
//
// ノートを伴わない契機もある。ミュート (`muting.muteeId`) / ブロック
// (`blocking.blockeeId`) / 通報 (`abuse_user_report.targetUserId`) はいずれも
// user への外部キーだけを要求し、ノート行は要らない。
func (m *Materializer) EnsureUser(ctx context.Context, userID string) (*model.User, error) {
	if m == nil || m.store == nil || userID == "" {
		return nil, ErrNoteNotFound
	}
	eph, err := m.store.GetUser(ctx, userID)
	if err != nil || eph == nil {
		return nil, ErrNoteNotFound
	}
	return m.materializeActor(eph)
}

// ensureAuthorOf materializes the author of an ephemeral note.
func (m *Materializer) ensureAuthorOf(n *model.Note) error {
	if n.User == nil || n.User.URI == nil || *n.User.URI == "" {
		// 著者を引けないノートは起こせない。TTL 切れで著者だけ落ちた場合など。
		return ErrNoteNotFound
	}
	_, err := m.materializeActor(n.User)
	return err
}

// materializeActor creates the actor row keeping its ephemeral ID.
//
// Redis の model.User をそのまま INSERT しない。`user_publickey` が無い remote
// user が DB に生まれ、以後の署名検証で鍵が引けなくなる。materialize は誰かが
// 触ったときだけなので頻度が低く、actor を fetch し直して鍵ごと正規に作る方が
// 安全。**ただし ID は据え置く。**
func (m *Materializer) materializeActor(u *model.User) (*model.User, error) {
	if m.actor == nil || u == nil || u.URI == nil || *u.URI == "" {
		return nil, ErrNoteNotFound
	}
	return m.actor.MaterializeActor(*u.URI, u.ID)
}
