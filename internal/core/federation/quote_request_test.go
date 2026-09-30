package federation

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/misc/id"
	"github.com/shiroha-a/mk/internal/model"
	"github.com/shiroha-a/mk/internal/repository"
)

const (
	qrBase      = "https://local.example"
	qrQuoterURI = "https://remote.example/users/bob"
	qrQuoting   = "https://remote.example/statuses/1"
	qrRequestID = "https://remote.example/quote-requests/1"
)

type qrNotes map[string]*model.Note

func (n qrNotes) FindByID(id string) (*model.Note, error) {
	if id == "dberr" {
		return nil, errors.New("db down")
	}
	if note, ok := n[id]; ok {
		return note, nil
	}
	return nil, repository.ErrNotFound
}

type qrUsers struct {
	users map[string]*model.User
	err   error
}

func (u qrUsers) FindByID(id string) (*model.User, error) {
	if u.err != nil {
		return nil, u.err
	}
	if x, ok := u.users[id]; ok {
		return x, nil
	}
	return nil, repository.ErrNotFound
}

type qrPairs struct {
	set map[[2]string]bool
	err error
}

func (p qrPairs) IsBlocked(a, b string) (bool, error)   { return p.set[[2]string{a, b}], p.err }
func (p qrPairs) IsFollowing(a, b string) (bool, error) { return p.set[[2]string{a, b}], p.err }

type qrStore struct {
	rows []*model.NoteQuoteAuthorization
	err  error
}

func (s *qrStore) Ensure(a *model.NoteQuoteAuthorization) (*model.NoteQuoteAuthorization, error) {
	if s.err != nil {
		return nil, s.err
	}
	for _, r := range s.rows {
		if r.NoteID == a.NoteID && r.QuotingURI == a.QuotingURI {
			cp := *r
			return &cp, nil
		}
	}
	cp := *a
	s.rows = append(s.rows, &cp)
	out := cp
	return &out, nil
}

func (s *qrStore) Remove(noteID, quotingURI, quoterID string) error {
	if s.err != nil {
		return s.err
	}
	kept := s.rows[:0]
	for _, r := range s.rows {
		if r.NoteID != noteID || r.QuotingURI != quotingURI || r.QuoterID != quoterID {
			kept = append(kept, r)
		}
	}
	s.rows = kept
	return nil
}

type qrResponder struct {
	accepted []*model.NoteQuoteAuthorization
	rejected []string
	err      error
}

func (r *qrResponder) SendQuoteAccept(_ *model.Note, a *model.NoteQuoteAuthorization, _ *model.User) error {
	cp := *a
	r.accepted = append(r.accepted, &cp)
	return r.err
}

func (r *qrResponder) SendQuoteReject(_ *model.Note, requestID, quotingURI string, _ *model.User) error {
	r.rejected = append(r.rejected, requestID+" "+quotingURI)
	return r.err
}

type qrEnv struct {
	h       *QuoteRequestHandler
	notes   qrNotes
	users   *qrUsers
	blocks  *qrPairs
	follows *qrPairs
	store   *qrStore
	resp    *qrResponder
	fetched []string
	remote  map[string]*activitypub.Note
	fetchEr error
	quoter  *model.User
}

