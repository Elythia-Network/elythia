package fedrule

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

type memRepo struct {
	rules              map[string]*model.FederationRule
	order              []string
	failList, failSave error
	failCount          error
	updateNotFound     bool
}

func newMemRepo() *memRepo { return &memRepo{rules: map[string]*model.FederationRule{}} }

func (r *memRepo) List() ([]*model.FederationRule, error) {
	if r.failList != nil {
		return nil, r.failList
	}
	var out []*model.FederationRule
	for _, id := range r.order {
		if x, ok := r.rules[id]; ok {
			c := *x
			out = append(out, &c)
		}
	}
	return out, nil
}

func (r *memRepo) FindByID(id string) (*model.FederationRule, error) {
	x, ok := r.rules[id]
	if !ok {
		return nil, repository.ErrNotFound
	}
	c := *x
	return &c, nil
}

func (r *memRepo) Count() (int64, error) { return int64(len(r.rules)), r.failCount }

func (r *memRepo) Create(rule *model.FederationRule) error {
	if r.failSave != nil {
		return r.failSave
	}
	c := *rule
	r.rules[rule.ID] = &c
	r.order = append(r.order, rule.ID)
	return nil
}

func (r *memRepo) Update(rule *model.FederationRule) error {
	if r.failSave != nil {
		return r.failSave
	}
	if _, ok := r.rules[rule.ID]; !ok || r.updateNotFound {
		return repository.ErrNotFound
	}
	c := *rule
	r.rules[rule.ID] = &c
	return nil
}

func (r *memRepo) Delete(id string) error {
	if r.failSave != nil {
		return r.failSave
	}
	if _, ok := r.rules[id]; !ok {
		return repository.ErrNotFound
	}
	delete(r.rules, id)
	return nil
}

type memHits struct {
	forgotten []string
	forgetErr error
}

func (h *memHits) Counts(context.Context) (map[string]int64, error) {
	return map[string]int64{"x": 1}, nil
}
func (h *memHits) Samples(context.Context, string, int) ([]Hit, error) {
	return []Hit{{RuleID: "x"}}, nil
}
func (h *memHits) Forget(_ context.Context, id string) error {
	h.forgotten = append(h.forgotten, id)
	return h.forgetErr
}

func newTestManager(t *testing.T) (*Manager, *memRepo, *memHits, *Service, *int) {
	t.Helper()
	repo := newMemRepo()
	hits := &memHits{}
	svc := NewService(repo, nil, nil)
	idGen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	notified := 0
	m := NewManager(repo, svc, hits, idGen, func() { notified++ })
	return m, repo, hits, svc, &notified
}

func TestManager_Lifecycle(t *testing.T) {
	m, repo, hits, svc, notified := newTestManager(t)
	ctx := context.Background()

	_, err := m.Create(&model.FederationRule{Mode: "bad"})
	assert.True(t, IsValidationError(err))

	created, err := m.Create(noteRule())
	require.NoError(t, err)
	assert.NotEmpty(t, created.ID)
	assert.False(t, created.CreatedAt.IsZero())
	assert.Equal(t, 1, *notified)
	assert.True(t, svc.HasNoteRules(), "this process reloads at once")

	list, err := m.List()
	require.NoError(t, err)
	require.Len(t, list, 1)

	// 名前と mode だけの変更では記録を残す。
	upd := *created
	upd.Name = "renamed"
	upd.Mode = model.FederationRuleModeEnforce
	upd.CreatedAt = time.Time{}
	before, after, err := m.Update(&upd)
	require.NoError(t, err)
	assert.Equal(t, "", before.Name)
	assert.Equal(t, "renamed", after.Name)
	assert.Equal(t, created.CreatedAt, after.CreatedAt, "createdAt is kept")
	assert.Empty(t, hits.forgotten)
	assert.Equal(t, 2, *notified)

	// 条件を変えたら記録を捨てる。
	upd2 := *after
	upd2.Hosts = model.StringArray{"other.example"}
	_, _, err = m.Update(&upd2)
	require.NoError(t, err)
	assert.Equal(t, []string{created.ID}, hits.forgotten)

	counts, err := m.Counts(ctx)
	require.NoError(t, err)
	assert.EqualValues(t, 1, counts["x"])
	samples, err := m.Samples(ctx, created.ID, 10)
	require.NoError(t, err)
	assert.Len(t, samples, 1)
	_, err = m.Samples(ctx, "missing", 10)
	assert.ErrorIs(t, err, ErrNotFound)

	_, _, err = m.Update(&model.FederationRule{ID: "missing", Mode: "record", Target: "note", Hosts: model.StringArray{"a"}, Reject: true})
	assert.ErrorIs(t, err, ErrNotFound)
	_, _, err = m.Update(&model.FederationRule{ID: created.ID, Mode: "bad"})
	assert.True(t, IsValidationError(err))

	deleted, err := m.Delete(created.ID)
	require.NoError(t, err)
	assert.Equal(t, created.ID, deleted.ID)
	assert.Equal(t, []string{created.ID, created.ID}, hits.forgotten)
	assert.False(t, svc.HasNoteRules())
	_, err = m.Delete(created.ID)
	assert.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, repo.rules)
}

