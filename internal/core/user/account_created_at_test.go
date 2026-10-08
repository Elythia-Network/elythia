package user

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/misc/id"
	"github.com/elythia-network/elythia/internal/model"
)

func TestAccountCreatedAt(t *testing.T) {
	gen, err := id.NewGenerator("aidx")
	require.NoError(t, err)
	firstSeen := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	created := time.Date(2018, 3, 4, 0, 0, 0, 0, time.UTC)
	userID := gen.Generate(firstSeen)
	host := "remote.example"

	cases := []struct {
		name   string
		user   *model.User
		gen    id.Generator
		want   time.Time
		wantOK bool
	}{
		{"remote with stored creation time", &model.User{ID: userID, Host: &host, AccountCreatedAt: &created}, gen, created, true},
		{"remote with stored creation time and no generator", &model.User{ID: userID, Host: &host, AccountCreatedAt: &created}, nil, created, true},
		{"remote without it falls back to first seen", &model.User{ID: userID, Host: &host}, gen, firstSeen, true},
		{"local uses the ID time", &model.User{ID: userID}, gen, firstSeen, true},
		{"local ignores a stray column value", &model.User{ID: userID, AccountCreatedAt: &created}, gen, firstSeen, true},
		{"nil user", nil, gen, time.Time{}, false},
		{"no generator", &model.User{ID: userID, Host: &host}, nil, time.Time{}, false},
		{"unparsable ID", &model.User{ID: "!", Host: &host}, gen, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := AccountCreatedAt(tc.user, tc.gen)
			assert.Equal(t, tc.wantOK, ok)
			assert.Truef(t, tc.want.Equal(got), "want %s, got %s", tc.want, got)
		})
	}
}
