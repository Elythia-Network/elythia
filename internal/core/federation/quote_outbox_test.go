package federation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

func (s *qrStore) FindByNoteIDAndQuotingURI(noteID, quotingURI string) (*model.NoteQuoteAuthorization, error) {
	for _, r := range s.rows {
		if r.NoteID == noteID && r.QuotingURI == quotingURI {
			cp := *r
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

type qoRequests struct {
	rows map[string]*model.NoteQuoteRequest
	err  error
}

func (r *qoRequests) FindByNoteID(noteID string) (*model.NoteQuoteRequest, error) {
	if r.err != nil {
		return nil, r.err
	}
	if q, ok := r.rows[noteID]; ok {
		cp := *q
		return &cp, nil
	}
	return nil, repository.ErrNotFound
}

func (r *qoRequests) FindByRequestURI(uri string) (*model.NoteQuoteRequest, error) {
	if r.err != nil {
		return nil, r.err
	}
	for _, q := range r.rows {
		if q.RequestURI == uri {
			cp := *q
			return &cp, nil
		}
	}
	return nil, repository.ErrNotFound
}

func (r *qoRequests) Ensure(noteID, uri string) (*model.NoteQuoteRequest, error) {
	if r.err != nil {
		return nil, r.err
	}
	if _, ok := r.rows[noteID]; !ok {
		r.rows[noteID] = &model.NoteQuoteRequest{NoteID: noteID, RequestURI: uri, State: model.QuoteRequestPending}
	}
	return r.FindByNoteID(noteID)
}

func (r *qoRequests) MarkAccepted(noteID, approval string) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	q := r.rows[noteID]
	if q.State == model.QuoteRequestPending {
		q.State, q.ApprovalURI, q.UpdateSent = model.QuoteRequestAccepted, &approval, false
	}
	return q.ApprovalURI != nil && *q.ApprovalURI == approval && !q.UpdateSent, nil
}

func (r *qoRequests) MarkUpdateSent(noteID, approval string) error {
	if r.err != nil {
		return r.err
	}
	if q := r.rows[noteID]; q.ApprovalURI != nil && *q.ApprovalURI == approval {
		q.UpdateSent = true
	}
	return nil
}

func (r *qoRequests) MarkRejected(noteID string) error {
	if r.err != nil {
		return r.err
	}
	if q := r.rows[noteID]; q.State == model.QuoteRequestPending {
		q.State = model.QuoteRequestRejected
	}
	return nil
}

type qoDelivery struct {
	requests []string // "<noteId> <quotedURI> <authorId>"
	updates  []string // "<noteId> <authorId>"
	err      error
}

func (d *qoDelivery) SendQuoteRequest(note *model.Note, quotedURI string, author *model.User) error {
	d.requests = append(d.requests, note.ID+" "+quotedURI+" "+author.ID)
	return d.err
}

func (d *qoDelivery) SendNoteUpdate(note *model.Note, author *model.User) error {
	d.updates = append(d.updates, note.ID+" "+author.ID)
	return d.err
}

const (
	qoCarolURI  = "https://remote.example/users/carol"
	qoRemoteURI = "https://remote.example/statuses/9"
	qoApproval  = "https://remote.example/users/carol/quote_authorizations/1"
)

type qoEnv struct {
	*qrEnv
	o        *QuoteOutbox
	requests *qoRequests
	delivery *qoDelivery
	dave     *model.User
}

// newQOEnv: alice (ローカル、段階 2 の投稿の作者)、dave (ローカル、引用する人)、
// carol (リモート、remote の作者)。
func newQOEnv(t *testing.T) *qoEnv {
	t.Helper()
	e := &qoEnv{qrEnv: newQREnv(t), requests: &qoRequests{rows: map[string]*model.NoteQuoteRequest{}}, delivery: &qoDelivery{}}
	carolURI, host := qoCarolURI, "remote.example"
	e.dave = &model.User{ID: "dave"}
	e.users.users["dave"] = e.dave
	e.users.users["carol"] = &model.User{ID: "carol", Host: &host, URI: &carolURI}
	remoteURI := qoRemoteURI
	e.notes["remote"].URI = &remoteURI
	e.o = NewQuoteOutbox(e.h, e.requests, e.store, e.delivery)
	return e
}

func (e *qoEnv) approval(t *testing.T, n *model.Note) string {
	t.Helper()
	a, err := e.o.ApprovalURI(n)
	require.NoError(t, err)
	return a
}

func (e *qoEnv) quote(id, target string, vis model.NoteVisibility) *model.Note {
	text := "look"
	n := &model.Note{ID: id, UserID: "dave", Text: &text, RenoteID: strp(target), Visibility: vis}
	e.notes[id] = n
	return n
}

func TestQuoteOutbox_PrepareIssuesLocalApproval(t *testing.T) {
	e := newQOEnv(t)
	n := e.quote("q1", "pub", model.NoteVisibilityPublic)
	require.NoError(t, e.o.Prepare(n, e.dave))
	require.Len(t, e.store.rows, 1)
	got := e.store.rows[0]
	assert.Equal(t, "pub", got.NoteID)
	assert.Equal(t, "dave", got.QuoterID)
	assert.Equal(t, qrBase+"/notes/q1", got.QuotingURI)
	assert.Nil(t, got.RequestID)
	assert.Equal(t, qrBase+"/notes/pub/quote-authorizations/"+got.ID, e.approval(t, n))

	// 作り直しても承認は 1 つ。
	require.NoError(t, e.o.Prepare(n, e.dave))
	assert.Len(t, e.store.rows, 1)
	assert.Empty(t, e.delivery.requests, "local quotes are not requested")
}

func TestQuoteOutbox_PrepareSkips(t *testing.T) {
	for name, setup := range map[string]func(e *qoEnv) *model.Note{
		"self quote": func(e *qoEnv) *model.Note {
			n := e.quote("q1", "pub", model.NoteVisibilityPublic)
			n.UserID = "alice"
			return n
		},
		"remote target": func(e *qoEnv) *model.Note { return e.quote("q1", "remote", model.NoteVisibilityPublic) },
		"direct quote":  func(e *qoEnv) *model.Note { return e.quote("q1", "pub", model.NoteVisibilitySpecified) },
		"local only": func(e *qoEnv) *model.Note {
			n := e.quote("q1", "pub", model.NoteVisibilityPublic)
			n.LocalOnly = true
			return n
		},
		"pure renote": func(e *qoEnv) *model.Note {
			n := e.quote("q1", "pub", model.NoteVisibilityPublic)
			n.Text = nil
			return n
		},
		"quoting a pure renote": func(e *qoEnv) *model.Note { return e.quote("q1", "renote", model.NoteVisibilityPublic) },
		"target gone":           func(e *qoEnv) *model.Note { return e.quote("q1", "nothere", model.NoteVisibilityPublic) },
		"blocked": func(e *qoEnv) *model.Note {
			e.blocks.set[[2]string{"alice", "dave"}] = true
			return e.quote("q1", "pub", model.NoteVisibilityPublic)
		},
		"followers only, not following": func(e *qoEnv) *model.Note { return e.quote("q1", "fol", model.NoteVisibilityPublic) },
		"direct target":                 func(e *qoEnv) *model.Note { return e.quote("q1", "dm", model.NoteVisibilityPublic) },
	} {
		t.Run(name, func(t *testing.T) {
			e := newQOEnv(t)
			n := setup(e)
			require.NoError(t, e.o.Prepare(n, e.dave))
			assert.Empty(t, e.store.rows)
			assert.Empty(t, e.approval(t, n))
		})
	}
}

func TestQuoteOutbox_PrepareFollowersOnlyWithFollow(t *testing.T) {
	e := newQOEnv(t)
	e.follows.set[[2]string{"dave", "alice"}] = true
	require.NoError(t, e.o.Prepare(e.quote("q1", "fol", model.NoteVisibilityFollowers), e.dave))
	assert.Len(t, e.store.rows, 1)
}

func TestQuoteOutbox_PrepareFailures(t *testing.T) {
	e := newQOEnv(t)
	assert.Error(t, e.o.Prepare(e.quote("q1", "dberr", model.NoteVisibilityPublic), e.dave), "quoted note lookup")

	e = newQOEnv(t)
	e.blocks.err = errors.New("db down")
	assert.Error(t, e.o.Prepare(e.quote("q1", "pub", model.NoteVisibilityPublic), e.dave), "decision")

	e = newQOEnv(t)
	e.store.err = errors.New("db down")
	assert.Error(t, e.o.Prepare(e.quote("q1", "pub", model.NoteVisibilityPublic), e.dave), "record")
}

func TestQuoteOutbox_RequestApproval(t *testing.T) {
	e := newQOEnv(t)
	n := e.quote("q1", "remote", model.NoteVisibilityHome)
	require.NoError(t, e.o.RequestApproval(n, e.dave))
	assert.Equal(t, []string{"q1 " + qoRemoteURI + " carol"}, e.delivery.requests)
	row := e.requests.rows["q1"]
	require.NotNil(t, row)
	assert.Equal(t, qrBase+"/notes/q1#quote-request", row.RequestURI)
	assert.Equal(t, model.QuoteRequestPending, row.State)
	// 承認が返るまでは quoteAuthorization を付けない。
	assert.Empty(t, e.approval(t, n))
}

func TestQuoteOutbox_RequestApprovalSkips(t *testing.T) {
	for name, setup := range map[string]func(e *qoEnv) *model.Note{
		"local target": func(e *qoEnv) *model.Note { return e.quote("q1", "pub", model.NoteVisibilityPublic) },
		"direct quote": func(e *qoEnv) *model.Note { return e.quote("q1", "remote", model.NoteVisibilitySpecified) },
		"local only": func(e *qoEnv) *model.Note {
			n := e.quote("q1", "remote", model.NoteVisibilityPublic)
			n.LocalOnly = true
			return n
		},
		"not a quote": func(e *qoEnv) *model.Note {
			n := e.quote("q1", "remote", model.NoteVisibilityPublic)
			n.Text = nil
			return n
		},
		"target no uri": func(e *qoEnv) *model.Note {
			e.notes["remote"].URI = nil
			return e.quote("q1", "remote", model.NoteVisibilityPublic)
		},
		"author gone": func(e *qoEnv) *model.Note {
			delete(e.users.users, "carol")
			return e.quote("q1", "remote", model.NoteVisibilityPublic)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQOEnv(t)
			require.NoError(t, e.o.RequestApproval(setup(e), e.dave))
			assert.Empty(t, e.delivery.requests)
			assert.Empty(t, e.requests.rows)
		})
	}
}

func TestQuoteOutbox_RequestApprovalFailures(t *testing.T) {
	e := newQOEnv(t)
	e.users.err = errors.New("db down")
	assert.Error(t, e.o.RequestApproval(e.quote("q1", "remote", model.NoteVisibilityPublic), e.dave), "author lookup")

	e = newQOEnv(t)
	e.requests.err = errors.New("db down")
	assert.Error(t, e.o.RequestApproval(e.quote("q1", "remote", model.NoteVisibilityPublic), e.dave), "record")
	assert.Empty(t, e.delivery.requests, "nothing is sent that could not be matched later")

	e = newQOEnv(t)
	e.delivery.err = errors.New("queue down")
	assert.Error(t, e.o.RequestApproval(e.quote("q1", "remote", model.NoteVisibilityPublic), e.dave), "send")
}

func (e *qoEnv) requested(t *testing.T) *model.Note {
	t.Helper()
	n := e.quote("q1", "remote", model.NoteVisibilityPublic)
	require.NoError(t, e.o.RequestApproval(n, e.dave))
	return n
}

func TestQuoteOutbox_HandleAccept(t *testing.T) {
	e := newQOEnv(t)
	n := e.requested(t)
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, qrBase+"/notes/q1#quote-request", true, qoApproval))
	assert.Equal(t, model.QuoteRequestAccepted, e.requests.rows["q1"].State)
	assert.Equal(t, qoApproval, e.approval(t, n))
	assert.Equal(t, []string{"q1 dave"}, e.delivery.updates)
}

