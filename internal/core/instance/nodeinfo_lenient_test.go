package instance_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/instance"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fetchNodeinfo(t *testing.T, body string) *model.Instance {
	t.Helper()
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(body)}}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))
	return repo.Instances["remote.example"]
}

// **型が違う 1 フィールドで document 全部を失わない。** struct へ直接 decode
// していた頃は `{"software":{"name":123}}` で json.Unmarshal が error を返し、
// Fetch が中断して他の列も 1 つも入らなかった (#2726)。
func TestFetch_NonStringSoftwareNameKeepsOtherFields(t *testing.T) {
	got := fetchNodeinfo(t, `{
		"software": {"name": 123, "version": "1.2.3"},
		"openRegistrations": true,
		"metadata": {"nodeName": "Remote", "nodeDescription": "desc"}
	}`)
	require.NotNil(t, got.SoftwareName)
	// upstream の FetchInstanceMetadataService は string でなければ '?' を入れる。
	assert.Equal(t, "?", *got.SoftwareName)
	require.NotNil(t, got.SoftwareVersion)
	assert.Equal(t, "1.2.3", *got.SoftwareVersion)
	require.NotNil(t, got.OpenRegistrations)
	assert.True(t, *got.OpenRegistrations)
	require.NotNil(t, got.Name)
	assert.Equal(t, "Remote", *got.Name)
	require.NotNil(t, got.Description)
	assert.Equal(t, "desc", *got.Description)
}

// `software` そのものが object でないケースでも他の値は残る。
func TestFetch_NonObjectSoftwareKeepsMetadata(t *testing.T) {
	got := fetchNodeinfo(t, `{"software": "misskey", "metadata": {"nodeName": "Remote"}}`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "?", *got.SoftwareName)
	require.NotNil(t, got.Name)
	assert.Equal(t, "Remote", *got.Name)
}

// `software` が無い document でも '?' が入る (upstream は nodeinfo が取れた
// 時点で softwareName を必ず更新する)。
func TestFetch_MissingSoftwareStoresPlaceholder(t *testing.T) {
	got := fetchNodeinfo(t, `{"metadata": {"nodeName": "Remote"}}`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "?", *got.SoftwareName)
}

// softwareName は upstream と同じく lowercase で保存する。software block の
// 判定は元から case-insensitive なので回避には使えないが、federation/instances
// が返す値が upstream に近づく (**「揃う」ではない** — `strings.ToLower` と JS の
// `toLowerCase()` の残差は fetch_metadata.go の nodeinfoDocument のコメント)。
func TestFetch_SoftwareNameIsLowercased(t *testing.T) {
	got := fetchNodeinfo(t, `{"software": {"name": "MisSkey", "version": "1"}}`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
}

// 型が違う openRegistrations / metadata は落として残りを保存する。
func TestFetch_WrongTypesAreDropped(t *testing.T) {
	got := fetchNodeinfo(t, `{
		"software": {"name": "misskey", "version": 42},
		"openRegistrations": "yes",
		"metadata": ["not", "an", "object"]
	}`)
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
	assert.Nil(t, got.SoftwareVersion)
	assert.Nil(t, got.OpenRegistrations)
	assert.Nil(t, got.Name)
}

// JSON として壊れている document は従来どおり error。
func TestFetch_MalformedJSONStillFails(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	repo.Instances["remote.example"] = &model.Instance{ID: "i1", Host: "remote.example"}
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(`{"software":`)}}
	assert.Error(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))
}

// JSON の `null` は **JS で falsy**。upstream は `if (info)` で囲っているので
// nodeinfo が null なら software 系の列に一切触れない。`'?'` を書くと、壊れた
// nodeinfo を返す相手の softwareName を毎回上書きしてしまう。
//
// **「object ではないから」ではない** — `[]` / `123` は JSON の型としては
// array / number で object ではないが、truthy なので `'?'` が入る
// (`TestFetch_NonObjectNodeinfoStoresPlaceholder`)。
func TestFetch_NullDocumentDoesNotOverwriteSoftwareName(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	existing := "misskey"
	repo.Instances["remote.example"] = &model.Instance{
		ID: "i1", Host: "remote.example", SoftwareName: &existing,
	}
	fetcher := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(`null`)}}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher).Fetch("remote.example"))

	got := repo.Instances["remote.example"]
	require.NotNil(t, got.SoftwareName)
	assert.Equal(t, "misskey", *got.SoftwareName)
	// 一方、`{}` は truthy なので upstream と同じく '?' が入る。
	fetcher2 := &scriptedFetcher{bodies: [][]byte{[]byte(discoveryBody), []byte(`{}`)}}
	require.NoError(t, instance.NewFetchMetadataService(repo, fetcher2).Fetch("remote.example"))
	assert.Equal(t, "?", *repo.Instances["remote.example"].SoftwareName)
}
