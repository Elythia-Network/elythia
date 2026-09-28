package fedrule

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/shiroha-a/mk/internal/model"
)

type fakeStore struct {
	mu    sync.Mutex
	rules []*model.FederationRule
	err   error
	calls int
}

func (f *fakeStore) List() ([]*model.FederationRule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.rules, f.err
}

type fakeRecorder struct{ hits []Hit }

func (f *fakeRecorder) Record(h Hit) { f.hits = append(f.hits, h) }

var testNow = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

// firstSeen は ID に埋めた時刻を返す (テストでは "t<unix>" 形式)。
func testFirstSeen(id string) (time.Time, error) {
	switch id {
	case "old":
		return testNow.Add(-72 * time.Hour), nil
	case "fresh":
		return testNow.Add(-2 * time.Hour), nil
	}
	return time.Time{}, errors.New("unknown id")
}

func newTestService(t *testing.T, rules ...*model.FederationRule) (*Service, *fakeStore, *fakeRecorder) {
	t.Helper()
	store := &fakeStore{rules: rules}
	rec := &fakeRecorder{}
	s := &Service{store: store, recorder: rec, clock: func() time.Time { return testNow }, firstSeen: testFirstSeen}
	s.set.Store(&ruleSet{})
	require.NoError(t, s.Reload())
	return s, store, rec
}

func enforce(r *model.FederationRule) *model.FederationRule {
	r.Mode = model.FederationRuleModeEnforce
	return r
}

func TestEvaluateNote_Conditions(t *testing.T) {
	known := Actor{Known: true, FirstSeen: testNow.Add(-72 * time.Hour)}
	base := NoteInput{Host: "sub.spam.example", Actor: known, Text: "hello world", CW: "", Tags: []string{"misskey"}}
	for _, tc := range []struct {
		name  string
		rule  *model.FederationRule
		in    func(n *NoteInput)
		match bool
	}{
		{"host suffix", &model.FederationRule{Hosts: model.StringArray{"spam.example"}}, nil, true},
		{"other host", &model.FederationRule{Hosts: model.StringArray{"other.example"}}, nil, false},
		{"bot wanted, not bot", &model.FederationRule{IsBot: ptr(true)}, nil, false},
		{"bot wanted, bot", &model.FederationRule{IsBot: ptr(true)}, func(n *NoteInput) { n.Actor.IsBot = true }, true},
		{"non-bot wanted", &model.FederationRule{IsBot: ptr(false)}, nil, true},
		{"bot unknown actor", &model.FederationRule{IsBot: ptr(false)}, func(n *NoteInput) { n.Actor = Actor{} }, false},
		{"new, old actor", &model.FederationRule{NewWithinHours: ptr(24)}, nil, false},
		{"new, fresh actor", &model.FederationRule{NewWithinHours: ptr(24)}, func(n *NoteInput) { n.Actor.FirstSeen = testNow.Add(-time.Hour) }, true},
		{"new, unknown actor", &model.FederationRule{NewWithinHours: ptr(24)}, func(n *NoteInput) { n.Actor = Actor{} }, true},
		{"new, unparsable id", &model.FederationRule{NewWithinHours: ptr(24)}, func(n *NoteInput) { n.Actor.FirstSeen = time.Time{} }, false},
		{"new, exactly at the edge", &model.FederationRule{NewWithinHours: ptr(72)}, nil, false},
		{"new, measured at the note's time", &model.FederationRule{NewWithinHours: ptr(24)}, func(n *NoteInput) { n.At = testNow.Add(-71 * time.Hour) }, true},
		{"pattern in text", &model.FederationRule{Patterns: model.StringArray{"world hello"}}, nil, true},
		{"pattern in cw", &model.FederationRule{Patterns: model.StringArray{"/^sp[a]m$/"}}, func(n *NoteInput) { n.CW = "spam" }, true},
		{"pattern miss", &model.FederationRule{Patterns: model.StringArray{"buy"}}, nil, false},
		{"pattern in poll choice", &model.FederationRule{Patterns: model.StringArray{"buy"}}, func(n *NoteInput) { n.PollChoices = []string{"ok", "buy it"} }, true},
		{"attachment wanted", &model.FederationRule{HasAttachment: ptr(true)}, nil, false},
		{"attachment present", &model.FederationRule{HasAttachment: ptr(true)}, func(n *NoteInput) { n.HasAttachment = true }, true},
		{"no attachment wanted", &model.FederationRule{HasAttachment: ptr(false)}, nil, true},
		{"tag", &model.FederationRule{Tags: model.StringArray{"ad", "misskey"}}, nil, true},
		{"tag miss", &model.FederationRule{Tags: model.StringArray{"ad"}}, nil, false},
		{"all conditions (AND)", &model.FederationRule{Hosts: model.StringArray{"spam.example"}, Patterns: model.StringArray{"buy"}}, nil, false},
		{"host missing", &model.FederationRule{Hosts: model.StringArray{"spam.example"}}, func(n *NoteInput) { n.Host = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.rule.ID, tc.rule.Target, tc.rule.Reject = "r", model.FederationRuleTargetNote, true
			s, _, rec := newTestService(t, enforce(tc.rule))
			in := base
			if tc.in != nil {
				tc.in(&in)
			}
			assert.Equal(t, tc.match, s.EvaluateNote(in).Reject)
			assert.Equal(t, tc.match, len(rec.hits) == 1)
		})
	}
}

