package federation

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// QuoteRequestStore records the quote requests local notes send (#3234).
type QuoteRequestStore interface {
	FindByNoteID(noteID string) (*model.NoteQuoteRequest, error)
	FindByRequestURI(requestURI string) (*model.NoteQuoteRequest, error)
	Ensure(noteID, requestURI string, nextResendAt time.Time) (*model.NoteQuoteRequest, error)
	DueResends(now time.Time, limit int) ([]model.NoteQuoteRequest, error)
	ClaimResend(noteID string, due time.Time, next *time.Time) (bool, error)
	ListByApprovalURI(approvalURI string, limit int) ([]model.NoteQuoteRequest, error)
	MarkAccepted(noteID, approvalURI string) (bool, error)
	MarkRevoked(noteID string) (bool, error)
	MarkUpdateSent(noteID, state, approvalURI string) error
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
	if _, err := o.requests.Ensure(note.ID, o.decider.urls.QuoteRequestURI(note.ID), o.decider.now().Add(quoteRequestResendAfter[0])); err != nil {
		return fmt.Errorf("quote outbox: record request: %w", err)
	}
	if err := o.deliver.SendQuoteRequest(note, *target.URI, quotedAuthor); err != nil {
		return fmt.Errorf("quote outbox: send request: %w", err)
	}
	return nil
}

// quoteRequestResendAfter are when (after the first send) a still-pending
// QuoteRequest is sent again (#3238)。
//
// Mastodon は、引用する投稿を別の経路 (相手のサーバーにいるフォロワー宛ての
// Create・検索・閲覧) で取り込んでいる最中に QuoteRequest が届くと、引用先が結び
// 付く前の記録で照合して黙って捨てる (再試行もしない)。同じ id で送り直せば取り込みの
// 後に照合される。**送り直すのは保留中のものだけ** — Mastodon は QuoteRequest を
// 受けると引用の状態を見ずに承認し直すので、承認・拒否・取り消しの後に送ると
// 取り消された引用を承認済みに戻してしまう (#3237 の敵対的レビュー)。
var quoteRequestResendAfter = []time.Duration{time.Minute, 10 * time.Minute}

// quoteRequestResendTolerance is how late a resend may still be sent. それより
// 遅れた枠 (mk-go や定期処理が止まっていた間の分) は送らずに使い切る —
// 送り直しは取り込みとの競合を拾うためのもので、遅れて送っても意味が無く、
// その間に相手側で承認・取り消しが済んでいれば取り消しを元に戻しうる。
const quoteRequestResendTolerance = 3 * time.Minute

// quoteRequestResendBatch bounds how many resends one run takes.
const quoteRequestResendBatch = 100