func newQREnv(t *testing.T) *qrEnv {
	t.Helper()
	text := "hello"
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	quoterURI := qrQuoterURI
	host := "remote.example"
	e := &qrEnv{
		notes: qrNotes{
			"pub":    {ID: "pub", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic},
			"home":   {ID: "home", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityHome},
			"fol":    {ID: "fol", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityFollowers},
			"dm":     {ID: "dm", UserID: "alice", Text: &text, Visibility: model.NoteVisibilitySpecified},
			"lonly":  {ID: "lonly", UserID: "alice", Text: &text, Visibility: model.NoteVisibilityPublic, LocalOnly: true},
			"renote": {ID: "renote", UserID: "alice", RenoteID: strp("pub"), Visibility: model.NoteVisibilityPublic},
			"remote": {ID: "remote", UserID: "carol", UserHost: &host, Text: &text, Visibility: model.NoteVisibilityPublic},
			"gone":   {ID: "gone", UserID: "ghost", Text: &text, Visibility: model.NoteVisibilityPublic},
		},
		users:   &qrUsers{users: map[string]*model.User{"alice": {ID: "alice"}}},
		blocks:  &qrPairs{set: map[[2]string]bool{}},
		follows: &qrPairs{set: map[[2]string]bool{}},
		store:   &qrStore{},
		resp:    &qrResponder{},
		remote:  map[string]*activitypub.Note{},
		quoter:  &model.User{ID: "bob", Host: &host, URI: &quoterURI},
	}
	e.h = NewQuoteRequestHandler(QuoteRequestDeps{
		Notes: e.notes, Users: e.users, Blocks: e.blocks, Follows: e.follows, Approvals: e.store,
		FetchNote: func(uri string) (*activitypub.Note, error) {
			e.fetched = append(e.fetched, uri)
			if e.fetchEr != nil {
				return nil, e.fetchEr
			}
			if n, ok := e.remote[uri]; ok {
				return n, nil
			}
			return nil, &activitypub.StatusError{StatusCode: 404}
		},
		Respond: e.resp, URLs: activitypub.NewURLBuilder(qrBase), IDGen: gen,
	})
	return e
}

// inlineInstrument is the quoting note as Mastodon inlines it.
func inlineInstrument(target string) map[string]any {
	return map[string]any{
		"id": qrQuoting, "type": "Note", "attributedTo": qrQuoterURI,
		"quote": target, "_misskey_quote": target, "content": "quoting",
	}
}

func quoteRequest(t *testing.T, noteID string, instrument any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"id": qrRequestID, "type": "QuoteRequest", "actor": qrQuoterURI,
		"object": qrBase + "/notes/" + noteID, "instrument": instrument,
	})
	require.NoError(t, err)
	return raw
}

func (e *qrEnv) handle(t *testing.T, raw json.RawMessage) error {
	t.Helper()
	return e.h.Handle(e.quoter, raw)
}

func TestQuoteRequest_AcceptsPublicAndHome(t *testing.T) {
	for _, noteID := range []string{"pub", "home"} {
		t.Run(noteID, func(t *testing.T) {
			e := newQREnv(t)
			target := qrBase + "/notes/" + noteID
			require.NoError(t, e.handle(t, quoteRequest(t, noteID, inlineInstrument(target))))
			require.Len(t, e.resp.accepted, 1)
			a := e.resp.accepted[0]
			assert.Equal(t, noteID, a.NoteID)
			assert.Equal(t, "bob", a.QuoterID)
			assert.Equal(t, qrQuoting, a.QuotingURI)
			require.NotNil(t, a.RequestID)
			assert.Equal(t, qrRequestID, *a.RequestID)
			assert.Empty(t, e.resp.rejected)
			assert.Empty(t, e.fetched, "an inline instrument from the actor's host is not fetched")
		})
	}
}

// 同じ引用の再送には同じ承認を返し、Accept の object は今回の QuoteRequest を指す。
func TestQuoteRequest_ResentGetsSameApproval(t *testing.T) {
	e := newQREnv(t)
	target := qrBase + "/notes/pub"
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(target))))
	raw, err := json.Marshal(map[string]any{
		"id": qrRequestID + "-again", "type": "QuoteRequest", "actor": qrQuoterURI,
		"object": target, "instrument": inlineInstrument(target),
	})
	require.NoError(t, err)
	require.NoError(t, e.handle(t, raw))
	require.Len(t, e.resp.accepted, 2)
	assert.Equal(t, e.resp.accepted[0].ID, e.resp.accepted[1].ID)
	assert.Equal(t, qrRequestID+"-again", *e.resp.accepted[1].RequestID)
	assert.Len(t, e.store.rows, 1)
}

