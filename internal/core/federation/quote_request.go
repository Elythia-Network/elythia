package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/misc/colfit"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

// QuoteApprovalStore records quote approvals granted by local authors.
type QuoteApprovalStore interface {
	Ensure(a *model.NoteQuoteAuthorization) (*model.NoteQuoteAuthorization, error)
	Remove(noteID, quotingURI, quoterID string) error
}

// QuoteRequestResponder sends the answer to a QuoteRequest back to the
// requesting remote actor.
type QuoteRequestResponder interface {
	SendQuoteAccept(note *model.Note, approval *model.NoteQuoteAuthorization, quoter *model.User) error
	SendQuoteReject(note *model.Note, requestID, quotingURI string, quoter *model.User) error
}

// QuoteRequestHandler answers FEP-044f QuoteRequest activities for local
// notes (#3234): it decides whether the remote actor may quote the note,
// records the approval, and replies with Accept (carrying the approval URI)
// or Reject.
//
// 承認は自動で行う (手動承認は持たない)。判断は投稿の公開範囲とブロックだけで、
// 利用者が範囲を選ぶ設定はまだ無い。
type QuoteRequestHandler struct {
	notes interface {
		FindByID(id string) (*model.Note, error)
	}
	users interface {
		FindByID(id string) (*model.User, error)
	}
	blocks interface {
		IsBlocked(blockerID, blockeeID string) (bool, error)
	}
	follows interface {
		IsFollowing(followerID, followeeID string) (bool, error)
	}
	approvals QuoteApprovalStore
	fetchNote func(uri string) (*activitypub.Note, error)
	respond   QuoteRequestResponder
	urls      *activitypub.URLBuilder
	idGen     id.Generator
	now       func() time.Time
}

// QuoteRequestDeps groups the dependencies of NewQuoteRequestHandler.
type QuoteRequestDeps struct {
	Notes interface {
		FindByID(id string) (*model.Note, error)
	}
	Users interface {
		FindByID(id string) (*model.User, error)
	}
	Blocks interface {
		IsBlocked(blockerID, blockeeID string) (bool, error)
	}
	Follows interface {
		IsFollowing(followerID, followeeID string) (bool, error)
	}
	Approvals QuoteApprovalStore
	FetchNote func(uri string) (*activitypub.Note, error)
	Respond   QuoteRequestResponder
	URLs      *activitypub.URLBuilder
	IDGen     id.Generator
}

// NewQuoteRequestHandler constructs a QuoteRequestHandler.
func NewQuoteRequestHandler(d QuoteRequestDeps) *QuoteRequestHandler {
	return &QuoteRequestHandler{
		notes: d.Notes, users: d.Users, blocks: d.Blocks, follows: d.Follows,
		approvals: d.Approvals, fetchNote: d.FetchNote, respond: d.Respond,
		urls: d.URLs, idGen: d.IDGen, now: time.Now,
	}
}

type quoteRequestActivity struct {
	ID         string          `json:"id"`
	Actor      string          `json:"actor"`
	Object     json.RawMessage `json:"object"`
	Instrument json.RawMessage `json:"instrument"`
}