// record は記録だけ、enforce は効かせる。disabled は数えもしない。拒否が
// 最優先で、書き換えは合わせる。CW は評価順で最初のもの。
func TestEvaluateNote_ModesAndComposition(t *testing.T) {
	host := model.StringArray{"spam.example"}
	rules := []*model.FederationRule{
		{ID: "off", Mode: model.FederationRuleModeDisabled, Target: model.FederationRuleTargetNote, Hosts: host, Reject: true},
		{ID: "rec", Mode: model.FederationRuleModeRecord, Target: model.FederationRuleTargetNote, Hosts: host, Reject: true},
		{ID: "cw1", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote, Hosts: host, CW: ptr("first"), Sensitive: true},
		{ID: "cw2", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote, Hosts: host, CW: ptr("second"), Unlist: true},
		{ID: "strip", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote, Hosts: host, StripMedia: true},
		{ID: "act", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetActivity, ActivityTypes: model.StringArray{"Create"}, Reject: true},
	}
	s, _, rec := newTestService(t, rules...)
	d := s.EvaluateNote(NoteInput{Host: "spam.example", Subject: "https://spam.example/n/1"})
	assert.False(t, d.Reject, "a record-only rule does not reject")
	assert.True(t, d.Sensitive && d.Unlist && d.StripMedia)
	require.NotNil(t, d.CW)
	assert.Equal(t, "first", *d.CW)
	require.Len(t, rec.hits, 4, "disabled rules and activity rules are not evaluated for notes")
	assert.Equal(t, Hit{RuleID: "rec", Host: "spam.example", Subject: "https://spam.example/n/1", Kind: "Note", Applied: false, At: testNow}, rec.hits[0])
	assert.True(t, rec.hits[1].Applied)

	// CW の文言は返り値を書き換えてもルールに影響しない。
	*d.CW = "changed"
	d = s.EvaluateNote(NoteInput{Host: "spam.example", Update: true})
	assert.Equal(t, "first", *d.CW)
	assert.Equal(t, "Update", rec.hits[len(rec.hits)-1].Kind)

	rules[1].Mode = model.FederationRuleModeEnforce
	require.NoError(t, s.Reload())
	d = s.EvaluateNote(NoteInput{Host: "spam.example"})
	assert.True(t, d.Reject)
	assert.True(t, d.Any())
	assert.False(t, Decision{}.Any())
}

func TestEvaluateActivity(t *testing.T) {
	rules := []*model.FederationRule{
		{ID: "follow", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetActivity,
			ActivityTypes: model.StringArray{"Follow"}, NewWithinHours: ptr(24), Reject: true},
		{ID: "react", Mode: model.FederationRuleModeRecord, Target: model.FederationRuleTargetActivity,
			ActivityTypes: model.StringArray{"EmojiReact", "Like"}, Hosts: model.StringArray{"spam.example"}, Reject: true},
		{ID: "note", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote, Hosts: model.StringArray{"spam.example"}, Reject: true},
	}
	s, _, rec := newTestService(t, rules...)
	fresh := s.ActorFacts(&model.User{ID: "fresh"})
	old := s.ActorFacts(&model.User{ID: "old", IsBot: true})
	assert.Equal(t, Actor{Known: true, FirstSeen: testNow.Add(-2 * time.Hour)}, fresh)
	assert.True(t, old.IsBot)
	assert.Equal(t, Actor{}, s.ActorFacts(nil))
	assert.True(t, s.ActorFacts(&model.User{ID: "zzz"}).FirstSeen.IsZero())

	assert.True(t, s.EvaluateActivity(ActivityInput{Host: "a.example", Type: "Follow", Actor: fresh}).Reject)
	assert.True(t, s.EvaluateActivity(ActivityInput{Host: "a.example", Type: "follow", Actor: Actor{}}).Reject, "an unseen actor is new")
	assert.False(t, s.EvaluateActivity(ActivityInput{Host: "a.example", Type: "Follow", Actor: old}).Reject)
	assert.False(t, s.EvaluateActivity(ActivityInput{Host: "spam.example", Type: "Create", Actor: fresh}).Reject, "note rules are not activity rules")
	n := len(rec.hits)
	assert.False(t, s.EvaluateActivity(ActivityInput{Host: "spam.example", Type: "EmojiReaction", Actor: old}).Reject)
	require.Len(t, rec.hits, n+1, "EmojiReaction is the same activity as EmojiReact")
	assert.Equal(t, "react", rec.hits[n].RuleID)
	assert.False(t, rec.hits[n].Applied)
}