func TestQuoteRequest_FollowersOnlyNeedsFollow(t *testing.T) {
	e := newQREnv(t)
	target := qrBase + "/notes/fol"
	require.NoError(t, e.handle(t, quoteRequest(t, "fol", inlineInstrument(target))))
	assert.Empty(t, e.resp.accepted)
	// フォロワーでない相手には、投稿があることも知らせない (Reject も返さない)。
	assert.Empty(t, e.resp.rejected)

	e = newQREnv(t)
	e.follows.set[[2]string{"bob", "alice"}] = true
	require.NoError(t, e.handle(t, quoteRequest(t, "fol", inlineInstrument(target))))
	assert.Len(t, e.resp.accepted, 1)

	// 逆向き (作者が引用者をフォロー) では足りない。
	e = newQREnv(t)
	e.follows.set[[2]string{"alice", "bob"}] = true
	require.NoError(t, e.handle(t, quoteRequest(t, "fol", inlineInstrument(target))))
	assert.Empty(t, e.resp.accepted)
}

// 引用する投稿を確かめられないときは答えない (Mastodon も黙って捨てる)。Reject に
// すると、inline で送らない実装の正当な引用を恒久的に拒否してしまう。
func TestQuoteRequest_UnverifiableInstrumentIsIgnored(t *testing.T) {
	for name, tc := range map[string]struct {
		noteID string
		setup  func(e *qrEnv)
		instr  func(target string) any
	}{
		"quotes another note": {noteID: "pub", instr: func(string) any { return inlineInstrument(qrBase + "/notes/home") }},
		"not by the actor": {noteID: "pub", instr: func(target string) any {
			m := inlineInstrument(target)
			m["attributedTo"] = "https://remote.example/users/mallory"
			return m
		}},
		"instrument on another host": {noteID: "pub", instr: func(target string) any {
			m := inlineInstrument(target)
			m["id"] = "https://evil.example/statuses/1"
			return m
		}},
		"fetched instrument gone": {noteID: "pub", instr: func(string) any { return qrQuoting }},
		"fetched host mismatch": {noteID: "pub", setup: func(e *qrEnv) { e.fetchEr = ErrObjectHostMismatch },
			instr: func(string) any { return qrQuoting }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQREnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			require.NoError(t, e.handle(t, quoteRequest(t, tc.noteID, tc.instr(qrBase+"/notes/"+tc.noteID))))
			assert.Empty(t, e.resp.accepted)
			assert.Empty(t, e.resp.rejected)
			assert.Empty(t, e.store.rows)
		})
	}
}

func TestQuoteRequest_Rejects(t *testing.T) {
	for name, tc := range map[string]struct {
		noteID string
		setup  func(e *qrEnv)
		instr  func(target string) any
	}{
		"author blocks": {noteID: "pub", setup: func(e *qrEnv) { e.blocks.set[[2]string{"alice", "bob"}] = true }},
		"quoter blocks": {noteID: "pub", setup: func(e *qrEnv) { e.blocks.set[[2]string{"bob", "alice"}] = true }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQREnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			target := qrBase + "/notes/" + tc.noteID
			var instr any = inlineInstrument(target)
			if tc.instr != nil {
				instr = tc.instr(target)
			}
			require.NoError(t, e.handle(t, quoteRequest(t, tc.noteID, instr)))
			assert.Empty(t, e.resp.accepted)
			require.Len(t, e.resp.rejected, 1)
			// Reject は受け取った QuoteRequest を id で指す。
			assert.True(t, strings.HasPrefix(e.resp.rejected[0], qrRequestID+" "), e.resp.rejected[0])
			assert.Empty(t, e.store.rows)
		})
	}
}