// Handle processes a QuoteRequest from quoter. raw is the whole activity.
// 一時的な失敗 (DB・instrument の取得) は error で返し、inbox の再試行に任せる。
// 承認できないものは Reject を返し、こちらの投稿でないものは黙って捨てる。
func (h *QuoteRequestHandler) Handle(quoter *model.User, raw json.RawMessage) error {
	if quoter == nil || quoter.IsLocal() || quoter.URI == nil {
		return nil
	}
	var req quoteRequestActivity
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil
	}
	objectURI := refID(req.Object)
	noteID, ok := h.localNoteID(objectURI)
	if !ok {
		return nil
	}
	note, err := h.notes.FindByID(noteID)
	if repository.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("quote request: find note: %w", err)
	}
	// ただの renote は引用の対象にならない (Mastodon も reblog は答えない)。
	if note.UserHost != nil || isPureRenote(note) {
		return nil
	}
	quotingURI := refID(req.Instrument)
	if quotingURI == "" {
		return nil
	}

	// 列に入らない値は記録できない (INSERT がクエリごと落ちる)。一時的な失敗として
	// 再試行させると、同じ失敗を繰り返すだけなので答えずに捨てる。
	if !colfit.Fits(quotingURI, quoteURIMaxRunes) || !colfit.Fits(req.ID, quoteURIMaxRunes) {
		return nil
	}

	decision, err := h.decide(note, quoter)
	if err != nil {
		return err
	}
	if decision == quoteIgnore {
		return nil
	}
	if decision == quoteAllow {
		ok, err := h.instrumentQuotes(req.Instrument, quotingURI, quoter, h.urls.NoteURI(note.ID))
		if err != nil {
			return err
		}
		// 確かめられない引用には答えない (Mastodon も sanity check に落ちたら
		// 黙って捨てる)。Reject にすると、引用する投稿を inline で送らない実装の
		// 正当な引用 (取得が 401 / 404 になるもの) を恒久的に拒否してしまう。
		if !ok {
			return nil
		}
	}
	if decision == quoteReject {
		return h.reject(note, req.ID, quotingURI, quoter)
	}

	var requestID *string
	if req.ID != "" {
		requestID = &req.ID
	}
	approval, err := h.approvals.Ensure(&model.NoteQuoteAuthorization{
		ID:         h.idGen.Generate(h.now()),
		NoteID:     note.ID,
		QuoterID:   quoter.ID,
		QuotingURI: quotingURI,
		RequestID:  requestID,
	})
	if err != nil {
		return fmt.Errorf("quote request: record approval: %w", err)
	}
	// **記録が別の相手のものなら答えない (#3239)。** 記録は (投稿, 引用 URI) で
	// 一意なので、同じホストの別人が先にその引用 URI を名乗っていると、Ensure は
	// その人の行を返す。答えると、その人の承認をこの相手へ渡すことになり、ブロック
	// による取り消しも相手を取り違える。名乗れるのは相手のサーバーの管理者だけ
	// (inline の instrument は actor と同じホストのときだけ信じる) なので、記録を
	// 書き換えずに黙って捨てる。記録は名乗った側のまま残るので、作者が本物の相手を
	// ブロックしてもその記録は取り消されない — 名乗った側をブロックすれば消える。
	// 相手のサーバーの管理者はそもそも本物の相手を騙れるので、脅威の範囲は変わらない。
	if approval.QuoterID != quoter.ID {
		slog.Warn("quote request: approval is recorded for another quoter",
			"noteId", note.ID, "quotingUri", quotingURI, "quoter", quoter.ID, "recorded", approval.QuoterID)
		return nil
	}
	// 記録した後でブロックを確かめ直す (#3234 段階 4)。作者のブロックは「ブロックを
	// 記録 → 承認を消す」の順に進むので、判定と記録の間にブロックされると、消した
	// 後に承認ができてしまう。記録の後に確かめれば、そのブロックは見える。
	// **確かめ直した後にブロックされる窓は残る** — そのときは消された承認の Accept を
	// 送ることになるが、承認は 404 なので第三者は確かめて退ける。
	blocked, err := h.blockedBetween(note.UserID, quoter.ID)
	if err != nil {
		return err
	}
	if blocked {
		return h.reject(note, req.ID, quotingURI, quoter)
	}
	// 再送された QuoteRequest には、最初の承認をそのまま返す (同じ承認 URI)。
	// ただし Accept の object は今回の QuoteRequest の id を指す (相手は id で
	// 自分の引用を引き当てる)。
	if requestID != nil {
		approval.RequestID = requestID
	}
	// 記録は冪等なので、届けられなければ再試行してよい (同じ承認を送り直す)。
	if err := h.respond.SendQuoteAccept(note, approval, quoter); err != nil {
		return fmt.Errorf("quote request: send accept: %w", err)
	}
	return nil
}

