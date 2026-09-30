package federation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// QuoteRequestStore records the quote requests local notes send (#3234).
type QuoteRequestStore interface {
	FindByNoteID(noteID string) (*model.NoteQuoteRequest, error)
	FindByRequestURI(requestURI string) (*model.NoteQuoteRequest, error)
	Ensure(noteID, requestURI string) (*model.NoteQuoteRequest, error)
	MarkAccepted(noteID, approvalURI string) (bool, error)
	MarkUpdateSent(noteID, approvalURI string) error
	MarkRejected(noteID string) error
}

// QuoteApprovalLookup finds an approval granted to a quoting note.
type QuoteApprovalLookup interface {
	FindByNoteIDAndQuotingURI(noteID, quotingURI string) (*model.NoteQuoteAuthorization, error)
}

// QuoteOutboxDelivery sends what the quote outbox produces.
type QuoteOutboxDelivery interface {
	SendQuoteRequest(note *model.Note, quotedURI string, quotedAuthor *model.User) error
	SendNoteUpdate(note *model.Note, author *model.User) error
}

// QuoteOutbox obtains FEP-044f approvals for quotes written by local users
// (#3234 段階 3):
//   - ローカル同士の引用は、段階 2 と同じ判定で自分で承認を発行する
//   - リモートの投稿の引用は、作者へ QuoteRequest を送り、返ってきた承認を
//     `quoteAuthorization` に入れた Update を配り直す
//
// 第三者 (Mastodon) は `quoteAuthorization` を取得して確かめてから引用を表示する。
type QuoteOutbox struct {
	decider   *QuoteRequestHandler
	requests  QuoteRequestStore
	approvals QuoteApprovalLookup
	deliver   QuoteOutboxDelivery
}

// NewQuoteOutbox constructs a QuoteOutbox. decider supplies the note / user
// lookups, the approval store and the rules shared with incoming requests.
func NewQuoteOutbox(decider *QuoteRequestHandler, requests QuoteRequestStore, approvals QuoteApprovalLookup, deliver QuoteOutboxDelivery) *QuoteOutbox {
	return &QuoteOutbox{decider: decider, requests: requests, approvals: approvals, deliver: deliver}
}

// quoteTarget returns the note quoted by note when note will be delivered as a
// quote that asks for approval, or nil.
//
// ダイレクトとローカル限定の引用は対象にしない。前者は承認の実体 (公開で配る)
// から宛先限定の投稿の URI が漏れ、後者はそもそも連合しない。
func (o *QuoteOutbox) quoteTarget(note *model.Note) (*model.Note, error) {
	if note == nil || !activitypub.IsQuote(note) || note.LocalOnly {
		return nil, nil
	}
	switch note.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome, model.NoteVisibilityFollowers:
	default:
		return nil, nil
	}
	target, err := o.decider.notes.FindByID(*note.RenoteID)
	if repository.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("quote outbox: find quoted note: %w", err)
	}
	if isPureRenote(target) {
		return nil, nil
	}
	return target, nil
}

// Prepare issues the approval for a local quote of another local user's note.
// Create を描画する前に呼ぶ (最初の Create から `quoteAuthorization` が付く)。
func (o *QuoteOutbox) Prepare(note *model.Note, author *model.User) error {
	target, err := o.quoteTarget(note)
	if err != nil || target == nil {
		return err
	}
	// 自分の投稿の引用は承認が要らない (FEP-044f。Mastodon も承認なしで通す)。
	if target.UserHost != nil || target.UserID == note.UserID {
		return nil
	}
	decision, err := o.decider.decide(target, author)
	if err != nil {
		return err
	}
	if decision != quoteAllow {
		return nil
	}
	_, err = o.decider.approvals.Ensure(&model.NoteQuoteAuthorization{
		ID:         o.decider.idGen.Generate(o.decider.now()),
		NoteID:     target.ID,
		QuoterID:   author.ID,
		QuotingURI: o.decider.urls.NoteURI(note.ID),
	})
	if err != nil {
		return fmt.Errorf("quote outbox: record local approval: %w", err)
	}
	return nil
}

// RequestApproval sends a QuoteRequest to the author of the remote note quoted
// by note. Create の配送の後に呼ぶ。
func (o *QuoteOutbox) RequestApproval(note *model.Note, author *model.User) error {
	target, err := o.quoteTarget(note)
	if err != nil || target == nil || target.UserHost == nil {
		return err
	}
	if target.URI == nil || *target.URI == "" {
		return nil
	}
	quotedAuthor, err := o.decider.users.FindByID(target.UserID)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote outbox: find quoted author: %w", err)
	}
	// 返ってくる Accept は id で照合するので、送る前に記録する。
	if _, err := o.requests.Ensure(note.ID, o.decider.urls.QuoteRequestURI(note.ID)); err != nil {
		return fmt.Errorf("quote outbox: record request: %w", err)
	}
	if err := o.deliver.SendQuoteRequest(note, *target.URI, quotedAuthor); err != nil {
		return fmt.Errorf("quote outbox: send request: %w", err)
	}
	return nil
}