// 見せていない投稿には答えない。Reject は作者の署名付きで作者の URI を載せる
// ので、答えるだけで「その id の投稿がある」「誰が書いた」が分かってしまう。
func TestQuoteRequest_DoesNotRevealHiddenNotes(t *testing.T) {
	for name, tc := range map[string]struct {
		noteID string
		setup  func(e *qrEnv)
	}{
		"direct message":        {noteID: "dm"},
		"local only":            {noteID: "lonly"},
		"author gone":           {noteID: "gone"},
		"author suspended":      {noteID: "pub", setup: func(e *qrEnv) { e.users.users["alice"].IsSuspended = true }},
		"author deleted":        {noteID: "pub", setup: func(e *qrEnv) { e.users.users["alice"].IsDeleted = true }},
		"followers, not follow": {noteID: "fol"},
		// ブロックされていても、見えない投稿であることを先に判定する。
		"dm and blocked": {noteID: "dm", setup: func(e *qrEnv) { e.blocks.set[[2]string{"alice", "bob"}] = true }},
	} {
		t.Run(name, func(t *testing.T) {
			e := newQREnv(t)
			if tc.setup != nil {
				tc.setup(e)
			}
			require.NoError(t, e.handle(t, quoteRequest(t, tc.noteID, inlineInstrument(qrBase+"/notes/"+tc.noteID))))
			assert.Empty(t, e.resp.accepted)
			assert.Empty(t, e.resp.rejected)
			assert.Empty(t, e.store.rows)
		})
	}
}

// 相手は QuoteRequest を自分からは送り直さないので、答えを届けられなければ
// error で返して inbox に再試行させる。承認は冪等なので、再試行でも増えない。
func TestQuoteRequest_DeliveryFailureIsRetried(t *testing.T) {
	e := newQREnv(t)
	e.resp.err = errors.New("redis down")
	target := qrBase + "/notes/pub"
	require.Error(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(target))))
	e.resp.err = nil
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(target))))
	require.Len(t, e.resp.accepted, 2)
	assert.Equal(t, e.resp.accepted[0].ID, e.resp.accepted[1].ID)
	assert.Len(t, e.store.rows, 1)

	e = newQREnv(t)
	e.resp.err = errors.New("redis down")
	e.blocks.set[[2]string{"alice", "bob"}] = true
	require.Error(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(target))))
}

// 列に入らない URI は記録できないので、再試行させずに捨てる。
func TestQuoteRequest_UnstorableURIsAreIgnored(t *testing.T) {
	target := qrBase + "/notes/pub"
	long := "https://remote.example/" + strings.Repeat("a", 512)
	for name, raw := range map[string]json.RawMessage{
		"long instrument": func() json.RawMessage {
			m := inlineInstrument(target)
			m["id"] = long
			return quoteRequest(t, "pub", m)
		}(),
		"long request id": func() json.RawMessage {
			b, err := json.Marshal(map[string]any{"id": long, "object": target, "instrument": inlineInstrument(target)})
			require.NoError(t, err)
			return b
		}(),
		"NUL in instrument": func() json.RawMessage {
			m := inlineInstrument(target)
			m["id"] = qrQuoting + "\x00"
			return quoteRequest(t, "pub", m)
		}(),
	} {
		e := newQREnv(t)
		require.NoError(t, e.handle(t, raw), name)
		assert.Empty(t, e.resp.accepted, name)
		assert.Empty(t, e.resp.rejected, name)
	}
}

