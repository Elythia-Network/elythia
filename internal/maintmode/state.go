// Package maintmode holds the maintenance mode shared by every Elythia
// process: the state kept in Redis, the per-process reports, the judge that
// decides from pg_stat_activity that the database has been let go, and the
// HTTP handler that answers meanwhile (#3463).
//
// 名前は internal/maintenance (backfill のバッチ) とキューの maintenance を
// 避けて maintmode にした。Redis のキーと API の名前は maintenance のまま。
package maintmode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// Phases of a maintenance, as stored in the state hash.
const (
	// PhaseEntering asks every process to let go of the database.
	PhaseEntering = "entering"
	// PhaseHeld means the judge saw no session on the database. It is a fact
	// of the moment it was judged; the judge keeps listing what connects
	// afterwards in State.Blockers.
	PhaseHeld = "held"
	// PhaseResuming asks every process to reopen the database and serve
	// again. The state is deleted once they all run.
	PhaseResuming = "resuming"
)

// Kinds of maintenance.
const (
	KindManual   = "manual"
	KindRestore  = "restore"
	KindRollback = "rollback"
)

// Process states reported under the proc keys.
const (
	// ProcStarting is written before the process reads the state and opens
	// the database, so a maintenance entered meanwhile waits for it.
	ProcStarting     = "starting"
	ProcRunning      = "running"
	ProcDraining     = "draining"
	ProcDrained      = "drained"
	ProcResuming     = "resuming"
	ProcResumeFailed = "resume_failed"
)

// 終了するプロセスは報告を消すだけにする (消し損ねても ProcTTL で消える)。
// 以前の stopping / left は、報告の順序に判定の正しさを預ける形で、レビューの
// たびに順序の穴が出た (#3463 の設計第2版 §1)。今は判定の根拠を
// pg_stat_activity に置き、報告は「まだ待つ理由」を足すだけに使う。

// Channel is the pubsub channel announcing a state change. Subscribers only
// use it as a hint to re-read the state hash.
//
// 既存の internal: の購読 (router.go) と同じ命名にした。中身は読まず、
// 受けたら hash を読み直すだけなので、接頭辞の無い名前が別のインスタンスと
// 重なっても、余分に 1 回読むだけで済む。
const Channel = "internal:maintenance"

// ProcTTL is how long a process report lives without being refreshed.
// Processes refresh it every PollInterval.
const ProcTTL = 10 * time.Second

// PollInterval is how often a process re-reads the state besides the pubsub
// hint, and refreshes its report. pubsub は取りこぼすので、読み直しを併用する。
const PollInterval = 2 * time.Second

// ErrActive is returned by Enter when a maintenance is already active.
var ErrActive = errors.New("maintmode: a maintenance is already active")

// ErrOpMismatch is returned when an operation names an opId that is not the
// active one.
var ErrOpMismatch = errors.New("maintmode: the active maintenance has a different opId")

// ErrNotActive is returned by SetPhase when no maintenance is active.
var ErrNotActive = errors.New("maintmode: no maintenance is active")

// State is the maintenance currently requested. A nil *State means the
// processes should serve normally.
type State struct {
	Phase       string    `json:"phase"`
	OpID        string    `json:"opId"`
	Kind        string    `json:"kind"`
	Reason      string    `json:"reason"`
	StartedAt   time.Time `json:"startedAt"`
	RequestedBy string    `json:"requestedBy"`
	// LeaseUntil is refreshed by the backup service while it drives an
	// operation (#3463 (6c)). Zero for a manual maintenance.
	LeaseUntil time.Time `json:"leaseUntil"`

	// The fields below are written by the judge.

	// HeldAt is when the judge moved the maintenance to held.
	HeldAt time.Time `json:"heldAt"`
	// JudgedAt is when the judge last looked.
	JudgedAt time.Time `json:"judgedAt"`
	// WaitingFor lists the processes whose report is not drained yet.
	WaitingFor []string `json:"waitingFor"`
	// Blockers are the sessions on the database at JudgedAt. While held, a
	// non-empty list means something connected after the maintenance was
	// judged held.
	Blockers []Blocker `json:"blockers"`
	// Terminated are the Elythia sessions the judge ended after the drain
	// timeout.
	Terminated []Blocker `json:"terminated"`
	// JudgeError is the last error of the judge (e.g. it could not reach
	// PostgreSQL), empty when the last look succeeded.
	JudgeError string `json:"judgeError"`
}

// Holding reports whether the processes should keep away from the database.
func (s *State) Holding() bool {
	return s != nil && s.Phase != PhaseResuming
}

