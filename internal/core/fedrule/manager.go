package fedrule

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ErrNotFound means the rule does not exist.
var ErrNotFound = errors.New("fedrule: no such rule")

// ErrTooManyRules means MaxRules would be exceeded.
var ErrTooManyRules = errors.New("fedrule: too many rules")

// Repository is the storage the Manager edits.
type Repository interface {
	List() ([]*model.FederationRule, error)
	FindByID(id string) (*model.FederationRule, error)
	Count() (int64, error)
	Create(rule *model.FederationRule) error
	Update(rule *model.FederationRule) error
	Delete(id string) error
}

// HitReader reads and resets the recorded hits.
type HitReader interface {
	Counts(ctx context.Context) (map[string]int64, error)
	Samples(ctx context.Context, ruleID string, limit int) ([]Hit, error)
	Forget(ctx context.Context, ruleID string) error
}

// Manager creates, edits and deletes rules for the admin API and keeps every
// process's evaluator in step.
type Manager struct {
	repo  Repository
	svc   *Service
	hits  HitReader
	idGen id.Generator
	clock func() time.Time
	// notify は他のプロセスへ変更を知らせる (pubsub)。自プロセスは svc.Reload で
	// 直接読み直す。
	notify func()
	mu     sync.Mutex
}

// NewManager constructs a Manager. svc / hits / notify may be nil.
func NewManager(repo Repository, svc *Service, hits HitReader, idGen id.Generator, notify func()) *Manager {
	return &Manager{repo: repo, svc: svc, hits: hits, idGen: idGen, clock: time.Now, notify: notify}
}

// List returns the rules in evaluation order.
func (m *Manager) List() ([]*model.FederationRule, error) { return m.repo.List() }

// Counts returns each rule's hits in the last HitWindow.
func (m *Manager) Counts(ctx context.Context) (map[string]int64, error) {
	if m.hits == nil {
		return map[string]int64{}, nil
	}
	return m.hits.Counts(ctx)
}

// Samples returns a rule's recent hits.
func (m *Manager) Samples(ctx context.Context, ruleID string, limit int) ([]Hit, error) {
	if _, err := m.find(ruleID); err != nil {
		return nil, err
	}
	if m.hits == nil {
		return []Hit{}, nil
	}
	return m.hits.Samples(ctx, ruleID, limit)
}

func (m *Manager) find(ruleID string) (*model.FederationRule, error) {
	r, err := m.repo.FindByID(ruleID)
	if repository.IsNotFound(err) {
		return nil, ErrNotFound
	}
	return r, err
}

// Create validates and stores a new rule.
func (m *Manager) Create(rule *model.FederationRule) (*model.FederationRule, error) {
	if err := Normalize(rule); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, err := m.repo.Count()
	if err != nil {
		return nil, err
	}
	if n >= MaxRules {
		return nil, ErrTooManyRules
	}
	now := m.clock()
	rule.ID = m.idGen.Generate(now)
	rule.CreatedAt, rule.UpdatedAt = now, now
	if err := m.repo.Create(rule); err != nil {
		return nil, err
	}
	m.changed()
	return rule, nil
}

// Update validates and overwrites an existing rule. 戻り値の 1 つ目は変更前。
func (m *Manager) Update(rule *model.FederationRule) (before, after *model.FederationRule, err error) {
	if err := Normalize(rule); err != nil {
		return nil, nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	before, err = m.find(rule.ID)
	if err != nil {
		return nil, nil, err
	}
	rule.CreatedAt = before.CreatedAt
	rule.UpdatedAt = m.clock()
	if err := m.repo.Update(rule); err != nil {
		if repository.IsNotFound(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	// **条件を変えたら当たった記録を捨てる。** 前の条件で数えた件数は新しい
	// 条件の当たり具合を表さないので、record で様子を見る運用が成り立たない。
	// mode / 動作 / 名前だけの変更では残す (record → enforce に切り替えた直後に
	// 根拠が消えると困る)。
	if !sameConditions(before, rule) {
		m.forget(rule.ID)
	}
	m.changed()
	return before, rule, nil
}

// Delete removes a rule and returns it.
func (m *Manager) Delete(ruleID string) (*model.FederationRule, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	before, err := m.find(ruleID)
	if err != nil {
		return nil, err
	}
	if err := m.repo.Delete(ruleID); err != nil {
		if repository.IsNotFound(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	m.forget(ruleID)
	m.changed()
	return before, nil
}

func (m *Manager) forget(ruleID string) {
	if m.hits == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.hits.Forget(ctx, ruleID); err != nil {
		slog.Warn("fedrule: cannot reset rule hits", "ruleId", ruleID, "err", err)
	}
}

// changed makes this process and the others read the rules again.
func (m *Manager) changed() {
	if m.svc != nil {
		if err := m.svc.Reload(); err != nil {
			// 読めなければ次の評価で読み直す (Invalidate)。変更そのものは保存済み。
			slog.Warn("fedrule: cannot reload rules after a change", "err", err)
			m.svc.Invalidate()
		}
	}
	if m.notify != nil {
		m.notify()
	}
}

func sameConditions(a, b *model.FederationRule) bool {
	return a.Target == b.Target &&
		slices.Equal(a.Hosts, b.Hosts) &&
		slices.Equal(a.ActivityTypes, b.ActivityTypes) &&
		slices.Equal(a.Patterns, b.Patterns) &&
		slices.Equal(a.Tags, b.Tags) &&
		equalPtr(a.IsBot, b.IsBot) &&
		equalPtr(a.NewWithinHours, b.NewWithinHours) &&
		equalPtr(a.HasAttachment, b.HasAttachment)
}

func equalPtr[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