// こちらが答える筋合いの無いものは黙って捨てる (Reject も返さない)。
func TestQuoteRequest_Ignores(t *testing.T) {
	e := newQREnv(t)
	for name, raw := range map[string]json.RawMessage{
		"pure renote":     quoteRequest(t, "renote", inlineInstrument(qrBase+"/notes/renote")),
		"remote note":     quoteRequest(t, "remote", inlineInstrument(qrBase+"/notes/remote")),
		"missing note":    quoteRequest(t, "nope", inlineInstrument(qrBase+"/notes/nope")),
		"no instrument":   json.RawMessage(`{"id":"x","type":"QuoteRequest","object":"` + qrBase + `/notes/pub"}`),
		"other host":      json.RawMessage(`{"id":"x","object":"https://other.example/notes/pub","instrument":"` + qrQuoting + `"}`),
		"nested path":     json.RawMessage(`{"id":"x","object":"` + qrBase + `/notes/pub/activity","instrument":"` + qrQuoting + `"}`),
		"malformed":       json.RawMessage(`[`),
		"object as {id}":  json.RawMessage(`{"id":"x","object":{"id":"` + qrBase + `/notes/nope"},"instrument":"` + qrQuoting + `"}`),
		"empty object id": json.RawMessage(`{"id":"x","object":"","instrument":"` + qrQuoting + `"}`),
	} {
		require.NoError(t, e.handle(t, raw), name)
	}
	assert.Empty(t, e.resp.accepted)
	assert.Empty(t, e.resp.rejected)
	// ローカルの利用者や URI の無い相手からは受けない。
	local := &model.User{ID: "carl"}
	require.NoError(t, e.h.Handle(local, quoteRequest(t, "pub", inlineInstrument(qrBase+"/notes/pub"))))
	require.NoError(t, e.h.Handle(nil, quoteRequest(t, "pub", inlineInstrument(qrBase+"/notes/pub"))))
	assert.Empty(t, e.resp.accepted)
}

// inline でなければ取得して確かめる。一時的な失敗は error (inbox が再試行する)。
func TestQuoteRequest_FetchesInstrument(t *testing.T) {
	e := newQREnv(t)
	target := qrBase + "/notes/pub"
	e.remote[qrQuoting] = &activitypub.Note{Object: activitypub.Object{ID: qrQuoting}, AttributedTo: qrQuoterURI, MisskeyQuote: activitypub.APLenientID(target)}
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", qrQuoting)))
	assert.Equal(t, []string{qrQuoting}, e.fetched)
	assert.Len(t, e.resp.accepted, 1)

	// Fedibird の quoteUri だけで引用している投稿も確かめられる。
	e = newQREnv(t)
	e.remote[qrQuoting] = &activitypub.Note{Object: activitypub.Object{ID: qrQuoting}, AttributedTo: qrQuoterURI, QuoteURI: activitypub.APLenientID(target)}
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", qrQuoting)))
	assert.Len(t, e.resp.accepted, 1)

	e = newQREnv(t)
	e.fetchEr = errors.New("connection reset")
	err := e.handle(t, quoteRequest(t, "pub", qrQuoting))
	require.Error(t, err)
	assert.Empty(t, e.resp.accepted)
	assert.Empty(t, e.resp.rejected)
}

// 判定できないとき (DB 障害) は通さずに error を返す。
func TestQuoteRequest_Failures(t *testing.T) {
	target := qrBase + "/notes/pub"
	for name, setup := range map[string]func(e *qrEnv){
		"users":   func(e *qrEnv) { e.users.err = errors.New("db down") },
		"blocks":  func(e *qrEnv) { e.blocks.err = errors.New("db down") },
		"approve": func(e *qrEnv) { e.store.err = errors.New("db down") },
	} {
		e := newQREnv(t)
		setup(e)
		require.Error(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(target))), name)
		assert.Empty(t, e.resp.accepted, name)
	}
	e := newQREnv(t)
	e.follows.err = errors.New("db down")
	require.Error(t, e.handle(t, quoteRequest(t, "fol", inlineInstrument(qrBase+"/notes/fol"))))
	e = newQREnv(t)
	require.Error(t, e.handle(t, quoteRequest(t, "dberr", inlineInstrument(qrBase+"/notes/dberr"))))
}

