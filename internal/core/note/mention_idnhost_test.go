package note_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corenote "github.com/elythia-network/elythia/internal/core/note"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
)

// **メンションの host が大文字混じりでも解決されること** (#2704)。
//
// メンションの host は書き手が打ったままなので、`@user@Remote.Example` のように
// 大文字混じりで来る。生の完全一致だと解決されず `note.mentions` に載らないので、
// **通知も飛ばない**。ユーザーページが開けないのと違ってエラーが出ないため
// 気付きにくい。
//
// **Unicode 形はここには来ない。** `mentionRegex` の host 部が
// `[A-Za-z0-9.\-]+` で ASCII 限定なので、`@user@パイ.example` は host が落ちて
// ローカル `@user` として parse される (upstream の mfm-js も同じ)。効くのは
// 大文字混じり ASCII と大文字 punycode。
func TestResolveMentionUserIDs_IDNHost(t *testing.T) {
	const puny = "xn--eckve.example"
	repo := testutil.NewMockUserRepository()
	host := puny
	uri := "https://" + puny + "/users/idn"
	repo.Users["idn1"] = &model.User{
		ID: "idn1", Username: "IdnUser", UsernameLower: "idnuser", Host: &host, URI: &uri,
	}
	svc := corenote.NewCreateService(nil, nil, nil, nil)
	require.NotNil(t, svc)
	svc.SetUserRepo(repo)

	for _, tc := range []struct {
		name string
		host string
	}{
		{"punycode", puny},
		{"punycode uppercase", "XN--ECKVE.EXAMPLE"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := svc.ResolveMentionUserIDsForTest([]corenote.Mention{
				{Username: "idnuser", Host: tc.host},
			}, nil)
			assert.Equal(t, []string{"idn1"}, got, "host=%q のメンションが解決されること", tc.host)
		})
	}

	t.Run("別の host は解決しない", func(t *testing.T) {
		got := svc.ResolveMentionUserIDsForTest([]corenote.Mention{
			{Username: "idnuser", Host: "other.example"},
		}, nil)
		assert.Empty(t, got)
	})
}
