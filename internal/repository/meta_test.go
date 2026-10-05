package repository

import (
	"testing"

	"github.com/elythia-network/elythia/internal/config"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetaRepository_Fetch(t *testing.T) {
	repo := NewMetaRepository(testDB)

	name := "Test Instance"
	meta := &model.Meta{
		ID:   "m_f_1",
		Name: &name,
	}
	require.NoError(t, testDB.Create(meta).Error)
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, meta.ID)

	found, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, &name, found.Name)
}

// 行が無ければ本家 MetaService.fetch() と同じくその場で作って返す。
// 起動後に truncate されても全 API が 500 のまま固まらないための保証。
func TestMetaRepository_Fetch_CreatesWhenMissing(t *testing.T) {
	repo := NewMetaRepository(testDB)

	testDB.Exec(`DELETE FROM "meta"`)
	defer testDB.Exec(`DELETE FROM "meta"`)

	found, err := repo.Fetch()
	require.NoError(t, err)
	require.NotNil(t, found)
	assert.Equal(t, defaultMetaID, found.ID)

	// 2 回目は作り直さず同じ行を返す。
	again, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, found.ID, again.ID)

	var count int64
	require.NoError(t, testDB.Model(&model.Meta{}).Count(&count).Error)
	assert.EqualValues(t, 1, count, "行が重複して作られないこと")
}

func TestMetaRepository_Update(t *testing.T) {
	repo := NewMetaRepository(testDB)

	meta := &model.Meta{ID: "m_u_1"}
	require.NoError(t, testDB.Create(meta).Error)
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, meta.ID)

	newName := "Updated"
	require.NoError(t, repo.Update(map[string]any{"name": newName}))

	found, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, &newName, found.Name)
}

// model.StringArray を varchar[] 列に Update できることを担保する。admin
// handler 側の coerceMetaArrayFields が []any → model.StringArray 変換を
// 行った後で、ここで実 DB 経由で永続化されることを最後の砦として確認。
// #590 で修正したバグの regression test。
func TestMetaRepository_Update_FederationHosts(t *testing.T) {
	repo := NewMetaRepository(testDB)

	meta := &model.Meta{ID: "m_fh_1", Federation: "all"}
	require.NoError(t, testDB.Create(meta).Error)
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, meta.ID)

	require.NoError(t, repo.Update(map[string]any{
		"federation":      "specified",
		"federationHosts": model.StringArray{"allowed.example", "trusted.example"},
		"blockedHosts":    model.StringArray{"bad.example"},
	}))

	found, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "specified", found.Federation)
	assert.Equal(t, []string{"allowed.example", "trusted.example"}, []string(found.FederationHosts))
	assert.Equal(t, []string{"bad.example"}, []string(found.BlockedHosts))
}

// 旧バグ ([]any{"x"} を直接渡すと "expression is of type record" で UPDATE
// 失敗) の boundary を実 DB で明示する。Update 自体が error を返し、その
// 結果として string 列の federation も更新されないことを記録する。これに
// より将来 admin handler の coerce を外したり、別経路で同じ落とし穴に
// 嵌まったときに即座に気付ける。
func TestMetaRepository_Update_RawAnySliceFails(t *testing.T) {
	repo := NewMetaRepository(testDB)

	meta := &model.Meta{ID: "m_fhraw_1", Federation: "all"}
	require.NoError(t, testDB.Create(meta).Error)
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, meta.ID)

	err := repo.Update(map[string]any{
		"federation":      "specified",
		"federationHosts": []any{"allowed.example"},
	})
	require.Error(t, err, "varchar[] 列に []any を直接渡すと PostgreSQL がエラーを返す")

	// 同 transaction 内の string 列も巻き戻る。
	found, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "all", found.Federation, "UPDATE 全体がロールバックされる")
}

func TestMetaRepository_EnsureInitial_Creates(t *testing.T) {
	repo := NewMetaRepository(testDB)

	// 空の状態から初期行を作成する。
	testDB.Exec(`DELETE FROM "meta"`)

	require.NoError(t, repo.EnsureInitial("m_ei_1"))
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, "m_ei_1")

	got, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "m_ei_1", got.ID)
}

// 新規インスタンスでソースコードの案内が空にならないこと (#2700)。列 DEFAULT は
// GORM の NULL 明示挿入に負けるので、EnsureInitial 側で入れているかを見る。
func TestMetaRepository_EnsureInitial_SetsRepositoryURL(t *testing.T) {
	repo := NewMetaRepository(testDB)

	testDB.Exec(`DELETE FROM "meta"`)
	require.NoError(t, repo.EnsureInitial("m_ei_repo"))
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, "m_ei_repo")

	got, err := repo.Fetch()
	require.NoError(t, err)
	require.NotNil(t, got.RepositoryURL, "repositoryUrl が NULL のままだと /about-mkgo が案内を出せない")
	assert.Equal(t, defaultRepositoryURL, *got.RepositoryURL)
}

