package activitypub

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

// ローカルの人の actor に、ID の日時 (登録日) を `published` として載せる (#3465)。
func TestRenderPerson_PublishedIsAccountCreationTime(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	created := time.Date(2019, 7, 8, 9, 10, 11, 123000000, time.UTC)
	r := newRenderer()
	r.SetIDGenerator(gen)

	m := marshalMap(t, r.RenderPerson(&model.User{ID: gen.Generate(created), Username: "alice"}, nil, "PUBKEY", nil))
	assert.Equal(t, "2019-07-08T09:10:11.123Z", m["published"])

	// 読めない ID では現在時刻で埋めず、出さない。
	m = marshalMap(t, r.RenderPerson(&model.User{ID: "!", Username: "bob"}, nil, "PUBKEY", nil))
	_, ok := m["published"]
	assert.False(t, ok, "読めない ID で published を出している")
}

func TestRenderPerson_NoIDGeneratorOmitsPublished(t *testing.T) {
	m := marshalMap(t, newRenderer().RenderPerson(&model.User{ID: "u1", Username: "alice"}, nil, "PUBKEY", nil))
	_, ok := m["published"]
	assert.False(t, ok)
}

// リモートの actor の `published` は、読める形なら文字列として取り出し、
// 読めない形でも actor 自体は読める (#3465)。
func TestPerson_UnmarshalPublished(t *testing.T) {
	cases := map[string]string{
		`"2017-04-08T00:00:00Z"`:            "2017-04-08T00:00:00Z",
		`["2017-04-08T00:00:00Z"]`:          "2017-04-08T00:00:00Z",
		`{"@value":"2017-04-08T00:00:00Z"}`: "2017-04-08T00:00:00Z",
		`1491609600000`:                     "2017-04-08T00:00:00Z",
		`true`:                              "",
		`null`:                              "",
	}
	for literal, want := range cases {
		t.Run(literal, func(t *testing.T) {
			var p Person
			require.NoError(t, json.Unmarshal([]byte(`{"id":"https://remote.example/users/a","type":"Person","preferredUsername":"a","published":`+literal+`}`), &p))
			assert.Equal(t, want, p.Published.String())
			assert.Equal(t, "a", p.PreferredUsername)
		})
	}
}
