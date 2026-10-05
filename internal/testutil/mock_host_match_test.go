package testutil

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// **mock は本番の repository (hostMatch) と同じ意味論であること** (#2704)。
//
// ずれると、mock を使うテストだけが通って本番との差が隠れる。service 経由でも
// host は正規化されずに repository まで届く (`ShowByUsername` は自分で正規化しない)
// が、そちらは DB miss がリモート解決に化けるので**引けたかどうかを直接は見られない**。
// 引き当ての意味論はここで固定する。
func TestMockUserRepository_HostMatchesProduction(t *testing.T) {
	newRepo := func(stored string) *MockUserRepository {
		repo := NewMockUserRepository()
		h := stored
		repo.Users["u1"] = &model.User{ID: "u1", Username: "Alice", UsernameLower: "alice", Host: &h}
		return repo
	}

	cases := []struct {
		name   string
		stored string
		query  string
		want   bool
	}{
		{"完全一致", "remote.example", "remote.example", true},
		{"unicode で引く", "xn--eckve.example", "パイ.example", true},
		{"大文字 punycode で引く", "xn--eckve.example", "XN--ECKVE.EXAMPLE", true},
		{"大文字 ASCII で引く", "remote.example", "Remote.Example", true},
		{"別ホストは引かない", "remote.example", "other.example", false},
		// **非正規化のまま保存された行は引けない (#2996)。** 生の形にも当てる
		// 互換経路を撤去したので、保存された綴りそのもので問い合わせても当たらない。
		// ここが true に戻ると「mock だけ緩い」状態になり、本番で引けない行が
		// テストでだけ引ける。
		{"非正規化で保存された行は保存された形でも引けない", "Mixed.Example", "Mixed.Example", false},
		{"非正規化で保存された行は正規化形でも引けない", "Mixed.Example", "mixed.example", false},
		{"unicode で保存された行は punycode では引けない", "パイ.example", "xn--eckve.example", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newRepo(tc.stored)
			q := tc.query

			got, err := repo.FindByUsernameLower("alice", &q)
			if tc.want {
				require.NoError(t, err, "stored=%q query=%q", tc.stored, tc.query)
				assert.Equal(t, "u1", got.ID)
			} else {
				assert.Error(t, err, "stored=%q query=%q", tc.stored, tc.query)
			}

			many, err := repo.FindManyByUsernamesAndHost([]string{"Alice"}, &q)
			require.NoError(t, err)
			assert.Equal(t, tc.want, len(many) == 1, "一括引きも同じ結果になること")
		})
	}

	t.Run("host nil はローカル行だけ", func(t *testing.T) {
		repo := newRepo("remote.example")
		repo.Users["local"] = &model.User{ID: "local", Username: "Alice", UsernameLower: "alice"}
		got, err := repo.FindByUsernameLower("alice", nil)
		require.NoError(t, err)
		assert.Equal(t, "local", got.ID)
	})

	// **username も本番と同じく小文字化して突き合わせること。** 本番は
	// `"usernameLower" = lower(?)` なので `@Alice@remote.example` でも引けるが、
	// mock が byte 一致だと**そこだけ引けない**。mention の解決は
	// `ExtractMentionStructs` の結果をそのまま渡すので、書き手が大文字で打てば
	// 大文字のまま届く (`internal/core/federation` の 2 箇所)。
	t.Run("username の大文字小文字を問わない", func(t *testing.T) {
		repo := newRepo("remote.example")
		h := "remote.example"
		for _, name := range []string{"alice", "Alice", "ALICE"} {
			got, err := repo.FindByUsernameLower(name, &h)
			require.NoError(t, err, "username=%q", name)
			assert.Equal(t, "u1", got.ID)

			local := NewMockUserRepository()
			local.Users["l1"] = &model.User{ID: "l1", Username: "Alice", UsernameLower: "alice"}
			lgot, lerr := local.FindByUsernameLower(name, nil)
			require.NoError(t, lerr, "ローカルでも同じこと (username=%q)", name)
			assert.Equal(t, "l1", lgot.ID)
		}
	})
}

// **表記違いの 2 行があっても正規形の行に決まる (#2996)。** 本番と同じく、
// 問い合わせ側の綴りで結果が変わらないこと (変わると mention の解決先が書き手の
// 打ち方で揺れる)。
func TestMockUserRepository_ResolvesToNormalizedRowLikeProduction(t *testing.T) {
	newRepo := func() *MockUserRepository {
		repo := NewMockUserRepository()
		norm, raw := "mixed.example", "Mixed.Example"
		repo.Users["u_norm"] = &model.User{ID: "u_norm", UsernameLower: "dupuser", Host: &norm}
		repo.Users["u_raw"] = &model.User{ID: "u_raw", UsernameLower: "dupuser", Host: &raw}
		return repo
	}
	for _, q := range []string{"Mixed.Example", "mixed.example", "MIXED.EXAMPLE"} {
		t.Run(q, func(t *testing.T) {
			h := q
			got, err := newRepo().FindByUsernameLower("dupuser", &h)
			require.NoError(t, err)
			assert.Equal(t, "u_norm", got.ID, "正規形の行を返すこと")

			many, merr := newRepo().FindManyByUsernamesAndHost([]string{"dupuser"}, &h)
			require.NoError(t, merr)
			require.Len(t, many, 1, "正規形の行だけが返ること (#2996 で畳む処理は撤去した)")
			assert.Equal(t, "u_norm", many[0].ID)
		})
	}
}