// ResendPending sends again the QuoteRequests that are still pending and due
// (#3238)。毎分の定期処理から呼ぶ。送った件数を返す。
//
// 行を取り分けてから送る (取り分けは「保留中で予定が変わっていない」ときだけ成功する)
// ので、複数の worker が同時に走っても二重に送らない。送る前に、投稿がまだ引用で、
// 公開範囲が対象かを確かめ直す。1 件の失敗で残りを止めない (取り分けた分は次の予定へ
// 進むので、失敗した回は送り直さない)。
func (o *QuoteOutbox) ResendPending(now time.Time) (int, error) {
	due, err := o.requests.DueResends(now, quoteRequestResendBatch)
	if err != nil {
		return 0, fmt.Errorf("quote resend: find due: %w", err)
	}
	sent := 0
	for _, req := range due {
		if req.NextResendAt == nil {
			continue
		}
		// 次の枠は作成からの予定どおりに置く (遅れて走っても予定をずらさない)。
		var next *time.Time
		if n := req.ResendCount + 1; n < len(quoteRequestResendAfter) {
			t := req.NextResendAt.Add(quoteRequestResendAfter[n] - quoteRequestResendAfter[n-1])
			next = &t
		}
		claimed, err := o.requests.ClaimResend(req.NoteID, *req.NextResendAt, next)
		if err != nil {
			slog.Warn("quote resend: claim", "noteId", req.NoteID, "err", err)
			continue
		}
		if !claimed || now.Sub(*req.NextResendAt) > quoteRequestResendTolerance {
			continue
		}
		ok, err := o.resend(req.NoteID)
		if err != nil {
			slog.Warn("quote resend: send", "noteId", req.NoteID, "err", err)
			continue
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

// resend sends the QuoteRequest of the quoting note again, if it still quotes
// a remote note it may ask about, and reports whether it sent.
func (o *QuoteOutbox) resend(noteID string) (bool, error) {
	note, err := o.decider.notes.FindByID(noteID)
	if repository.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	target, err := o.quoteTarget(note)
	if err != nil || target == nil || target.UserHost == nil || target.URI == nil || *target.URI == "" {
		return false, err
	}
	// 引用した本人がその後に凍結・削除されていたら送らない。
	quoter, err := o.decider.users.FindByID(note.UserID)
	if repository.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if quoter.IsSuspended || quoter.IsDeleted {
		return false, nil
	}
	quotedAuthor, err := o.decider.users.FindByID(target.UserID)
	if repository.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := o.deliver.SendQuoteRequest(note, *target.URI, quotedAuthor); err != nil {
		return false, err
	}
	return true, nil
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
// inbox に再試行させる (承認の記録は冪等)。承認の後に届いた Reject は取り消し
// として扱う (Mastodon の Reject#reject_quote! と同じ)。
func (o *QuoteOutbox) HandleAnswer(actorURI, requestURI string, accepted bool, result string) error {
	req, err := o.requests.FindByRequestURI(requestURI)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote answer: find request: %w", err)
	}
	note, quoter, ok, err := o.answerTarget(req, actorURI)
	if err != nil || !ok {
		return err
	}
	if !accepted {
		if err := o.requests.MarkRejected(note.ID); err != nil {
			return fmt.Errorf("quote answer: record reject: %w", err)
		}
		if req.ApprovalURI == nil {
			return nil
		}
		return o.revoke(note, quoter, *req.ApprovalURI)
	}
	// 承認の URI は作者のホストのものに限る (Mastodon の Accept#accept_quote! と
	// 同じ)。第三者はこれを取りに行くので、別ホストを指させない。
	if !validApprovalURI(result, actorURI) {
		return nil
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
	return o.sendUpdate(note, quoter, model.QuoteRequestAccepted, result)
}

// HandleRevocation withdraws our quote's approval when its author deletes it
// (a Delete whose object is the approval, FEP-044f). 自分の引用の承認でなければ
// handled = false を返し、呼び出し側は通常の Delete として扱う。
func (o *QuoteOutbox) HandleRevocation(actorURI, approvalURI string) (bool, error) {
	reqs, err := o.requests.ListByApprovalURI(approvalURI, quoteRevocationMatchLimit)
	if err != nil {
		return false, fmt.Errorf("quote revocation: find request: %w", err)
	}
	handled := false
	for i := range reqs {
		note, quoter, ok, err := o.answerTarget(&reqs[i], actorURI)
		if err != nil {
			return true, err
		}
		// 照合できなければ通常の Delete として扱わせる。承認の URI は相手のホストの
		// 任意の URI でありうるので、たまたま一致した別の Delete (アカウントやノートの
		// 削除) を飲み込まない。
		if !ok {
			continue
		}
		handled = true
		if err := o.revoke(note, quoter, approvalURI); err != nil {
			return true, err
		}
	}
	return handled, nil
}

// quoteRevocationMatchLimit bounds how many requests one Delete is matched
// against. 正当な承認 URI は引用 1 つにつき 1 つなので、普通は 1 件。
const quoteRevocationMatchLimit = 20

// revoke marks the request revoked and re-delivers the note without the
// approval. 第三者 (Mastodon) は承認の抜けた Update を受けて、引用を未承認に戻す。
func (o *QuoteOutbox) revoke(note *model.Note, quoter *model.User, approvalURI string) error {
	needSend, err := o.requests.MarkRevoked(note.ID)
	if err != nil {
		return fmt.Errorf("quote revocation: record: %w", err)
	}
	if !needSend {
		return nil
	}
	return o.sendUpdate(note, quoter, model.QuoteRequestRevoked, approvalURI)
}

// sendUpdate re-delivers note and records that the Update for (state, approval)
// went out. 失敗は error で返して inbox に再試行させる (記録は「未配信」のまま)。
func (o *QuoteOutbox) sendUpdate(note *model.Note, quoter *model.User, state, approvalURI string) error {
	if err := o.deliver.SendNoteUpdate(note, quoter); err != nil {
		return fmt.Errorf("quote answer: send update: %w", err)
	}
	if err := o.requests.MarkUpdateSent(note.ID, state, approvalURI); err != nil {
		// 送れてはいるので再試行はさせない (次に同じ答えが届けば送り直すだけ)。
		slog.Warn("quote answer: record update delivery", "noteId", note.ID, "err", err)
	}
	return nil
}

// answerTarget loads the quoting note and its author for an answer to req and
// checks that actorURI is the author of the quoted note. 答えられるのは引用される
// 投稿の作者だけ (他人の Accept / Reject / Delete で状態を変えさせない)。「無い」と
// 作者の不一致は ok = false、DB 障害は error。
func (o *QuoteOutbox) answerTarget(req *model.NoteQuoteRequest, actorURI string) (*model.Note, *model.User, bool, error) {
	note, err := o.decider.notes.FindByID(req.NoteID)
	if repository.IsNotFound(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("quote answer: find note: %w", err)
	}
	if note.RenoteID == nil {
		return nil, nil, false, nil
	}
	target, err := o.decider.notes.FindByID(*note.RenoteID)
	if repository.IsNotFound(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("quote answer: find quoted note: %w", err)
	}
	quotedAuthor, err := o.decider.users.FindByID(target.UserID)
	if repository.IsNotFound(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("quote answer: find quoted author: %w", err)
	}
	if quotedAuthor.URI == nil || *quotedAuthor.URI != actorURI {
		return nil, nil, false, nil
	}
	quoter, err := o.decider.users.FindByID(note.UserID)
	if repository.IsNotFound(err) {
		return nil, nil, false, nil
	}
	if err != nil {
		return nil, nil, false, fmt.Errorf("quote answer: find quoter: %w", err)
	}
	return note, quoter, true, nil
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
	if json.Unmarshal(object, &inner) != nil || !apTypeIs(inner.Type, "QuoteRequest") {
		return ""
	}
	return inner.ID
}

// apTypeIs reports whether an AP `type` (a string or an array) includes the
// FEP-044f term name, compact (`QuoteRequest`) or expanded
// (`https://w3id.org/fep/044f#QuoteRequest`, JSON-LD を compact できなかったとき)。
func apTypeIs(raw json.RawMessage, name string) bool {
	var types []string
	var one string
	if json.Unmarshal(raw, &one) == nil {
		types = []string{one}
	} else {
		_ = json.Unmarshal(raw, &types)
	}
	for _, t := range types {
		if strings.EqualFold(t, name) || strings.EqualFold(t, "https://w3id.org/fep/044f#"+name) {
			return true
		}
	}
	return false
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
