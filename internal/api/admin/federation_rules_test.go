package admin_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/core/fedrule"
	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
)

// ruleMemRepo is an in-memory fedrule.Repository.
type ruleMemRepo struct {
	rules []*model.FederationRule
	err   error
}

func (r *ruleMemRepo) List() ([]*model.FederationRule, error) { return r.rules, r.err }
func (r *ruleMemRepo) FindByID(id string) (*model.FederationRule, error) {
	for _, x := range r.rules {
		if x.ID == id {
			c := *x
			return &c, nil
		}
	}
	return nil, repository.ErrNotFound
}
func (r *ruleMemRepo) Count() (int64, error) { return int64(len(r.rules)), r.err }
func (r *ruleMemRepo) Create(rule *model.FederationRule) error {
	c := *rule
	r.rules = append(r.rules, &c)
	return nil
}
func (r *ruleMemRepo) Update(rule *model.FederationRule) error {
	for i, x := range r.rules {
		if x.ID == rule.ID {
			c := *rule
			r.rules[i] = &c
			return nil
		}
	}
	return repository.ErrNotFound
}
func (r *ruleMemRepo) Delete(id string) error {
	for i, x := range r.rules {
		if x.ID == id {
			r.rules = append(r.rules[:i], r.rules[i+1:]...)
			return nil
		}
	}
	return repository.ErrNotFound
}

type ruleHitsStub struct {
	counts    map[string]int64
	countsErr error
}

func (s *ruleHitsStub) Counts(context.Context) (map[string]int64, error) {
	return s.counts, s.countsErr
}
func (s *ruleHitsStub) Samples(_ context.Context, ruleID string, _ int) ([]fedrule.Hit, error) {
	return []fedrule.Hit{{RuleID: ruleID, Host: "spam.example", Subject: "https://spam.example/n/1", Kind: "Note",
		At: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}}, nil
}
func (s *ruleHitsStub) Forget(context.Context, string) error { return nil }

func newRuleManager(t *testing.T) (*fedrule.Manager, *ruleMemRepo, *ruleHitsStub) {
	t.Helper()
	repo := &ruleMemRepo{}
	hits := &ruleHitsStub{counts: map[string]int64{}}
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	return fedrule.NewManager(repo, nil, hits, gen, nil), repo, hits
}