func TestQuoteOutbox_HandleAnswerIgnores(t *testing.T) {
	long := "https://remote.example/" + strings.Repeat("a", quoteURIMaxRunes)
	for name, tc := range map[string]struct {
		actor, request, result string
	}{
		"not the quoted author":    {actor: qrQuoterURI, result: qoApproval},
		"unknown request":          {actor: qoCarolURI, request: qrBase + "/notes/zz#quote-request", result: qoApproval},
		"approval on another host": {actor: qoCarolURI, result: "https://evil.example/approvals/1"},
		"no approval":              {actor: qoCarolURI},
		"approval too long":        {actor: qoCarolURI, result: long},
		"approval not http":        {actor: qoCarolURI, result: "ftp://remote.example/a"},
		"approval with nul":        {actor: qoCarolURI, result: "https://remote.example/a\x00"},
		"approval on www host":     {actor: qoCarolURI, result: "https://www.remote.example/approvals/1"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQOEnv(t)
			n := e.requested(t)
			req := tc.request
			if req == "" {
				req = qrBase + "/notes/q1#quote-request"
			}
			require.NoError(t, e.o.HandleAnswer(tc.actor, req, true, tc.result))
			assert.Equal(t, model.QuoteRequestPending, e.requests.rows["q1"].State)
			assert.Empty(t, e.approval(t, n))
			assert.Empty(t, e.delivery.updates)
		})
	}
}