// ApprovalURI returns the `quoteAuthorization` of a local quoting note, or ""
// when it has none. renderer の resolver として使う。DB 障害は error で返す —
// 描画側が「承認なしで描画してよいか」(取得・Create) と「作らずに再試行するか」
// (承認を配り直す Update) を選ぶ。
func (o *QuoteOutbox) ApprovalURI(note *model.Note) (string, error) {
	if note == nil || !activitypub.IsQuote(note) {
		return "", nil
	}
	target, err := o.decider.notes.FindByID(*note.RenoteID)
	if repository.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("quote approval: find quoted note: %w", err)
	}
	if target.UserHost != nil {
		req, err := o.requests.FindByNoteID(note.ID)
		if repository.IsNotFound(err) {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("quote approval: find request: %w", err)
		}
		if req.State == model.QuoteRequestAccepted && req.ApprovalURI != nil {
			return *req.ApprovalURI, nil
		}
		return "", nil
	}
	// 自分の投稿の引用には承認を作らない (Prepare) ので、引いても見つからない。
	a, err := o.approvals.FindByNoteIDAndQuotingURI(target.ID, o.decider.urls.NoteURI(note.ID))
	if repository.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("quote approval: find local approval: %w", err)
	}
	return o.decider.urls.QuoteAuthorizationURI(target.ID, a.ID), nil
}

// HandleAnswer records the Accept (with result = approval URI) or Reject that
// actorURI sent for one of our QuoteRequests, identified by requestURI.
// 承認されたら、承認を付けた Update を配り直す。届けられなければ error で返し、
// inbox に再試行させる (承認の記録は冪等)。
func (o *QuoteOutbox) HandleAnswer(actorURI, requestURI string, accepted bool, result string) error {
	req, err := o.requests.FindByRequestURI(requestURI)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote answer: find request: %w", err)
	}
	note, err := o.decider.notes.FindByID(req.NoteID)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote answer: find note: %w", err)
	}
	if note.RenoteID == nil {
		return nil
	}
	target, err := o.decider.notes.FindByID(*note.RenoteID)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote answer: find quoted note: %w", err)
	}
	quotedAuthor, err := o.decider.users.FindByID(target.UserID)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote answer: find quoted author: %w", err)
	}
	// 答えられるのは引用される投稿の作者だけ。他人の Accept で承認済みにさせない。
	if quotedAuthor.URI == nil || *quotedAuthor.URI != actorURI {
		return nil
	}
	if !accepted {
		if err := o.requests.MarkRejected(note.ID); err != nil {
			return fmt.Errorf("quote answer: record reject: %w", err)
		}
		return nil
	}
	// 承認の URI は作者のホストのものに限る (Mastodon の Accept#accept_quote! と
	// 同じ)。第三者はこれを取りに行くので、別ホストを指させない。
	if !validApprovalURI(result, actorURI) {
		return nil
	}
	quoter, err := o.decider.users.FindByID(note.UserID)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote answer: find quoter: %w", err)
	}
	// 配り直すのは、その承認を付けた Update をまだ配り終えていないときだけ。同じ
	// Accept が何度届いても、その度にフォロワー全員へ送らない (Mastodon も pending
	// のときしか受けない)。
	//
	// **失敗しても承認の記録は消さない。** 「未配信」の印だけで再試行を表す。記録を
	// 戻す形にすると、失敗と重なって届いた同じ Accept が「変化なし」で成功扱いに
	// なり、inbox の重複除けが再試行を捨てて承認ごと失われる (レビュー 2 周目)。
	needSend, err := o.requests.MarkAccepted(note.ID, result)
	if err != nil {
		return fmt.Errorf("quote answer: record accept: %w", err)
	}
	if !needSend {
		return nil
	}
	if err := o.deliver.SendNoteUpdate(note, quoter); err != nil {
		return fmt.Errorf("quote answer: send update: %w", err)
	}
	if err := o.requests.MarkUpdateSent(note.ID, result); err != nil {
		// 送れてはいるので再試行はさせない (次に同じ Accept が届けば送り直すだけ)。
		slog.Warn("quote answer: record update delivery", "noteId", note.ID, "err", err)
	}
	return nil
}

func validApprovalURI(uri, actorURI string) bool {
	if !colfit.Fits(uri, quoteURIMaxRunes) {
		return false
	}
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	// 相手が申告した値なので、`www.` を同一視しない厳密な比較を使う
	// (resolver.go の sameDeliveryHost の doc)。Mastodon もホストをそのまま比べる。
	return sameDeliveryHost(uri, actorURI)
}

// quoteAnswerRequestURI returns the QuoteRequest id an Accept / Reject answers,
// or "" when the object is not one of ours.
//
// object は埋め込み (type が QuoteRequest) でも、id だけの文字列でも来る。文字列の
// ときは形 (`<base>/notes/<id>#quote-request`) で見分ける。
func quoteAnswerRequestURI(object json.RawMessage, localBaseURL string) string {
	var s string
	if json.Unmarshal(object, &s) == nil {
		if strings.HasPrefix(s, localBaseURL+"/notes/") && strings.HasSuffix(s, "#quote-request") {
			return s
		}
		return ""
	}
	var inner struct {
		ID   string          `json:"id"`
		Type json.RawMessage `json:"type"`
	}
	if json.Unmarshal(object, &inner) != nil {
		return ""
	}
	var types []string
	var one string
	if json.Unmarshal(inner.Type, &one) == nil {
		types = []string{one}
	} else {
		_ = json.Unmarshal(inner.Type, &types)
	}
	for _, t := range types {
		if strings.EqualFold(t, "QuoteRequest") || strings.EqualFold(t, "https://w3id.org/fep/044f#QuoteRequest") {
			return inner.ID
		}
	}
	return ""
}

// quoteAnswerResult reads the `result` (the approval URI) of an Accept. 配列なら
// 先頭を使う (Mastodon の first_of_value と同じ)。
func quoteAnswerResult(raw json.RawMessage) string {
	var act struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(raw, &act) != nil {
		return ""
	}
	var list []json.RawMessage
	if json.Unmarshal(act.Result, &list) == nil {
		if len(list) == 0 {
			return ""
		}
		return refID(list[0])
	}
	return refID(act.Result)
}
