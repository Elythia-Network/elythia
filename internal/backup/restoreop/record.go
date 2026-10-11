// Package restoreop runs a restore requested from the admin page (#3463
// (6c)): it restores a generation into a new database while the server keeps
// running, puts every process into maintenance, swaps the database names,
// cleans Redis, checks the result and lets the processes resume. Each step
// is recorded in Redis before it runs, so that a backup service that
// restarts in the middle can put things back (Recover).
//
// 名前を入れ替えてよいかは、PostgreSQL に確かめさせる (メンテナンスの判定役の
// held と、入れ替える直前の pg_stat_activity、接続のある DB の名前の変更を
// PostgreSQL 自身が拒むこと)。各プロセスの報告は、どこでも根拠にしない。
// 前の段階 (6a)(6b) のレビューで、自己申告と停止の順序に頼った判定から High が
// 繰り返し出たため。
package restoreop

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Steps of an operation, as recorded.
const (
	// StepPreparing: restoring into the new database while the server runs.
	StepPreparing = "preparing"
	// StepReady: the new database is ready; waiting for the confirmation.
	StepReady = "ready"
	// StepEntering: confirmed; the processes are letting go of the
	// database. The names may already be swapped if the service died in
	// the middle (Recover reads pg_database).
	StepEntering = "entering"
	// StepSwitched: the restored database has the server's name.
	StepSwitched = "switched"
	// StepResuming: the processes are coming back on the restored database.
	StepResuming = "resuming"
	// StepReverting: putting the previous database back after a failure.
	StepReverting = "reverting"
	// StepAborting: dropping the new database before switching (aborted
	// or expired).
	StepAborting = "aborting"

	// Final steps.

	// StepDone: the restore is finished.
	StepDone = "done"
	// StepFailed: preparing failed; the new database was dropped.
	StepFailed = "failed"
	// StepExpired: nobody confirmed within the ready window.
	StepExpired = "expired"
	// StepAborted: ended without swapping the names (cancelled, or the
	// processes did not let go in time).
	StepAborted = "aborted"
	// StepReverted: the names were swapped, then swapped back.
	StepReverted = "reverted"
	// StepUnknown: it could not tell whether the names were swapped. The
	// maintenance stays on and an operator has to decide.
	StepUnknown = "unknown"
)

// Final reports whether step ends the operation.
func Final(step string) bool {
	switch step {
	case StepDone, StepFailed, StepExpired, StepAborted, StepReverted, StepUnknown:
		return true
	}
	return false
}

// recordTTL is how long a finished record (and its journal) stays in Redis.
const recordTTL = 7 * 24 * time.Hour