// ProcReport is what one process says about itself.
//
// OpenConns / InFlight / Error are shown by `elythia maintenance status` and
// are never used to judge that the database has been let go.
type ProcReport struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Version   string    `json:"version"`
	Commit    string    `json:"commit"`
	State     string    `json:"state"`
	OpenConns int       `json:"openConns"`
	InFlight  int64     `json:"inFlight"`
	Error     string    `json:"error"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Store reads and writes the maintenance state in Redis.
type Store struct {
	rdb    *redis.Client
	pub    *redis.Client
	prefix string
}

// NewStore returns a Store keeping its keys under prefix (the configured
// redis.prefix plus ':', as config.RedisOptions.KeyPrefix returns). rdb
// holds the keys; pub carries the change notifications.
func NewStore(rdb, pub *redis.Client, prefix string) *Store {
	if pub == nil {
		pub = rdb
	}
	return &Store{rdb: rdb, pub: pub, prefix: prefix}
}

// StateKey is the hash holding the requested maintenance.
func (s *Store) StateKey() string { return s.prefix + "elythia:maintenance" }

// ProcKey is the report key of one process.
func (s *Store) ProcKey(id string) string { return s.prefix + "elythia:maintenance:proc:" + id }

// Get returns the active maintenance, or nil when there is none.
func (s *Store) Get(ctx context.Context) (*State, error) {
	m, err := s.rdb.HGetAll(ctx, s.StateKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("maintmode: read state: %w", err)
	}
	if len(m) == 0 {
		return nil, nil
	}
	st := &State{
		Phase:       m["phase"],
		OpID:        m["opId"],
		Kind:        m["kind"],
		Reason:      m["reason"],
		RequestedBy: m["requestedBy"],
		StartedAt:   parseTime(m["startedAt"]),
		LeaseUntil:  parseTime(m["leaseUntil"]),
		HeldAt:      parseTime(m["heldAt"]),
		JudgedAt:    parseTime(m["judgedAt"]),
		JudgeError:  m["judgeError"],
	}
	// 判定役が書く欄は表示のためのもの。壊れていたら空として読む。
	_ = json.Unmarshal([]byte(m["waitingFor"]), &st.WaitingFor)
	_ = json.Unmarshal([]byte(m["blockers"]), &st.Blockers)
	_ = json.Unmarshal([]byte(m["terminated"]), &st.Terminated)
	if st.Phase == "" {
		// phase の無い hash は、書きかけか壊れたもの。安全側に倒して
		// メンテナンス中として扱う。
		st.Phase = PhaseEntering
	}
	return st, nil
}

// Enter starts a maintenance. It fails with ErrActive when one is already
// active, so two operators cannot overwrite each other.
func (s *Store) Enter(ctx context.Context, st State) error {
	if st.Phase == "" {
		st.Phase = PhaseEntering
	}
	if st.StartedAt.IsZero() {
		st.StartedAt = time.Now()
	}
	// phase だけを HSETNX で先に置き、取れた側だけが残りを書く。
	ok, err := s.rdb.HSetNX(ctx, s.StateKey(), "phase", st.Phase).Result()
	if err != nil {
		return fmt.Errorf("maintmode: write state: %w", err)
	}
	if !ok {
		return ErrActive
	}
	fields := map[string]any{
		"opId":        st.OpID,
		"kind":        st.Kind,
		"reason":      st.Reason,
		"requestedBy": st.RequestedBy,
		"startedAt":   formatTime(st.StartedAt),
	}
	if !st.LeaseUntil.IsZero() {
		fields["leaseUntil"] = formatTime(st.LeaseUntil)
	}
	if err := s.rdb.HSet(ctx, s.StateKey(), fields).Err(); err != nil {
		return fmt.Errorf("maintmode: write state: %w", err)
	}
	s.notify(ctx)
	return nil
}

// setPhaseScript moves the state to ARGV[2] when it exists and, unless
// ARGV[1] is empty, its opId is ARGV[1]. 0: no state, -1: opId mismatch.
//
// 読んで比べてから書く形にすると、その間に exit と enter がやり直されたときに
// 新しいメンテナンスの phase を書き換える。比べるのと書くのを 1 つにする。
var setPhaseScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if ARGV[1] ~= '' and redis.call('HGET', KEYS[1], 'opId') ~= ARGV[1] then return -1 end
redis.call('HSET', KEYS[1], 'phase', ARGV[2])
return 1`)

// SetPhase moves the active maintenance to phase. opID must match the
// active one unless it is empty.
func (s *Store) SetPhase(ctx context.Context, opID, phase string) error {
	n, err := setPhaseScript.Run(ctx, s.rdb, []string{s.StateKey()}, opID, phase).Int()
	if err != nil {
		return fmt.Errorf("maintmode: write state: %w", err)
	}
	switch n {
	case 0:
		return ErrNotActive
	case -1:
		return ErrOpMismatch
	}
	s.notify(ctx)
	return nil
}