func TestQuoteOutbox_HandleReject(t *testing.T) {
	e := newQOEnv(t)
	e.requested(t)
	// 引用される作者以外の Reject では変えない。
	require.NoError(t, e.o.HandleAnswer(qrQuoterURI, qrBase+"/notes/q1#quote-request", false, ""))
	assert.Equal(t, model.QuoteRequestPending, e.requests.rows["q1"].State)
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, qrBase+"/notes/q1#quote-request", false, ""))
	assert.Equal(t, model.QuoteRequestRejected, e.requests.rows["q1"].State)
	assert.Empty(t, e.delivery.updates)
}

func TestQuoteOutbox_HandleAnswerFailures(t *testing.T) {
	uri := qrBase + "/notes/q1#quote-request"

	e := newQOEnv(t)
	e.requested(t)
	e.delivery.err = errors.New("queue down")
	assert.Error(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval), "a lost update is retried")

	e = newQOEnv(t)
	e.requested(t)
	e.requests.err = errors.New("db down")
	assert.Error(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval), "request lookup")

	e = newQOEnv(t)
	e.requested(t)
	e.users.err = errors.New("db down")
	assert.Error(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval), "author lookup")

	// 引用した投稿が消えていたら、答えは捨てる。
	e = newQOEnv(t)
	e.requested(t)
	delete(e.notes, "q1")
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	assert.Empty(t, e.delivery.updates)
}

