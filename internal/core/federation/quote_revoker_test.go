package federation

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/activitypub"
	"github.com/elythia-network/elythia/internal/model"
)

type qvApprovals struct {
	rows  []model.NoteQuoteAuthorization
	asked [][2]string
	err   error
}

func (a *qvApprovals) DeleteByAuthorAndQuoter(authorID, quoterID string) ([]model.NoteQuoteAuthorization, error) {
	a.asked = append(a.asked, [2]string{authorID, quoterID})
	return a.rows, a.err
}

type qvDeletes struct {
	sent []string // "<author> <quoter> <approval>"
	err  error
}

func (d *qvDeletes) SendApprovalDelete(author, quoter *model.User, a *model.NoteQuoteAuthorization) error {
	d.sent = append(d.sent, author.ID+" "+quoter.ID+" "+a.ID)
	return d.err
}

type qvUpdates struct {
	sent []string
	err  error
}

func (u *qvUpdates) SendNoteUpdate(note *model.Note, author *model.User) error {
	u.sent = append(u.sent, note.ID+" "+author.ID)
	return u.err
}

type qvEnv struct {
	r       *QuoteRevoker
	approvs *qvApprovals
	deletes *qvDeletes
	updates *qvUpdates
	users   qrUsers
	notes   qrNotes
}

func newQVEnv() *qvEnv {
	host, uri := "remote.example", "https://remote.example/users/bob"
	e := &qvEnv{
		approvs: &qvApprovals{},
		deletes: &qvDeletes{},
		updates: &qvUpdates{},
		users: qrUsers{users: map[string]*model.User{
			"alice": {ID: "alice"},
			"dave":  {ID: "dave"},
			"bob":   {ID: "bob", Host: &host, URI: &uri},
		}},
		notes: qrNotes{"dq": {ID: "dq", UserID: "dave"}},
	}
	e.r = NewQuoteRevoker(e.approvs, e.users, e.notes, e.deletes, e.updates, activitypub.NewURLBuilder(qrBase))
	return e
}

// ローカルの作者がリモートの相手をブロックしたら、その相手の引用に出した承認を
// 取り消す。相手のサーバーが引用する投稿を配り直すので、こちらは Delete だけ。
func TestQuoteRevoker_RemoteQuoter(t *testing.T) {
	e := newQVEnv()
	e.approvs.rows = []model.NoteQuoteAuthorization{
		{ID: "a1", NoteID: "n1", QuoterID: "bob", QuotingURI: "https://remote.example/notes/1"},
		{ID: "a2", NoteID: "n2", QuoterID: "bob", QuotingURI: "https://remote.example/notes/2"},
	}
	e.r.RevokeQuotesOnBlock("alice", "bob")
	assert.Equal(t, [][2]string{{"alice", "bob"}}, e.approvs.asked)
	assert.Equal(t, []string{"alice bob a1", "alice bob a2"}, e.deletes.sent)
	assert.Empty(t, e.updates.sent)
}

// 相手もローカルなら、引用する投稿の Update (承認なし) もこちらが配り直す。
func TestQuoteRevoker_LocalQuoter(t *testing.T) {
	e := newQVEnv()
	e.approvs.rows = []model.NoteQuoteAuthorization{
		{ID: "a1", NoteID: "n1", QuoterID: "dave", QuotingURI: qrBase + "/notes/dq"},
		{ID: "a2", NoteID: "n2", QuoterID: "dave", QuotingURI: qrBase + "/notes/gone"},
		{ID: "a3", NoteID: "n3", QuoterID: "dave", QuotingURI: "https://elsewhere.example/notes/x"},
		// こちらの投稿でも、相手本人のものでなければ配り直さない。
		{ID: "a4", NoteID: "n4", QuoterID: "dave", QuotingURI: qrBase + "/notes/aq"},
	}
	e.notes["aq"] = &model.Note{ID: "aq", UserID: "alice"}
	e.r.RevokeQuotesOnBlock("alice", "dave")
	assert.Equal(t, []string{"alice dave a1", "alice dave a2", "alice dave a3", "alice dave a4"}, e.deletes.sent)
	assert.Equal(t, []string{"dq dave"}, e.updates.sent)
}

func TestQuoteRevoker_Skips(t *testing.T) {
	// ブロックしたのがリモートの利用者なら、こちらの承認ではないので何もしない。
	e := newQVEnv()
	e.approvs.rows = []model.NoteQuoteAuthorization{{ID: "a1", NoteID: "n1", QuoterID: "alice"}}
	e.r.RevokeQuotesOnBlock("bob", "alice")
	assert.Empty(t, e.approvs.asked)
	assert.Empty(t, e.deletes.sent)

	// 承認が無ければ何も送らない。
	e = newQVEnv()
	e.r.RevokeQuotesOnBlock("alice", "bob")
	assert.Len(t, e.approvs.asked, 1)
	assert.Empty(t, e.deletes.sent)

	// 引けない / 消せないときも止まるだけ (ブロックは成立させる)。
	for name, setup := range map[string]func(e *qvEnv){
		"blocker lookup": func(e *qvEnv) { e.users.err = errors.New("db down") },
		"remove":         func(e *qvEnv) { e.approvs.err = errors.New("db down") },
		"blockee gone":   func(e *qvEnv) { delete(e.users.users, "bob") },
	} {
		e := newQVEnv()
		e.approvs.rows = []model.NoteQuoteAuthorization{{ID: "a1", NoteID: "n1", QuoterID: "bob"}}
		setup(e)
		e.r = NewQuoteRevoker(e.approvs, e.users, e.notes, e.deletes, e.updates, activitypub.NewURLBuilder(qrBase))
		require.NotPanics(t, func() { e.r.RevokeQuotesOnBlock("alice", "bob") }, name)
		assert.Empty(t, e.deletes.sent, name)
	}

	// 送れなかった承認があっても、残りは送る。
	e = newQVEnv()
	e.approvs.rows = []model.NoteQuoteAuthorization{
		{ID: "a1", NoteID: "n1", QuoterID: "dave", QuotingURI: qrBase + "/notes/dq"},
		{ID: "a2", NoteID: "n2", QuoterID: "dave", QuotingURI: qrBase + "/notes/dq"},
	}
	e.deletes.err = errors.New("queue down")
	e.updates.err = errors.New("queue down")
	e.r.RevokeQuotesOnBlock("alice", "dave")
	assert.Len(t, e.deletes.sent, 2)
	assert.Len(t, e.updates.sent, 2)
}