// clearScript deletes the state when, unless ARGV[1] is empty, its opId is
// ARGV[1], and, unless ARGV[2] is empty, its phase is ARGV[2]. 1: deleted,
// 0: nothing to delete, -1: opId or phase mismatch.
var clearScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if ARGV[1] ~= '' and redis.call('HGET', KEYS[1], 'opId') ~= ARGV[1] then return -1 end
if ARGV[2] ~= '' and redis.call('HGET', KEYS[1], 'phase') ~= ARGV[2] then return -1 end
redis.call('DEL', KEYS[1])
return 1`)

// Clear ends the maintenance. opID must match the active one unless it is
// empty. Clearing when nothing is active is not an error.
func (s *Store) Clear(ctx context.Context, opID string) error {
	n, err := clearScript.Run(ctx, s.rdb, []string{s.StateKey()}, opID, "").Int()
	if err != nil {
		return fmt.Errorf("maintmode: clear state: %w", err)
	}
	if n == -1 {
		return ErrOpMismatch
	}
	s.notify(ctx)
	return nil
}

// clearResumed deletes the state when it is still opID at resuming. It
// reports whether it deleted it.
func (s *Store) clearResumed(ctx context.Context, opID string) (bool, error) {
	n, err := clearScript.Run(ctx, s.rdb, []string{s.StateKey()}, opID, PhaseResuming).Int()
	if err != nil {
		return false, fmt.Errorf("maintmode: clear state: %w", err)
	}
	if n == 1 {
		s.notify(ctx)
	}
	return n == 1, nil
}

// markHeldScript moves the state from entering to held when its opId is
// ARGV[1], and records the judge's view. 1: moved, 0: not moved.
var markHeldScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'phase') ~= 'entering' then return 0 end
if redis.call('HGET', KEYS[1], 'opId') ~= ARGV[1] then return 0 end
redis.call('HSET', KEYS[1], 'phase', 'held', 'heldAt', ARGV[2], 'judgedAt', ARGV[2],
	'waitingFor', '[]', 'blockers', '[]', 'terminated', ARGV[3], 'judgeError', '')
return 1`)

// markHeld moves opID from entering to held. It reports whether it did: a
// state that changed meanwhile (another opId, or not entering) is left as
// it is.
func (s *Store) markHeld(ctx context.Context, opID string, at time.Time, terminated []Blocker) (bool, error) {
	n, err := markHeldScript.Run(ctx, s.rdb, []string{s.StateKey()}, opID, formatTime(at), mustJSON(terminated)).Int()
	if err != nil {
		return false, fmt.Errorf("maintmode: write state: %w", err)
	}
	if n == 1 {
		s.notify(ctx)
	}
	return n == 1, nil
}

// progressScript records the judge's view on the state when its opId is
// ARGV[1] and its phase is entering or held.
var progressScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], 'opId') ~= ARGV[1] then return 0 end
local ph = redis.call('HGET', KEYS[1], 'phase')
if ph ~= 'entering' and ph ~= 'held' then return 0 end
redis.call('HSET', KEYS[1], 'judgedAt', ARGV[2], 'waitingFor', ARGV[3], 'blockers', ARGV[4],
	'terminated', ARGV[5], 'judgeError', ARGV[6])
return 1`)

// progress records what the judge saw on opID.
func (s *Store) progress(ctx context.Context, opID string, at time.Time, waitingFor []string, blockers, terminated []Blocker, judgeErr string) error {
	err := progressScript.Run(ctx, s.rdb, []string{s.StateKey()}, opID, formatTime(at),
		mustJSON(waitingFor), mustJSON(blockers), mustJSON(terminated), judgeErr).Err()
	if err != nil {
		return fmt.Errorf("maintmode: write state: %w", err)
	}
	return nil
}

// JudgeKey is the lock naming the judge.
func (s *Store) JudgeKey() string { return s.prefix + "elythia:maintenance:judge" }

// acquireJudgeScript takes the judge lock for ARGV[1] or extends it when
// ARGV[1] already holds it, for ARGV[2] milliseconds.
var acquireJudgeScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v == ARGV[1] then redis.call('PEXPIRE', KEYS[1], ARGV[2]) return 1 end
if not v then redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2]) return 1 end
return 0`)