func TestQuoteOutbox_ApprovalURIIgnoresNonQuotes(t *testing.T) {
	e := newQOEnv(t)
	text := "plain"
	assert.Empty(t, e.approval(t, &model.Note{ID: "p", UserID: "dave", Text: &text}))
	assert.Empty(t, e.approval(t, nil))
	assert.Empty(t, e.approval(t, e.quote("q0", "nothere", model.NoteVisibilityPublic)))
}

// DB 障害は「承認が無い」と区別して返す (描画側が、承認なしで描画するか作らずに
// 再試行するかを選ぶ)。
func TestQuoteOutbox_ApprovalURIErrors(t *testing.T) {
	e := newQOEnv(t)
	_, err := e.o.ApprovalURI(e.quote("q1", "dberr", model.NoteVisibilityPublic))
	assert.Error(t, err, "quoted note lookup")

	e = newQOEnv(t)
	e.requests.err = errors.New("db down")
	_, err = e.o.ApprovalURI(e.quote("q2", "remote", model.NoteVisibilityPublic))
	assert.Error(t, err, "request lookup")

	e = newQOEnv(t)
	e.o = NewQuoteOutbox(e.h, e.requests, qoFailingApprovals{}, e.delivery)
	_, err = e.o.ApprovalURI(e.quote("q3", "pub", model.NoteVisibilityPublic))
	assert.Error(t, err, "local approval lookup")
}