func TestQuoteRequestDeliveryHook_Renders(t *testing.T) {
	urls := activitypub.NewURLBuilder(qrBase)
	r := activitypub.NewRenderer(urls)
	note := &model.Note{ID: "pub", UserID: "alice"}
	req := qrRequestID
	accept := r.RenderQuoteRequestAccept(note, &model.NoteQuoteAuthorization{ID: "qa1", NoteID: "pub", QuotingURI: qrQuoting, RequestID: &req}, qrQuoterURI)
	body, err := json.Marshal(accept)
	require.NoError(t, err)
	var got map[string]any
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "Accept", got["type"])
	assert.Equal(t, qrBase+"/users/alice", got["actor"])
	assert.Equal(t, qrBase+"/notes/pub/quote-authorizations/qa1", got["result"])
	obj := got["object"].(map[string]any)
	assert.Equal(t, qrRequestID, obj["id"])
	assert.Equal(t, "QuoteRequest", obj["type"])
	assert.Equal(t, qrQuoterURI, obj["actor"])
	assert.Equal(t, qrBase+"/notes/pub", obj["object"])
	assert.Equal(t, qrQuoting, obj["instrument"])

	body, err = json.Marshal(r.RenderQuoteRequestReject(note, qrRequestID, qrQuoting, qrQuoterURI))
	require.NoError(t, err)
	got = nil
	require.NoError(t, json.Unmarshal(body, &got))
	assert.Equal(t, "Reject", got["type"])
	assert.NotContains(t, got, "result")
	assert.Equal(t, qrRequestID, got["object"].(map[string]any)["id"])
	assert.Equal(t, fmt.Sprint(qrQuoting), got["object"].(map[string]any)["instrument"])
}

// qrFlipBlocks reports no block for the first `after` checks, then a block by
// the author (ブロックが判定と記録の間に入った状態)。
type qrFlipBlocks struct {
	after, calls int
	errAt        int
}

func (b *qrFlipBlocks) IsBlocked(a, c string) (bool, error) {
	b.calls++
	if b.errAt != 0 && b.calls == b.errAt {
		return false, errors.New("db down")
	}
	return b.calls > b.after && a == "alice", nil
}

// 判定の後、記録の前にブロックされたら、記録した承認を消して Reject する
// (#3234 段階 4。ブロックで承認を消す処理より後に承認ができてしまうのを防ぐ)。
func TestQuoteRequest_BlockRacingWithApproval(t *testing.T) {
	e := newQREnv(t)
	e.h.blocks = &qrFlipBlocks{after: 2}
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(qrBase+"/notes/pub"))))
	assert.Empty(t, e.resp.accepted)
	assert.Len(t, e.resp.rejected, 1)
	assert.Empty(t, e.store.rows, "the approval recorded before the block is removed")

	// 確かめ直しに失敗したら答えずに再試行させる。
	e = newQREnv(t)
	e.h.blocks = &qrFlipBlocks{after: 100, errAt: 3}
	assert.Error(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(qrBase+"/notes/pub"))))
	assert.Empty(t, e.resp.accepted)
}

// Reject するときは、前の試行で記録された承認も消す。
func TestQuoteRequest_RejectRemovesExistingApproval(t *testing.T) {
	e := newQREnv(t)
	e.store.rows = []*model.NoteQuoteAuthorization{{ID: "old", NoteID: "pub", QuoterID: "bob", QuotingURI: qrQuoting}}
	e.blocks.set[[2]string{"alice", "bob"}] = true
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(qrBase+"/notes/pub"))))
	assert.Len(t, e.resp.rejected, 1)
	assert.Empty(t, e.store.rows)

	// 拒否の経路では instrument を確かめていないので、消すのは送ってきた相手の
	// 承認だけ。他人の引用 URI を並べられても、他人の承認は消さない。
	e = newQREnv(t)
	e.store.rows = []*model.NoteQuoteAuthorization{{ID: "victim", NoteID: "pub", QuoterID: "carol", QuotingURI: qrQuoting}}
	e.blocks.set[[2]string{"bob", "alice"}] = true
	require.NoError(t, e.handle(t, quoteRequest(t, "pub", qrQuoting)))
	assert.Len(t, e.resp.rejected, 1)
	require.Len(t, e.store.rows, 1, "someone else's approval is kept")
	assert.Equal(t, "victim", e.store.rows[0].ID)

	e = newQREnv(t)
	e.blocks.set[[2]string{"alice", "bob"}] = true
	e.store.err = errors.New("db down")
	assert.Error(t, e.handle(t, quoteRequest(t, "pub", inlineInstrument(qrBase+"/notes/pub"))))
	assert.Empty(t, e.resp.rejected)
}