// acquireJudge takes or extends the judge lock for id. It reports whether id
// holds it.
func (s *Store) acquireJudge(ctx context.Context, id string, ttl time.Duration) (bool, error) {
	n, err := acquireJudgeScript.Run(ctx, s.rdb, []string{s.JudgeKey()}, id, ttl.Milliseconds()).Int()
	if err != nil {
		return false, fmt.Errorf("maintmode: take the judge lock: %w", err)
	}
	return n == 1, nil
}

var releaseJudgeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0`)

// releaseJudge gives up the judge lock if id holds it.
func (s *Store) releaseJudge(ctx context.Context, id string) error {
	return releaseJudgeScript.Run(ctx, s.rdb, []string{s.JudgeKey()}, id).Err()
}

// Judge returns the ID of the current judge, or "" when there is none.
func (s *Store) Judge(ctx context.Context) (string, error) {
	v, err := s.rdb.Get(ctx, s.JudgeKey()).Result()
	if errors.Is(err, redis.Nil) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("maintmode: read the judge lock: %w", err)
	}
	return v, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return "[]"
	}
	return string(b)
}

// notify publishes the change hint. 取りこぼしても各プロセスは PollInterval で
// 読み直すので、失敗は返さない。
func (s *Store) notify(ctx context.Context) {
	_ = s.pub.Publish(ctx, Channel, "changed").Err()
}

// Report writes the report of one process with ProcTTL.
func (s *Store) Report(ctx context.Context, r ProcReport) error {
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = time.Now()
	}
	key := s.ProcKey(r.ID)
	// 全部の欄を毎回書くので、前の報告の欄は残らない。
	pipe := s.rdb.TxPipeline()
	pipe.HSet(ctx, key, map[string]any{
		"role":      r.Role,
		"version":   r.Version,
		"commit":    r.Commit,
		"state":     r.State,
		"openConns": strconv.Itoa(r.OpenConns),
		"inFlight":  strconv.FormatInt(r.InFlight, 10),
		"error":     r.Error,
		"updatedAt": formatTime(r.UpdatedAt),
	})
	pipe.Expire(ctx, key, ProcTTL)
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("maintmode: write report: %w", err)
	}
	return nil
}

// RemoveReport deletes the report of one process.
func (s *Store) RemoveReport(ctx context.Context, id string) error {
	return s.rdb.Del(ctx, s.ProcKey(id)).Err()
}

// Reports returns every live process report, sorted by ID.
func (s *Store) Reports(ctx context.Context) ([]ProcReport, error) {
	prefix := s.ProcKey("")
	var out []ProcReport
	iter := s.rdb.Scan(ctx, 0, prefix+"*", 100).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		m, err := s.rdb.HGetAll(ctx, key).Result()
		if err != nil {
			return nil, fmt.Errorf("maintmode: read report: %w", err)
		}
		if len(m) == 0 {
			continue // SCAN と HGETALL の間に期限が切れた
		}
		openConns, _ := strconv.Atoi(m["openConns"])
		inFlight, _ := strconv.ParseInt(m["inFlight"], 10, 64)
		out = append(out, ProcReport{
			ID:        strings.TrimPrefix(key, prefix),
			Role:      m["role"],
			Version:   m["version"],
			Commit:    m["commit"],
			State:     m["state"],
			OpenConns: openConns,
			InFlight:  inFlight,
			Error:     m["error"],
			UpdatedAt: parseTime(m["updatedAt"]),
		})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("maintmode: list reports: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// EvictionPolicy returns the maxmemory-policy of the Redis holding the state.
// Managed Redis services often refuse CONFIG; the error is then returned.
func (s *Store) EvictionPolicy(ctx context.Context) (string, error) {
	m, err := s.rdb.ConfigGet(ctx, "maxmemory-policy").Result()
	if err != nil {
		return "", fmt.Errorf("maintmode: read maxmemory-policy: %w", err)
	}
	return m["maxmemory-policy"], nil
}

// EvictsState reports whether a Redis with policy may evict the state key.
//
// 状態のキーは TTL を持たないので、volatile-* では追い出されない。allkeys-* だと
// メモリが足りなくなったときに追い出され、メンテナンスが勝手に終わる。
func EvictsState(policy string) bool {
	return strings.HasPrefix(policy, "allkeys-")
}

// Subscribe returns a channel that receives a value whenever the change hint
// arrives, until ctx is done.
func (s *Store) Subscribe(ctx context.Context) <-chan struct{} {
	out := make(chan struct{}, 1)
	sub := s.pub.Subscribe(ctx, Channel)
	go func() {
		defer close(out)
		defer func() { _ = sub.Close() }()
		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				select {
				case out <- struct{}{}:
				default:
				}
			}
		}
	}()
	return out
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(v string) time.Time {
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