type qoFailingApprovals struct{}

func (qoFailingApprovals) FindByNoteIDAndQuotingURI(string, string) (*model.NoteQuoteAuthorization, error) {
	return nil, errors.New("db down")
}

// 承認 URI があっても、承認済みでなければ付けない。
func TestQuoteOutbox_ApprovalURIOnlyWhenAccepted(t *testing.T) {
	e := newQOEnv(t)
	n := e.requested(t)
	stale := qoApproval
	e.requests.rows["q1"].ApprovalURI = &stale
	for _, state := range []string{model.QuoteRequestPending, model.QuoteRequestRejected} {
		e.requests.rows["q1"].State = state
		assert.Empty(t, e.approval(t, n), state)
	}
}

// 同じ承認が何度届いても、配り直しは 1 回だけ。承認の後から別の承認 URI の
// Accept が届いても変えず、配り直さない (Mastodon も pending のときしか受けない。
// 受けると、作者が URI を変えるたびにフォロワー全員へ Update を送ることになる)。
func TestQuoteOutbox_HandleAcceptOnlyOnce(t *testing.T) {
	uri := qrBase + "/notes/q1#quote-request"
	e := newQOEnv(t)
	n := e.requested(t)
	for i := 0; i < 3; i++ {
		require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	}
	assert.Len(t, e.delivery.updates, 1)
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval+"b"))
	assert.Len(t, e.delivery.updates, 1)
	assert.Equal(t, qoApproval, e.approval(t, n))
}

// 拒否された後の Accept では承認にしない。
func TestQuoteOutbox_AcceptAfterRejectIgnored(t *testing.T) {
	uri := qrBase + "/notes/q1#quote-request"
	e := newQOEnv(t)
	n := e.requested(t)
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, false, ""))
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	assert.Empty(t, e.delivery.updates)
	assert.Empty(t, e.approval(t, n))
}

// 配り直しに失敗しても承認の記録は消さない (取得や Create には承認が付き続ける)。
// 再試行で送り直す。失敗と重なって届いた同じ Accept も送り直す側に回る
// (「変化なし」で成功扱いにすると、inbox の重複除けが再試行を捨てて承認が届かない)。
func TestQuoteOutbox_HandleAcceptRetriesFailedUpdate(t *testing.T) {
	uri := qrBase + "/notes/q1#quote-request"
	e := newQOEnv(t)
	n := e.requested(t)
	e.delivery.err = errors.New("queue down")
	require.Error(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	assert.Equal(t, qoApproval, e.approval(t, n), "the approval stays recorded")
	require.Error(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval), "a duplicate Accept also tries to send")

	e.delivery.err = nil
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	assert.Len(t, e.delivery.updates, 3)
	// 配り終えたら、同じ Accept ではもう送らない。
	require.NoError(t, e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	assert.Len(t, e.delivery.updates, 3)
}

type qoFailingSentMark struct{ *qoRequests }

func (qoFailingSentMark) MarkUpdateSent(string, string) error { return errors.New("db down") }

// 送れた後の記録に失敗しても再試行はさせない (送り直しは次の Accept で足りる)。
func TestQuoteOutbox_HandleAcceptSentMarkFailure(t *testing.T) {
	e := newQOEnv(t)
	e.requested(t)
	o := NewQuoteOutbox(e.h, qoFailingSentMark{e.requests}, e.store, e.delivery)
	require.NoError(t, o.HandleAnswer(qoCarolURI, qrBase+"/notes/q1#quote-request", true, qoApproval))
	assert.Len(t, e.delivery.updates, 1)
}

func TestQuoteAnswerRequestURI(t *testing.T) {
	base := "https://local.example"
	ours := base + "/notes/q1#quote-request"
	for name, tc := range map[string]struct {
		object any
		want   string
	}{
		"id string":             {object: ours, want: ours},
		"foreign string":        {object: "https://other.example/notes/q1#quote-request"},
		"follow id string":      {object: base + "/follows/1"},
		"embedded":              {object: map[string]any{"id": ours, "type": "QuoteRequest"}, want: ours},
		"embedded expanded":     {object: map[string]any{"id": ours, "type": "https://w3id.org/fep/044f#QuoteRequest"}, want: ours},
		"embedded type array":   {object: map[string]any{"id": ours, "type": []string{"QuoteRequest"}}, want: ours},
		"embedded follow":       {object: map[string]any{"id": ours, "type": "Follow"}},
		"embedded without type": {object: map[string]any{"id": ours}},
		"local note id string":  {object: base + "/notes/q1"},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(tc.object)
			require.NoError(t, err)
			assert.Equal(t, tc.want, quoteAnswerRequestURI(raw, base))
		})
	}
	assert.Empty(t, quoteAnswerRequestURI(json.RawMessage(`[1`), base))
	assert.Empty(t, quoteAnswerRequestURI(json.RawMessage(`123`), base))
}

