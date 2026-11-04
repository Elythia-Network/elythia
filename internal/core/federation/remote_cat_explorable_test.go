package federation_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/elythia-network/elythia/internal/model"
)

// actorDocWith adds raw JSON members to an actor document.
func actorDocWith(t *testing.T, uri, username, extra string) string {
	t.Helper()
	doc := actorDoc(uri, username)
	if extra == "" {
		return doc
	}
	out := strings.TrimSuffix(doc, "}") + "," + extra + "}"
	// 置換が空振りすると「その項目の無い actor」で検査が空虚に通るので落とす。
	require.Contains(t, out, extra)
	return out
}

func storedCatExplorable(t *testing.T, db *gorm.DB, uri string) (isCat, isExplorable bool) {
	t.Helper()
	var row model.User
	require.NoError(t, db.Where(`"uri" = ?`, uri).First(&row).Error)
	return row.IsCat, row.IsExplorable
}

// リモートの actor の isCat と discoverable を、作るときに取り込む (#4022)。
// isExplorable は既定値 (true) を持つ列で、GORM は false を INSERT から外すので、
// 実 DB で確かめる。
func TestResolveActor_DB_StoresIsCatAndDiscoverable(t *testing.T) {
	db := openResyncDB(t)
	for _, tc := range []struct {
		name           string
		extra          string
		wantCat        bool
		wantExplorable bool
	}{
		{"cat and not discoverable", `"isCat":true,"discoverable":false`, true, false},
		{"discoverable", `"isCat":false,"discoverable":true`, false, true},
		// 本家は無い discoverable を書かないので、列の既定値 (true) になる
		{"neither", ``, false, true},
		// null は本家では取り込みに失敗する。こちらは無いときと同じに扱う (緩い方向)
		{"null discoverable", `"discoverable":null`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uri := "https://cat-explorable.example/users/ce_" + strings.ReplaceAll(tc.name, " ", "_")
			cleanupActor(t, db, uri)
			f := newRouteFetcher()
			f.bodies[uri] = actorDocWith(t, uri, "ce_user", tc.extra)
			r := newAccountCreatedAtResolver(t, db, f)

			u, err := r.ResolveActor(uri)
			require.NoError(t, err)
			assert.Equal(t, tc.wantCat, u.IsCat)
			assert.Equal(t, tc.wantExplorable, u.IsExplorable)
			isCat, isExplorable := storedCatExplorable(t, db, uri)
			assert.Equal(t, tc.wantCat, isCat, "isCat を列に保存すること")
			assert.Equal(t, tc.wantExplorable, isExplorable, "isExplorable を列に保存すること")
		})
	}
}

// 更新でも追従する。discoverable が無いときは今の値のまま (本家の updatePerson
// は undefined を書かない)。
func TestResolveActor_DB_RefreshFollowsIsCatAndDiscoverable(t *testing.T) {
	db := openResyncDB(t)
	const uri = "https://cat-explorable.example/users/ce_refresh"
	cleanupActor(t, db, uri)
	f := newRouteFetcher()
	f.bodies[uri] = actorDocWith(t, uri, "ce_refresh", `"isCat":true,"discoverable":false`)
	r := newAccountCreatedAtResolver(t, db, f)
	_, err := r.ResolveActor(uri)
	require.NoError(t, err)

	f.bodies[uri] = actorDocWith(t, uri, "ce_refresh", ``)
	u, err := r.ForceResolveActor(uri)
	require.NoError(t, err)
	assert.False(t, u.IsCat, "isCat が無くなったら猫でなくなる")
	assert.False(t, u.IsExplorable, "discoverable が無いときは今の値 (false) のまま")
	isCat, isExplorable := storedCatExplorable(t, db, uri)
	assert.False(t, isCat)
	assert.False(t, isExplorable)

	f.bodies[uri] = actorDocWith(t, uri, "ce_refresh", `"isCat":true,"discoverable":true`)
	u, err = r.ForceResolveActor(uri)
	require.NoError(t, err)
	assert.True(t, u.IsCat)
	assert.True(t, u.IsExplorable)
	isCat, isExplorable = storedCatExplorable(t, db, uri)
	assert.True(t, isCat)
	assert.True(t, isExplorable)
}