// quoteURIMaxRunes は note_quote_authorization の URI 列の幅。
const quoteURIMaxRunes = 512

type quoteDecision int

const (
	quoteIgnore quoteDecision = iota
	quoteReject
	quoteAllow
)

// decide reports how to answer a request to quote note.
//
// **見せていない投稿には答えない (Reject も返さない)。** Reject は作者の署名
// 付きで作者の URI を載せるので、答えるだけで「その id の投稿がある」「誰が
// 書いた」が分かる。aidx の id は時刻と連番なので推測でき、ダイレクト・
// ローカル限定・フォロワー限定の投稿の存在と作者を外から確かめられてしまう。
// 答えるのは、相手に見えている投稿だけ。**Mastodon より少し広い** — Mastodon は
// 公開と未収載 (distributable) にしか答えず、フォロワー限定の投稿にはフォロワー
// からでも答えない。mk-go はフォロワーには承認する (FEP の範囲内)。判定できない
// ときは error (通さない)。
func (h *QuoteRequestHandler) decide(note *model.Note, quoter *model.User) (quoteDecision, error) {
	if note.LocalOnly {
		return quoteIgnore, nil
	}
	author, err := h.users.FindByID(note.UserID)
	if repository.IsNotFound(err) {
		return quoteIgnore, nil
	}
	if err != nil {
		return quoteIgnore, fmt.Errorf("quote request: find author: %w", err)
	}
	if author.IsSuspended || author.IsDeleted {
		return quoteIgnore, nil
	}
	switch note.Visibility {
	case model.NoteVisibilityPublic, model.NoteVisibilityHome:
	case model.NoteVisibilityFollowers:
		// 配った範囲 (followers) の中からの引用だけを認める。フォロワーでない
		// 相手には、投稿があることも知らせない。
		following, err := h.follows.IsFollowing(quoter.ID, author.ID)
		if err != nil {
			return quoteIgnore, fmt.Errorf("quote request: follow check: %w", err)
		}
		if !following {
			return quoteIgnore, nil
		}
	default:
		return quoteIgnore, nil
	}
	blocked, err := h.blockedBetween(author.ID, quoter.ID)
	if err != nil {
		return quoteIgnore, err
	}
	if blocked {
		return quoteReject, nil
	}
	return quoteAllow, nil
}

// blockedBetween reports whether either user blocks the other.
func (h *QuoteRequestHandler) blockedBetween(a, b string) (bool, error) {
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		blocked, err := h.blocks.IsBlocked(pair[0], pair[1])
		if err != nil {
			return false, fmt.Errorf("quote request: block check: %w", err)
		}
		if blocked {
			return true, nil
		}
	}
	return false, nil
}

// reject answers with Reject and removes the approval this quoter may already
// hold for the quote (ブロックと競合して記録されたもの、前の試行で記録された
// もの)。届けられなければ error で返して再試行させる (相手は QuoteRequest を
// 自分からは送り直さないので、落とすと引用が保留のまま残る)。
//
// **消すのは送ってきた相手の承認だけ。** 拒否の経路では instrument を確かめて
// いない (`instrumentQuotes` は承認するときだけ走る) ので、quotingURI は相手が
// 書いた任意の値になる。相手で絞らないと、作者をブロックした (Block を送った)
// 相手が、他人の引用 URI を並べた QuoteRequest で他人の承認を消せる。
func (h *QuoteRequestHandler) reject(note *model.Note, requestID, quotingURI string, quoter *model.User) error {
	if err := h.approvals.Remove(note.ID, quotingURI, quoter.ID); err != nil {
		return fmt.Errorf("quote request: remove approval: %w", err)
	}
	if err := h.respond.SendQuoteReject(note, requestID, quotingURI, quoter); err != nil {
		return fmt.Errorf("quote request: send reject: %w", err)
	}
	return nil
}