func TestQuoteAnswerResult(t *testing.T) {
	assert.Equal(t, qoApproval, quoteAnswerResult(json.RawMessage(`{"result":"`+qoApproval+`"}`)))
	assert.Equal(t, qoApproval, quoteAnswerResult(json.RawMessage(`{"result":{"id":"`+qoApproval+`"}}`)))
	assert.Equal(t, qoApproval, quoteAnswerResult(json.RawMessage(`{"result":["`+qoApproval+`","https://x/2"]}`)))
	assert.Equal(t, qoApproval, quoteAnswerResult(json.RawMessage(`{"result":[{"id":"`+qoApproval+`"}]}`)))
	assert.Empty(t, quoteAnswerResult(json.RawMessage(`{"result":[]}`)))
	assert.Empty(t, quoteAnswerResult(json.RawMessage(`{}`)))
	assert.Empty(t, quoteAnswerResult(json.RawMessage(`[`)))
}

// 答えを受けたときの、引けない / 壊れている場合。「無い」は捨て、障害は再試行させる。
func TestQuoteOutbox_HandleAnswerLookups(t *testing.T) {
	uri := qrBase + "/notes/q1#quote-request"
	for name, tc := range map[string]struct {
		setup   func(e *qoEnv)
		wantErr bool
	}{
		"quoting note is no longer a quote": {setup: func(e *qoEnv) { e.notes["q1"].RenoteID = nil }},
		"quoted note gone":                  {setup: func(e *qoEnv) { delete(e.notes, "remote") }},
		"quoted note lookup fails":          {setup: func(e *qoEnv) { e.notes["q1"].RenoteID = strp("dberr") }, wantErr: true},
		"quoted author gone":                {setup: func(e *qoEnv) { delete(e.users.users, "carol") }},
		"quoter gone":                       {setup: func(e *qoEnv) { delete(e.users.users, "dave") }},
		"note lookup fails": {setup: func(e *qoEnv) {
			e.requests.rows["q1"].NoteID = "dberr"
		}, wantErr: true},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQOEnv(t)
			e.requested(t)
			tc.setup(e)
			err := e.o.HandleAnswer(qoCarolURI, uri, true, qoApproval)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Empty(t, e.delivery.updates)
		})
	}
}

type qoFailingMarks struct{ *qoRequests }

func (qoFailingMarks) MarkAccepted(string, string) (bool, error) { return false, errors.New("db down") }
func (qoFailingMarks) MarkRejected(string) error                 { return errors.New("db down") }

func TestQuoteOutbox_HandleAnswerRecordFailures(t *testing.T) {
	uri := qrBase + "/notes/q1#quote-request"
	e := newQOEnv(t)
	e.requested(t)
	o := NewQuoteOutbox(e.h, qoFailingMarks{e.requests}, e.store, e.delivery)
	assert.Error(t, o.HandleAnswer(qoCarolURI, uri, true, qoApproval))
	assert.Error(t, o.HandleAnswer(qoCarolURI, uri, false, ""))
	assert.Empty(t, e.delivery.updates, "nothing is sent before the approval is recorded")
}