// Fetch は行が消えたときにも EnsureInitial を通るので、そちらの経路でも
// 既定値が入ること。
func TestMetaRepository_Fetch_RecreatesWithRepositoryURL(t *testing.T) {
	repo := NewMetaRepository(testDB)

	testDB.Exec(`DELETE FROM "meta"`)
	defer testDB.Exec(`DELETE FROM "meta"`)

	got, err := repo.Fetch()
	require.NoError(t, err)
	require.NotNil(t, got.RepositoryURL)
	assert.Equal(t, defaultRepositoryURL, *got.RepositoryURL)
}

// repository 層は config を import しない方針なので値を持ち直している。
// 二重管理が drift しないようにここで固定する (#2700)。
func TestDefaultRepositoryURLMatchesConfig(t *testing.T) {
	assert.Equal(t, config.MkGoRepositoryURL, defaultRepositoryURL,
		"nodeinfo の software.repository と meta.repositoryUrl の既定値がずれている")
}

// feedbackUrl 側も同じ理由で持ち直している (#2891)。
func TestDefaultFeedbackURLMatchesConfig(t *testing.T) {
	assert.Equal(t, config.MkGoFeedbackURL, defaultFeedbackURL,
		"meta.feedbackUrl の既定値が config とずれている")
}

// 新規インスタンスで /about のフィードバック導線と nodeinfo の metadata が
// 空にならないこと (#2891)。000029 の列 DEFAULT は GORM の NULL 明示挿入に
// 負けるので、EnsureInitial 側で入れているかを見る。
func TestMetaRepository_EnsureInitial_SetsFeedbackURL(t *testing.T) {
	repo := NewMetaRepository(testDB)

	testDB.Exec(`DELETE FROM "meta"`)
	require.NoError(t, repo.EnsureInitial("m_ei_fb"))
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, "m_ei_fb")

	got, err := repo.Fetch()
	require.NoError(t, err)
	require.NotNil(t, got.FeedbackURL, "feedbackUrl が NULL のままだとフィードバック導線が消える")
	assert.Equal(t, defaultFeedbackURL, *got.FeedbackURL)
}

// Fetch は行が消えたときにも EnsureInitial を通るので、そちらの経路でも
// 両方の既定値が入ること (#2700 の repositoryUrl 側と対称)。
func TestMetaRepository_Fetch_RecreatesWithFeedbackURL(t *testing.T) {
	repo := NewMetaRepository(testDB)

	testDB.Exec(`DELETE FROM "meta"`)
	defer testDB.Exec(`DELETE FROM "meta"`)

	got, err := repo.Fetch()
	require.NoError(t, err)
	require.NotNil(t, got.FeedbackURL)
	assert.Equal(t, defaultFeedbackURL, *got.FeedbackURL)
}

func TestMetaRepository_EnsureInitial_NoopWhenExists(t *testing.T) {
	repo := NewMetaRepository(testDB)

	testDB.Exec(`DELETE FROM "meta"`)
	existing := &model.Meta{ID: "m_ei_2"}
	require.NoError(t, testDB.Create(existing).Error)
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, existing.ID)

	// 既存行がある場合は何もせず、別 ID を渡しても既存のまま。
	require.NoError(t, repo.EnsureInitial("m_ei_other"))

	got, err := repo.Fetch()
	require.NoError(t, err)
	assert.Equal(t, "m_ei_2", got.ID)
}

// #3015: minimumUsernameLength が実 DB の列として往復すること。
//
// **mock では検出できない。** `MockMetaRepository.Update` は key で switch する
// ので、列名の綴り違いや migration 漏れがあっても緑のまま通る。GORM の
// `Updates(map)` は key をそのまま列名に使うので、ここだけが「本当にその列が
// あるか」を見る。
func TestMetaRepository_Update_MinimumUsernameLength(t *testing.T) {
	repo := NewMetaRepository(testDB)

	meta := &model.Meta{ID: "m_u_mul"}
	require.NoError(t, testDB.Create(meta).Error)
	defer testDB.Exec(`DELETE FROM "meta" WHERE id = ?`, meta.ID)

	// **読み戻しは id 指定で行う。** `repo.Fetch` は `db.First()` = id 最小の
	// 1 行なので、他のテストが残した行があると別の行を見てしまう。
	var stored model.Meta
	require.NoError(t, testDB.Where("id = ?", meta.ID).First(&stored).Error)
	// migration の DEFAULT が効いていること。既存インスタンスは列が増えても
	// 挙動が変わらない。
	assert.Equal(t, 1, stored.MinimumUsernameLength, "列の既定が 1 でない")

	require.NoError(t, repo.Update(map[string]any{"minimumUsernameLength": 5}))
	require.NoError(t, testDB.Where("id = ?", meta.ID).First(&stored).Error)
	assert.Equal(t, 5, stored.MinimumUsernameLength)
}
