package entity

import (
	"testing"

	"github.com/shiroha-a/mk/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingInstances counts FindManyByHosts calls.
type countingInstances struct{ calls int }

func (c *countingInstances) FindManyByHosts(hosts []string) ([]*model.Instance, error) {
	c.calls++
	out := make([]*model.Instance, 0, len(hosts))
	for _, h := range hosts {
		name := "inst-" + h
		out = append(out, &model.Instance{Host: h, Name: &name})
	}
	return out, nil
}

// countingEmojis counts FindManyByNamesAndHost calls.
type countingEmojis struct{ calls int }

func (c *countingEmojis) FindManyByNamesAndHost(names []string, host *string) ([]*model.Emoji, error) {
	c.calls++
	out := make([]*model.Emoji, 0, len(names))
	for _, n := range names {
		out = append(out, &model.Emoji{Name: n, Host: host, PublicURL: "https://" + *host + "/" + n})
	}
	return out, nil
}

// 一覧の instance と絵文字は利用者の数に比例せず、instance 1 回・絵文字は
// ホストごとに 1 回で引く (#3330)。ローカルの利用者は解決しない (本家と同じ)。
func TestFillUserLites(t *testing.T) {
	a, b := "a.example", "b.example"
	users := []*model.User{
		{ID: "1", Username: "x", Host: &a, Emojis: model.StringArray{"e1"}},
		{ID: "2", Username: "y", Host: &a, Emojis: model.StringArray{"e2"}},
		{ID: "3", Username: "z", Host: &b, Emojis: model.StringArray{"e3"}},
		{ID: "4", Username: "local", Emojis: model.StringArray{"e4"}},
	}
	lites := make([]UserLite, len(users))
	ptrs := make([]*UserLite, 0, len(users)+1)
	for i, u := range users {
		lites[i] = PackUserLite(u)
		ptrs = append(ptrs, &lites[i])
	}
	// nil は飛ばす
	withNil := append(append([]*model.User{}, users...), nil)
	ptrs = append(ptrs, nil)
	inst, emo := &countingInstances{}, &countingEmojis{}

	FillUserLites(inst, emo, withNil, ptrs)

	assert.Equal(t, 1, inst.calls)
	assert.Equal(t, 2, emo.calls, "ホストごとに 1 回")
	require.NotNil(t, lites[0].Instance)
	assert.Equal(t, "inst-a.example", *lites[0].Instance.Name)
	assert.Equal(t, "inst-b.example", *lites[2].Instance.Name)
	assert.Equal(t, map[string]string{"e2": "https://a.example/e2"}, lites[1].Emojis)
	assert.Nil(t, lites[3].Instance)
	assert.Empty(t, lites[3].Emojis, "ローカルの利用者の絵文字は解決しない")
}

func TestFillUserLites_NoTargetsOrLookups(t *testing.T) {
	FillUserLites(nil, nil, nil, nil)
	FillUserLites(nil, nil, []*model.User{nil}, []*UserLite{nil})
	h := "a.example"
	u := &model.User{ID: "1", Host: &h, Emojis: model.StringArray{"e1"}}
	lite := PackUserLite(u)
	FillUserLites(nil, nil, []*model.User{u}, []*UserLite{&lite})
	assert.Nil(t, lite.Instance)
	// 長さが食い違う呼び出しは何もしない。
	inst := &countingInstances{}
	FillUserLites(inst, nil, []*model.User{u}, nil)
	assert.Zero(t, inst.calls)
}
