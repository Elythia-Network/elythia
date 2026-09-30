package federation

import (
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/model"
)

// QuoteApprovalRemover removes approvals granted by a local author.
type QuoteApprovalRemover interface {
	DeleteByAuthorAndQuoter(authorID, quoterID string) ([]model.NoteQuoteAuthorization, error)
}

// ApprovalDeleteSender delivers the revocation of an approval stamp.
type ApprovalDeleteSender interface {
	SendApprovalDelete(author, quoter *model.User, a *model.NoteQuoteAuthorization) error
}

// NoteUpdateSender re-delivers a local note as an Update.
type NoteUpdateSender interface {
	SendNoteUpdate(note *model.Note, author *model.User) error
}

// QuoteRevoker withdraws the quote approvals a local author granted to someone
// they block (#3234 段階 4、mk-go 独自)。Mastodon はブロックでは取り消さないが、
// 段階 2 でブロック中の QuoteRequest は Reject するので、ブロックする前に出した
// 承認だけが残り続けていた。
//
// 承認の行を先に消す (承認は 404 になる)。配送は best-effort — ブロックの hook は
// 再試行の仕組みを持たないが、相手が承認を取り直せば 404 で失効する。
type QuoteRevoker struct {
	approvals QuoteApprovalRemover
	users     interface {
		FindByID(id string) (*model.User, error)
	}
	notes interface {
		FindByID(id string) (*model.Note, error)
	}
	deletes ApprovalDeleteSender
	updates NoteUpdateSender
	urls    *activitypub.URLBuilder
}

// NewQuoteRevoker constructs a QuoteRevoker.
func NewQuoteRevoker(approvals QuoteApprovalRemover, users interface {
	FindByID(id string) (*model.User, error)
}, notes interface {
	FindByID(id string) (*model.Note, error)
}, deletes ApprovalDeleteSender, updates NoteUpdateSender, urls *activitypub.URLBuilder) *QuoteRevoker {
	return &QuoteRevoker{approvals: approvals, users: users, notes: notes, deletes: deletes, updates: updates, urls: urls}
}

// RevokeQuotesOnBlock withdraws the approvals blockerID gave to blockeeID's
// quotes. 取り消すのは作者がブロックしたときだけ (承認を出したのは作者)。
func (r *QuoteRevoker) RevokeQuotesOnBlock(blockerID, blockeeID string) {
	author, err := r.users.FindByID(blockerID)
	if err != nil || !author.IsLocal() {
		if err != nil {
			slog.Warn("quote revocation: find blocker", "blocker", blockerID, "err", err)
		}
		return
	}
	rows, err := r.approvals.DeleteByAuthorAndQuoter(blockerID, blockeeID)
	if err != nil {
		slog.Warn("quote revocation: remove approvals", "blocker", blockerID, "blockee", blockeeID, "err", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	quoter, err := r.users.FindByID(blockeeID)
	if err != nil {
		slog.Warn("quote revocation: find blockee", "blockee", blockeeID, "err", err)
		return
	}
	for i := range rows {
		a := &rows[i]
		if err := r.deletes.SendApprovalDelete(author, quoter, a); err != nil {
			slog.Warn("quote revocation: send delete", "approval", a.ID, "err", err)
		}
		// 引用する投稿もこちらのものなら、承認を外した Update を配り直す (第三者は
		// 承認の抜けた Update を受けて引用を未承認に戻す)。リモートの相手の投稿は
		// 相手のサーバーが配り直す。
		noteID, ok := strings.CutPrefix(a.QuotingURI, r.urls.NoteURI(""))
		if !ok {
			continue
		}
		note, err := r.notes.FindByID(noteID)
		if err != nil {
			slog.Warn("quote revocation: find quoting note", "noteId", noteID, "err", err)
			continue
		}
		// 相手本人の投稿だけ (他人の投稿を相手の名前で配り直さない)。
		if note.UserID != quoter.ID {
			continue
		}
		if err := r.updates.SendNoteUpdate(note, quoter); err != nil {
			slog.Warn("quote revocation: send update", "noteId", noteID, "err", err)
		}
	}
}

// SendApprovalDelete implements ApprovalDeleteSender: the Delete goes to the
// author's followers and, for a remote quoter, to the quoter's inbox.
// 引用した相手のサーバーは引用する投稿の Update を自分で配り直す (Mastodon の
// Delete#revoke_quote)。作者のフォロワーは、引用される投稿を見て引用を知った相手。
func (h *QuoteRequestDeliveryHook) SendApprovalDelete(author, quoter *model.User, a *model.NoteQuoteAuthorization) error {
	body, err := json.Marshal(h.renderer.RenderQuoteAuthorizationDelete(&model.Note{ID: a.NoteID, UserID: author.ID}, a))
	if err != nil {
		return err
	}
	// 引用を書き換えるのは相手のサーバーなので、相手へ先に送る (フォロワーへの
	// 送信が落ちても相手には届く)。ローカルの相手には送らない (DeliverToUser が
	// ローカルを飛ばす)。フォロワーへの送信では相手の inbox を除く (同じ inbox へ
	// 二度 POST しない、#2567)。
	if err := h.deliver.DeliverToUser(author.ID, quoter, body); err != nil {
		return err
	}
	var exclude map[string]bool
	if !quoter.IsLocal() {
		exclude = map[string]bool{preferredInbox(quoter): true}
	}
	return h.deliver.DeliverToFollowersExcluding(author.ID, body, exclude)
}
