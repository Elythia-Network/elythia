package fedrule

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/elythia-network/elythia/internal/model"
)

// Store is where the rules live.
type Store interface {
	List() ([]*model.FederationRule, error)
}

// Recorder keeps the hits. Record must not block (受信の経路から呼ぶ)。
type Recorder interface {
	Record(Hit)
}

// reloadInterval bounds how long a process keeps a snapshot without another
// process's change reaching it (pubsub が落ちたときの保険)。
const reloadInterval = 5 * time.Minute

// retryInterval is how soon a failed reload is tried again.
const retryInterval = 30 * time.Second

// Service evaluates the rules against inbound activities and notes.
//
// **受信のたびに DB を読まない。** ルールはスナップショットとして持ち、変更の
// 通知 (Invalidate) か reloadInterval の経過で読み直す。読み直しは 1 つの
// goroutine だけが行い、他は古いスナップショットで評価を続ける (受信を DB の
// 遅延で止めない)。
//
// **読み直しに失敗したら前のスナップショットを使い続ける。** 空にすると、DB が
// 瞬断しただけで拒否していたものが全部通る。
type Service struct {
	store    Store
	recorder Recorder
	clock    func() time.Time
	// firstSeen は actor の ID から初めて見た時刻を取り出す (aidx 等)。
	firstSeen func(id string) (time.Time, error)

	set      atomic.Pointer[ruleSet]
	stale    atomic.Bool
	nextLoad atomic.Int64 // unix nano
	loading  sync.Mutex
}

// NewService constructs a Service and loads the rules once.
func NewService(store Store, recorder Recorder, firstSeen func(id string) (time.Time, error)) *Service {
	s := &Service{store: store, recorder: recorder, clock: time.Now, firstSeen: firstSeen}
	s.set.Store(&ruleSet{})
	s.reload()
	return s
}

// SetClockForTest overrides the time source.
func (s *Service) SetClockForTest(fn func() time.Time) { s.clock = fn }

// Invalidate makes the next evaluation reload the rules.
func (s *Service) Invalidate() { s.stale.Store(true) }

// Reload reads the rules now (管理 API が変更の直後に呼ぶ)。
func (s *Service) Reload() error {
	s.loading.Lock()
	defer s.loading.Unlock()
	return s.load()
}

func (s *Service) reload() {
	if !s.loading.TryLock() {
		return
	}
	defer s.loading.Unlock()
	if err := s.load(); err != nil {
		slog.Warn("fedrule: cannot load federation rules; keeping the previous set", "err", err)
	}
}

func (s *Service) load() error {
	if s.store == nil {
		return nil
	}
	// stale を先に下ろす。読んでいる最中に来た変更の通知を取りこぼさない
	// (後から立った stale は次の評価でもう一度読ませる)。
	s.stale.Store(false)
	rules, err := s.store.List()
	if err != nil {
		// 失敗を stale で表すと、障害のあいだ受信のたびに DB を叩き直す。
		// 次に試す時刻だけを決める。
		s.nextLoad.Store(s.clock().Add(retryInterval).UnixNano())
		return err
	}
	s.set.Store(compileRules(rules))
	s.nextLoad.Store(s.clock().Add(reloadInterval).UnixNano())
	return nil
}

func (s *Service) rules() *ruleSet {
	if s.stale.Load() || s.clock().UnixNano() >= s.nextLoad.Load() {
		s.reload()
	}
	return s.set.Load()
}

// HasActivityRules reports whether any activity rule is active.
func (s *Service) HasActivityRules() bool { return len(s.rules().activities) > 0 }

// HasNoteRules reports whether any note rule is active.
func (s *Service) HasNoteRules() bool { return len(s.rules().notes) > 0 }

// ActorFacts builds the Actor for a user row (nil = 初めて見る actor)。
func (s *Service) ActorFacts(u *model.User) Actor {
	if u == nil {
		return Actor{}
	}
	a := Actor{Known: true, IsBot: u.IsBot}
	if s.firstSeen != nil {
		if t, err := s.firstSeen(u.ID); err == nil {
			a.FirstSeen = t
		}
	}
	return a
}

// EvaluateActivity applies the activity rules.
func (s *Service) EvaluateActivity(in ActivityInput) Decision {
	var d Decision
	set := s.rules()
	if len(set.activities) == 0 {
		return d
	}
	now := s.clock()
	for _, c := range set.activities {
		if !c.matchesActivity(in, now) {
			continue
		}
		enforced := c.rule.Mode == model.FederationRuleModeEnforce
		if enforced {
			d.merge(c.rule)
		}
		s.record(Hit{RuleID: c.rule.ID, Host: in.Host, Subject: in.Subject, Kind: in.Type, Applied: enforced, At: now})
	}
	return d
}

// EvaluateNote applies the note rules.
func (s *Service) EvaluateNote(in NoteInput) Decision {
	var d Decision
	set := s.rules()
	if len(set.notes) == 0 {
		return d
	}
	now := s.clock()
	// **初めて見るより前の日付の投稿は、評価した時刻で測る。** 投稿の時刻は AP の
	// published なので、後から取りに行った古い投稿 (ピン留めや返信先) は actor を
	// 初めて見た時刻より前になる。そのまま測ると差が負になり、何年も前から知って
	// いる相手でも「新規」として当たる (#3090 の敵対的レビュー 3 周目で実測)。
	ref := now
	if !in.At.IsZero() && !in.At.Before(in.Actor.FirstSeen) {
		ref = in.At
	}
	kind := "Note"
	if in.Update {
		kind = "Update"
	}
	for _, c := range set.notes {
		if !c.matchesNote(in, ref) {
			continue
		}
		enforced := c.rule.Mode == model.FederationRuleModeEnforce
		if enforced {
			d.merge(c.rule)
		}
		s.record(Hit{RuleID: c.rule.ID, Host: in.Host, Subject: in.Subject, Kind: kind, Applied: enforced, At: now})
	}
	return d
}

func (s *Service) record(h Hit) {
	if s.recorder != nil {
		s.recorder.Record(h)
	}
}
