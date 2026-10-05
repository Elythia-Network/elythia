package repository

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/model"
)

func TestFederationRuleRepository_CRUD(t *testing.T) {
	require.NoError(t, testDB.Exec(`DELETE FROM federation_rule`).Error)
	t.Cleanup(func() { testDB.Exec(`DELETE FROM federation_rule`) })
	repo := NewFederationRuleRepository(testDB)
	now := time.Now().UTC().Truncate(time.Millisecond)
	bot := true
	hours := 24
	cw := "spam?"

	b := &model.FederationRule{ID: "fr_b", CreatedAt: now, UpdatedAt: now, Name: "b", Mode: model.FederationRuleModeEnforce,
		Target: model.FederationRuleTargetNote, Position: 1, Hosts: model.StringArray{"spam.example"},
		IsBot: &bot, NewWithinHours: &hours, Patterns: model.StringArray{"buy now"}, Tags: model.StringArray{"ad"},
		StripMedia: true, Sensitive: true, Unlist: true, CW: &cw}
	a := &model.FederationRule{ID: "fr_a", CreatedAt: now, UpdatedAt: now, Name: "a", Mode: model.FederationRuleModeRecord,
		Target: model.FederationRuleTargetActivity, Position: 1, ActivityTypes: model.StringArray{"Follow"}, Reject: true,
		Hosts: model.StringArray{}, Patterns: model.StringArray{}, Tags: model.StringArray{}}
	first := &model.FederationRule{ID: "fr_z", CreatedAt: now, UpdatedAt: now, Mode: model.FederationRuleModeDisabled,
		Target: model.FederationRuleTargetNote, Position: 0, Hosts: model.StringArray{"x.example"}, Reject: true}
	for _, r := range []*model.FederationRule{b, a, first} {
		require.NoError(t, repo.Create(r))
	}

	list, err := repo.List()
	require.NoError(t, err)
	require.Len(t, list, 3)
	assert.Equal(t, []string{"fr_z", "fr_a", "fr_b"}, []string{list[0].ID, list[1].ID, list[2].ID}, "position, then id")
	got := list[2]
	assert.Equal(t, b.Hosts, got.Hosts)
	assert.Equal(t, &bot, got.IsBot)
	assert.Equal(t, &hours, got.NewWithinHours)
	assert.Equal(t, &cw, got.CW)
	assert.True(t, got.StripMedia && got.Sensitive && got.Unlist)
	assert.Nil(t, list[1].IsBot)

	n, err := repo.Count()
	require.NoError(t, err)
	assert.EqualValues(t, 3, n)

	// 更新は false / nil / 空配列も書く (Updates の zero 値の罠)。
	b.StripMedia, b.Sensitive, b.Unlist, b.IsBot, b.CW, b.Tags = false, false, false, nil, nil, model.StringArray{}
	b.Name = "b2"
	b.CreatedAt = now.Add(time.Hour) // 呼び出し側が何を渡しても作成日時は変えない
	require.NoError(t, repo.Update(b))
	got, err = repo.FindByID("fr_b")
	require.NoError(t, err)
	assert.Equal(t, "b2", got.Name)
	assert.False(t, got.StripMedia || got.Sensitive || got.Unlist)
	assert.Nil(t, got.IsBot)
	assert.Nil(t, got.CW)
	assert.Empty(t, got.Tags)
	assert.Equal(t, now, got.CreatedAt.UTC(), "createdAt is not overwritten")

	require.NoError(t, repo.Delete("fr_b"))
	_, err = repo.FindByID("fr_b")
	assert.True(t, IsNotFound(err))
	assert.True(t, IsNotFound(repo.Delete("fr_b")))
	assert.True(t, IsNotFound(repo.Update(b)), "updating a deleted rule does not resurrect it")
	_, err = repo.FindByID("fr_b")
	assert.True(t, IsNotFound(err))

	_, err = repo.FindByID("bad\x00id")
	assert.True(t, IsNotFound(err))
	assert.True(t, IsNotFound(repo.Delete("bad\x00id")))
}