func TestFederationRules_CreateListUpdateDelete(t *testing.T) {
	h, _, _, _ := newTestHandler(t)
	m, repo, hits := newRuleManager(t)
	h.SetFederationRuleManager(m)
	logs := attachModLog(t, h)

	rec := doPost(h.FederationRuleCreate, `{"name":"ads","mode":"record","target":"note",
		"hosts":["Spam.Example"],"patterns":["buy now"],"isBot":null,"cw":"広告","unlist":true}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	ruleID := created["id"].(string)
	assert.Equal(t, []any{"spam.example"}, created["hosts"], "normalized")
	assert.Equal(t, []any{}, created["tags"], "empty lists are [] not null")
	assert.Nil(t, created["isBot"])

	hits.counts[ruleID] = 7
	rec = doPost(h.FederationRules, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	var list []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list, 1)
	assert.EqualValues(t, 7, list[0]["hits"])
	assert.Equal(t, "広告", list[0]["cw"])

	rec = doPost(h.FederationRuleHits, `{"ruleId":"`+ruleID+`"}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `[{"ruleId":"`+ruleID+`","host":"spam.example","subject":"https://spam.example/n/1","kind":"Note","applied":false,"at":"2026-09-29T00:00:00Z"}]`, rec.Body.String())

	rec = doPost(h.FederationRuleUpdate, `{"ruleId":"`+ruleID+`","name":"ads2","mode":"enforce","target":"note","hosts":["spam.example"],"reject":true}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, model.FederationRuleModeEnforce, repo.rules[0].Mode)
	assert.Nil(t, repo.rules[0].CW, "the whole rule is replaced")

	rec = doPost(h.FederationRuleDelete, `{"ruleId":"`+ruleID+`"}`, adminUser)
	require.Equal(t, http.StatusNoContent, rec.Code)
	assert.Empty(t, repo.rules)

	require.Eventually(t, func() bool { return len(logs.Snapshot()) == 3 }, time.Second, 5*time.Millisecond)
	types := map[string]map[string]any{}
	for _, e := range logs.Snapshot() {
		var info map[string]any
		require.NoError(t, json.Unmarshal(e.Info, &info))
		types[e.Type] = info
	}
	require.Contains(t, types, "createFederationRule")
	assert.Equal(t, ruleID, types["createFederationRule"]["ruleId"])
	assert.Equal(t, "ads", types["createFederationRule"]["ruleName"])
	assert.Contains(t, types["createFederationRule"], "rule")
	require.Contains(t, types, "updateFederationRule")
	assert.Equal(t, "ads", types["updateFederationRule"]["before"].(map[string]any)["name"])
	assert.Equal(t, "ads2", types["updateFederationRule"]["after"].(map[string]any)["name"])
	require.Contains(t, types, "deleteFederationRule")
	assert.Equal(t, "ads2", types["deleteFederationRule"]["ruleName"])
}

func TestFederationRules_Errors(t *testing.T) {
	h, _, _, _ := newTestHandler(t)

	// 未配線: 一覧は空配列、それ以外は存在しない扱い / 500。
	rec := doPost(h.FederationRules, `{}`, adminUser)
	assert.JSONEq(t, `[]`, rec.Body.String())
	assert.Contains(t, doPost(h.FederationRuleHits, `{"ruleId":"x"}`, adminUser).Body.String(), "NO_SUCH_RULE")
	assert.Contains(t, doPost(h.FederationRuleUpdate, `{"ruleId":"x"}`, adminUser).Body.String(), "NO_SUCH_RULE")
	assert.Contains(t, doPost(h.FederationRuleDelete, `{"ruleId":"x"}`, adminUser).Body.String(), "NO_SUCH_RULE")
	assert.Equal(t, http.StatusInternalServerError, doPost(h.FederationRuleCreate, `{}`, adminUser).Code)

	m, repo, hits := newRuleManager(t)
	h.SetFederationRuleManager(m)
	rec = doPost(h.FederationRuleCreate, `{"mode":"enforce","target":"note","reject":true}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "INVALID_PARAM")
	assert.Contains(t, rec.Body.String(), "at least one condition")
	rec = doPost(h.FederationRuleCreate, `{"hosts":"not-an-array"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	rec = doPost(h.FederationRuleUpdate, `{"ruleId":"x","hosts":"not-an-array"}`, adminUser)
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	for _, handler := range []func() int{
		func() int { return doPost(h.FederationRuleHits, `{}`, adminUser).Code },
		func() int { return doPost(h.FederationRuleUpdate, `{}`, adminUser).Code },
		func() int { return doPost(h.FederationRuleDelete, `{}`, adminUser).Code },
	} {
		assert.Equal(t, http.StatusBadRequest, handler(), "ruleId is required")
	}
	assert.Contains(t, doPost(h.FederationRuleHits, `{"ruleId":"missing"}`, adminUser).Body.String(), "NO_SUCH_RULE")
	assert.Contains(t, doPost(h.FederationRuleDelete, `{"ruleId":"missing"}`, adminUser).Body.String(), "NO_SUCH_RULE")
	assert.Contains(t, doPost(h.FederationRuleUpdate, `{"ruleId":"missing","mode":"record","target":"note","hosts":["a"],"reject":true}`, adminUser).Body.String(), "NO_SUCH_RULE")

	for i := 0; i < fedrule.MaxRules; i++ {
		repo.rules = append(repo.rules, &model.FederationRule{ID: string(rune(0x4e00 + i))})
	}
	rec = doPost(h.FederationRuleCreate, `{"mode":"record","target":"note","hosts":["a"],"reject":true}`, adminUser)
	assert.Contains(t, rec.Body.String(), "TOO_MANY_RULES")

	// 件数が読めなくても一覧は出す。ルールが読めなければ 500。
	repo.rules = repo.rules[:1]
	hits.countsErr = errors.New("redis down")
	rec = doPost(h.FederationRules, `{}`, adminUser)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"hits":0`)
	repo.err = errors.New("db down")
	assert.Equal(t, http.StatusInternalServerError, doPost(h.FederationRules, `{}`, adminUser).Code)
	assert.Equal(t, http.StatusInternalServerError, doPost(h.FederationRuleCreate, `{"mode":"record","target":"note","hosts":["a"],"reject":true}`, adminUser).Code)
}