// 読み直し: 失敗したら前のスナップショットを使い続け、すぐには叩き直さない。
// Invalidate で次の評価が読み直す。時間が経てば読み直す。
func TestService_Reload(t *testing.T) {
	rule := enforce(&model.FederationRule{ID: "r", Target: model.FederationRuleTargetNote, Hosts: model.StringArray{"spam.example"}, Reject: true})
	s, store, _ := newTestService(t, rule)
	now := testNow
	s.SetClockForTest(func() time.Time { return now })
	in := NoteInput{Host: "spam.example"}
	require.True(t, s.EvaluateNote(in).Reject)
	calls := store.calls

	s.EvaluateNote(in)
	assert.Equal(t, calls, store.calls, "no reload while the snapshot is fresh")

	store.err = errors.New("db down")
	s.Invalidate()
	assert.True(t, s.EvaluateNote(in).Reject, "a failed reload keeps the previous rules")
	assert.Equal(t, calls+1, store.calls)
	s.EvaluateNote(in)
	assert.Equal(t, calls+1, store.calls, "a failed reload is not retried on every evaluation")
	now = now.Add(retryInterval)
	s.EvaluateNote(in)
	assert.Equal(t, calls+2, store.calls, "retried after the interval")

	store.err = nil
	store.rules = nil
	now = now.Add(retryInterval)
	assert.False(t, s.EvaluateNote(in).Reject)
	calls = store.calls
	now = now.Add(reloadInterval - time.Second)
	s.EvaluateNote(in)
	assert.Equal(t, calls, store.calls)
	now = now.Add(time.Second)
	s.EvaluateNote(in)
	assert.Equal(t, calls+1, store.calls, "reloaded after reloadInterval even without a notice")

	// NewService は最初に 1 度読む。store が nil でも動く。
	s2 := NewService(store, nil, nil)
	store.rules = []*model.FederationRule{rule}
	assert.False(t, s2.EvaluateNote(in).Reject)
	s2.Invalidate()
	assert.True(t, s2.EvaluateNote(in).Reject)
	assert.Equal(t, Actor{Known: true}, s2.ActorFacts(&model.User{ID: "x"}))
	s3 := NewService(nil, nil, nil)
	assert.NoError(t, s3.Reload())
	assert.False(t, s3.EvaluateActivity(ActivityInput{Type: "Follow"}).Reject)
}

// 検証を通っていない不正な regex が DB にあっても、そのルールを丸ごと外す
// (パターンだけ外すと条件が緩む)。
func TestCompileRules_DropsBrokenRule(t *testing.T) {
	set := compileRules([]*model.FederationRule{
		{ID: "bad", Mode: model.FederationRuleModeEnforce, Target: model.FederationRuleTargetNote,
			Hosts: model.StringArray{"spam.example"}, Patterns: model.StringArray{"/(x/"}, Reject: true},
		{ID: "unknown", Mode: model.FederationRuleModeEnforce, Target: "user", Reject: true},
	})
	assert.Empty(t, set.notes)
	assert.Empty(t, set.activities)
}

// 初めて見るより前の日付の投稿 (後から取りに行った古い投稿) の編集は、評価した
// 時刻で測る。投稿の時刻で測ると差が負になり、古くから知っている相手が新規に
// 当たる。
func TestEvaluateNote_AtBeforeFirstSeenFallsBackToNow(t *testing.T) {
	s, _, _ := newTestService(t, enforce(&model.FederationRule{ID: "r", Target: model.FederationRuleTargetNote,
		NewWithinHours: ptr(24), Reject: true}))
	old := Actor{Known: true, FirstSeen: testNow.Add(-365 * 24 * time.Hour)}
	assert.False(t, s.EvaluateNote(NoteInput{Actor: old, At: testNow.Add(-2 * 365 * 24 * time.Hour)}).Reject)
	fresh := Actor{Known: true, FirstSeen: testNow.Add(-72 * time.Hour)}
	assert.True(t, s.EvaluateNote(NoteInput{Actor: fresh, At: testNow.Add(-71 * time.Hour)}).Reject,
		"a note posted while the actor was new is still measured at its post time")
}