// Record is the state of one operation.
type Record struct {
	OpID         string    `json:"opId"`
	Step         string    `json:"step"`
	GenerationID string    `json:"generationId"`
	Database     string    `json:"database"`
	Target       string    `json:"target"`
	Before       string    `json:"before"`
	Aside        string    `json:"aside,omitempty"`
	UserID       string    `json:"userId"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
	ReadyAt      time.Time `json:"readyAt,omitzero"`

	ticketHash string
	tokenHash  string
}

// JournalEntry is one line of an operation's journal.
type JournalEntry struct {
	At      time.Time `json:"at"`
	Step    string    `json:"step"`
	Message string    `json:"message"`
}

// Store keeps the records in Redis.
//
// キーは <prefix>elythia:maintenance:restore で始める。戻した後の Redis の後始末
// (backup.CleanRedisAfterRestore) は elythia:maintenance を消さないので、途中で
// 自分の記録を消さない。
type Store struct {
	rdb    *redis.Client
	prefix string
}

// NewStore returns a Store keeping its keys under prefix (the redis.prefix
// of the config plus ':').
func NewStore(rdb *redis.Client, prefix string) *Store {
	return &Store{rdb: rdb, prefix: prefix}
}

func (s *Store) key(opID string) string     { return s.prefix + "elythia:maintenance:restore:" + opID }
func (s *Store) journal(opID string) string { return s.key(opID) + ":journal" }

// CurrentKey holds the opId of the operation that is not finished.
func (s *Store) CurrentKey() string { return s.prefix + "elythia:maintenance:restore-current" }

// ErrBusy is returned when another operation is not finished.
var ErrBusy = errors.New("restoreop: another restore is in progress")

// ErrNotFound is returned for an unknown opId.
var ErrNotFound = errors.New("restoreop: no such operation")

// ErrStep is returned when the operation is not at the step a request
// needs (confirming twice, aborting after the confirmation, ...).
var ErrStep = errors.New("restoreop: the operation is not at the right step")

var createScript = redis.NewScript(`
if redis.call('SET', KEYS[1], ARGV[1], 'NX') == false then return 0 end
redis.call('HSET', KEYS[2], unpack(ARGV, 2))
return 1`)

// create stores a new record and makes it the current one.
func (s *Store) create(ctx context.Context, r *Record) error {
	args := []any{r.OpID}
	args = append(args, s.fields(r)...)
	n, err := createScript.Run(ctx, s.rdb, []string{s.CurrentKey(), s.key(r.OpID)}, args...).Int()
	if err != nil {
		return fmt.Errorf("restoreop: write record: %w", err)
	}
	if n == 0 {
		return ErrBusy
	}
	return nil
}

func (s *Store) fields(r *Record) []any {
	return []any{
		"opId", r.OpID, "step", r.Step, "generationId", r.GenerationID, "database", r.Database,
		"target", r.Target, "before", r.Before, "aside", r.Aside, "userId", r.UserID, "error", r.Error,
		"ticketHash", r.ticketHash, "tokenHash", r.tokenHash,
		"createdAt", formatTime(r.CreatedAt), "updatedAt", formatTime(r.UpdatedAt), "readyAt", formatTime(r.ReadyAt),
	}
}

// Get returns the record of opID.
func (s *Store) Get(ctx context.Context, opID string) (*Record, error) {
	m, err := s.rdb.HGetAll(ctx, s.key(opID)).Result()
	if err != nil {
		return nil, fmt.Errorf("restoreop: read record: %w", err)
	}
	if len(m) == 0 {
		return nil, ErrNotFound
	}
	return &Record{
		OpID: m["opId"], Step: m["step"], GenerationID: m["generationId"], Database: m["database"],
		Target: m["target"], Before: m["before"], Aside: m["aside"], UserID: m["userId"], Error: m["error"],
		CreatedAt: parseTime(m["createdAt"]), UpdatedAt: parseTime(m["updatedAt"]), ReadyAt: parseTime(m["readyAt"]),
		ticketHash: m["ticketHash"], tokenHash: m["tokenHash"],
	}, nil
}

// Current returns the operation that is not finished, or nil.
func (s *Store) Current(ctx context.Context) (*Record, error) {
	opID, err := s.rdb.Get(ctx, s.CurrentKey()).Result()
	if errors.Is(err, redis.Nil) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("restoreop: read the current operation: %w", err)
	}
	r, err := s.Get(ctx, opID)
	if errors.Is(err, ErrNotFound) {
		// 記録が無いのに印だけが残っている (手で消した、など)。印は残し、呼び出し
		// 元に知らせる。勝手に消すと、進行中の操作を見失うかもしれない。
		return nil, fmt.Errorf("restoreop: %s names operation %s, which has no record", s.CurrentKey(), opID)
	}
	return r, err
}

// setStepScript moves the record from ARGV[1] (any when empty) to ARGV[2]
// and writes the other fields. 1: moved, 0: no record, -1: other step.
// A final step also clears the current marker (when it names this record)
// and puts a TTL on the record and its journal.
var setStepScript = redis.NewScript(`
if redis.call('EXISTS', KEYS[1]) == 0 then return 0 end
if ARGV[1] ~= '' and redis.call('HGET', KEYS[1], 'step') ~= ARGV[1] then return -1 end
redis.call('HSET', KEYS[1], 'step', ARGV[2], unpack(ARGV, 6))
if ARGV[3] == '1' then
  if redis.call('GET', KEYS[2]) == ARGV[4] then redis.call('DEL', KEYS[2]) end
  redis.call('EXPIRE', KEYS[1], tonumber(ARGV[5]))
  redis.call('EXPIRE', KEYS[3], tonumber(ARGV[5]))
end
return 1`)

// setStep moves opID from `from` (any when empty) to `to`, with extra
// fields. It returns ErrStep when the record is at another step.
func (s *Store) setStep(ctx context.Context, opID, from, to string, now time.Time, extra ...any) error {
	final := "0"
	if Final(to) {
		final = "1"
	}
	args := []any{from, to, final, opID, int64(recordTTL / time.Second), "updatedAt", formatTime(now)}
	args = append(args, extra...)
	n, err := setStepScript.Run(ctx, s.rdb, []string{s.key(opID), s.CurrentKey(), s.journal(opID)}, args...).Int()
	if err != nil {
		return fmt.Errorf("restoreop: write record: %w", err)
	}
	switch n {
	case 0:
		return ErrNotFound
	case -1:
		return ErrStep
	}
	return nil
}

// appendJournal adds one entry to the journal of opID.
func (s *Store) appendJournal(ctx context.Context, opID string, e JournalEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return s.rdb.RPush(ctx, s.journal(opID), b).Err()
}

// Journal returns the journal of opID.
func (s *Store) Journal(ctx context.Context, opID string) ([]JournalEntry, error) {
	raw, err := s.rdb.LRange(ctx, s.journal(opID), 0, -1).Result()
	if err != nil {
		return nil, fmt.Errorf("restoreop: read journal: %w", err)
	}
	out := make([]JournalEntry, 0, len(raw))
	for _, line := range raw {
		var e JournalEntry
		if json.Unmarshal([]byte(line), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

// newTicket returns a ticket and its hash.
func newTicket() (ticket, hash string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	ticket = base64.RawURLEncoding.EncodeToString(b)
	return ticket, hashString(ticket)
}

func newOpID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return "restore-" + hex.EncodeToString(b)
}

// hashString is the hex sha256 of v. ticket と session token は hash だけを置く。
func hashString(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
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