func TestManager_Limits(t *testing.T) {
	m, repo, _, _, _ := newTestManager(t)
	for i := 0; i < MaxRules; i++ {
		repo.rules[string(rune('a'+i%26))+time.Duration(i).String()] = &model.FederationRule{}
	}
	_, err := m.Create(noteRule())
	assert.ErrorIs(t, err, ErrTooManyRules)
}

func TestManager_Failures(t *testing.T) {
	m, repo, hits, svc, _ := newTestManager(t)
	boom := errors.New("db")

	repo.failCount = boom
	_, err := m.Create(noteRule())
	assert.ErrorIs(t, err, boom)
	repo.failCount = nil

	repo.failSave = boom
	_, err = m.Create(noteRule())
	assert.ErrorIs(t, err, boom)
	repo.failSave = nil

	created, err := m.Create(noteRule())
	require.NoError(t, err)

	repo.failSave = boom
	_, _, err = m.Update(created)
	assert.ErrorIs(t, err, boom)
	_, err = m.Delete(created.ID)
	assert.ErrorIs(t, err, boom)
	repo.failSave = nil

	// 見つけた直後に消された (並行した削除)。
	repo.updateNotFound = true
	_, _, err = m.Update(created)
	assert.ErrorIs(t, err, ErrNotFound)
	repo.updateNotFound = false

	// 記録の削除や読み直しに失敗しても、変更そのものは成功させる。
	hits.forgetErr = boom
	repo.failList = boom
	_, err = m.Delete(created.ID)
	require.NoError(t, err)
	assert.True(t, svc.stale.Load(), "a failed reload is retried on the next evaluation")

	// hits / svc / notify が無くても動く。
	bare := NewManager(newMemRepo(), nil, nil, m.idGen, nil)
	r, err := bare.Create(noteRule())
	require.NoError(t, err)
	counts, err := bare.Counts(context.Background())
	require.NoError(t, err)
	assert.Empty(t, counts)
	samples, err := bare.Samples(context.Background(), r.ID, 1)
	require.NoError(t, err)
	assert.Empty(t, samples)
	_, err = bare.Delete(r.ID)
	require.NoError(t, err)
}

func TestSameConditions(t *testing.T) {
	a := &model.FederationRule{Target: "note", Hosts: model.StringArray{"a"}, IsBot: ptr(true)}
	b := *a
	assert.True(t, sameConditions(a, &b))
	b.Mode, b.Name, b.Reject, b.CW = "enforce", "x", true, ptr("c")
	assert.True(t, sameConditions(a, &b), "mode, name and actions are not conditions")
	for name, mut := range map[string]func(r *model.FederationRule){
		"target":        func(r *model.FederationRule) { r.Target = "activity" },
		"hosts":         func(r *model.FederationRule) { r.Hosts = nil },
		"types":         func(r *model.FederationRule) { r.ActivityTypes = model.StringArray{"Follow"} },
		"patterns":      func(r *model.FederationRule) { r.Patterns = model.StringArray{"x"} },
		"tags":          func(r *model.FederationRule) { r.Tags = model.StringArray{"x"} },
		"isBot":         func(r *model.FederationRule) { r.IsBot = ptr(false) },
		"isBot nil":     func(r *model.FederationRule) { r.IsBot = nil },
		"new":           func(r *model.FederationRule) { r.NewWithinHours = ptr(1) },
		"hasAttachment": func(r *model.FederationRule) { r.HasAttachment = ptr(true) },
	} {
		c := *a
		mut(&c)
		assert.False(t, sameConditions(a, &c), name)
	}
}