// instrumentQuotes checks that the quoting note is by quoter and quotes
// targetURI. inline の instrument は actor と同じホストのときだけ信じ、そうで
// なければ取得して確かめる (取り込みはしない)。
func (h *QuoteRequestHandler) instrumentQuotes(rawInstrument json.RawMessage, quotingURI string, quoter *model.User, targetURI string) (bool, error) {
	if !sameHost(quotingURI, *quoter.URI) {
		return false, nil
	}
	var note *activitypub.Note
	var inline activitypub.Note
	if len(rawInstrument) > 0 && rawInstrument[0] == '{' && json.Unmarshal(rawInstrument, &inline) == nil {
		note = &inline
	} else {
		fetched, err := h.fetchNote(quotingURI)
		if err != nil {
			// 取れない・ホストが食い違うものは何度取り直しても変わらないので Reject。
			if isPermanentSkipError(err) || errors.Is(err, ErrObjectHostMismatch) {
				return false, nil
			}
			return false, fmt.Errorf("quote request: fetch instrument: %w", err)
		}
		note = fetched
	}
	if note.ID != quotingURI || string(note.AttributedTo) != *quoter.URI {
		return false, nil
	}
	for _, q := range []activitypub.APLenientID{note.Quote, note.MisskeyQuote, note.QuoteURL, note.QuoteURI} {
		if string(q) == targetURI {
			return true, nil
		}
	}
	return false, nil
}

// localNoteID extracts the id of a local note URI (`<base>/notes/<id>`).
func (h *QuoteRequestHandler) localNoteID(uri string) (string, bool) {
	prefix := h.urls.NoteURI("")
	if !strings.HasPrefix(uri, prefix) {
		return "", false
	}
	// 入れ子のパス (`/notes/<id>/activity`) や空の id は、引いても見つからない
	// (呼び出し側が「無い」として扱う) ので、ここでは形を問わない。
	return strings.TrimPrefix(uri, prefix), true
}

func isPureRenote(n *model.Note) bool {
	return n.RenoteID != nil && (n.Text == nil || *n.Text == "") && n.CW == nil && len(n.FileIDs) == 0 && !n.HasPoll
}

// refID reads an AP reference that is either a string or an object with id.
func refID(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.ID
	}
	return ""
}

func sameHost(a, b string) bool {
	ua, errA := url.Parse(a)
	ub, errB := url.Parse(b)
	if errA != nil || errB != nil || ua.Host == "" {
		return false
	}
	return normalizeMatchHost(ua) == normalizeMatchHost(ub)
}

// QuoteRequestDeliveryHook sends Accept / Reject for QuoteRequests through
// the delivery queue.
type QuoteRequestDeliveryHook struct {
	deliver  *DeliverService
	renderer *activitypub.Renderer
}

// NewQuoteRequestDeliveryHook constructs a QuoteRequestDeliveryHook.
func NewQuoteRequestDeliveryHook(deliver *DeliverService, renderer *activitypub.Renderer) *QuoteRequestDeliveryHook {
	return &QuoteRequestDeliveryHook{deliver: deliver, renderer: renderer}
}

// SendQuoteAccept implements QuoteRequestResponder.
func (h *QuoteRequestDeliveryHook) SendQuoteAccept(note *model.Note, approval *model.NoteQuoteAuthorization, quoter *model.User) error {
	body, err := json.Marshal(h.renderer.RenderQuoteRequestAccept(note, approval, *quoter.URI))
	if err != nil {
		return err
	}
	return h.deliver.DeliverToUser(note.UserID, quoter, body)
}

// SendQuoteReject implements QuoteRequestResponder.
func (h *QuoteRequestDeliveryHook) SendQuoteReject(note *model.Note, requestID, quotingURI string, quoter *model.User) error {
	body, err := json.Marshal(h.renderer.RenderQuoteRequestReject(note, requestID, quotingURI, *quoter.URI))
	if err != nil {
		return err
	}
	return h.deliver.DeliverToUser(note.UserID, quoter, body)
}
