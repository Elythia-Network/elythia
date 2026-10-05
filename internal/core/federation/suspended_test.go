package federation

import (
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func strp(s string) *string { return &s }

// MatchSuspendedSoftware のマッチ判定を網羅する (display + deliver gate 共有)。
func TestMatchSuspendedSoftware(t *testing.T) {
	entries := []SuspendedSoftwareEntry{{Software: "Mastodon", VersionRange: "4.2.0"}}
	wildcard := []SuspendedSoftwareEntry{{Software: "Mastodon", VersionRange: "*"}}
	tests := []struct {
		name    string
		swName  *string
		swVer   *string
		entries []SuspendedSoftwareEntry
		want    bool
	}{
		{"wildcard version", strp("mastodon"), strp("4.2.0"), wildcard, true},
		{"exact version", strp("pixelfed"), strp("0.12.4"), []SuspendedSoftwareEntry{{Software: "Pixelfed", VersionRange: "0.12.4"}}, true},
		{"prefix dot match", strp("pleroma"), strp("2.6.3-beta1"), []SuspendedSoftwareEntry{{Software: "Pleroma", VersionRange: "2.6.3"}}, true},
		{"prefix sub match", strp("akkoma"), strp("3.10.4"), []SuspendedSoftwareEntry{{Software: "Akkoma", VersionRange: "3.10"}}, true},
		{"no version match", strp("mastodon"), strp("4.3.0"), entries, false},
		{"different software", strp("misskey"), strp("2024.1.0"), wildcard, false},
		{"nil software name", nil, strp("4.2.0"), wildcard, false},
		{"nil version + wildcard", strp("mastodon"), nil, wildcard, true},
		{"nil version + specific", strp("mastodon"), nil, entries, false},
		{"empty entries", strp("mastodon"), strp("4.2.0"), nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchSuspendedSoftware(tt.swName, tt.swVer, tt.entries))
		})
	}
}

func TestNewSuspendedChecker_InvalidJSON(t *testing.T) {
	c := NewSuspendedChecker([]byte("invalid"), nil)
	assert.Empty(t, c.entries)
}

func TestSuspendedChecker_EmptyHost(t *testing.T) {
	c := NewSuspendedChecker([]byte(`[{"software":"mastodon","versionRange":"*"}]`), nil)
	assert.False(t, c.IsSuspended(""))
}

func TestSuspendedChecker_EmptyEntries(t *testing.T) {
	c := NewSuspendedChecker([]byte(`[]`), nil)
	assert.False(t, c.IsSuspended("example.com"))
}

// IsSuspended は host の instance を引いて software をマッチさせる。
func TestSuspendedChecker_IsSuspended_MatchAndMiss(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	name := "mastodon"
	ver := "4.2.0"
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "blocked.example", SoftwareName: &name, SoftwareVersion: &ver}))
	other := "misskey"
	require.NoError(t, repo.Create(&model.Instance{ID: "i2", Host: "ok.example", SoftwareName: &other, SoftwareVersion: &ver}))

	c := NewSuspendedChecker([]byte(`[{"software":"mastodon","versionRange":"*"}]`), repo)
	assert.True(t, c.IsSuspended("blocked.example"), "mastodon は suspend 対象")
	assert.False(t, c.IsSuspended("ok.example"), "misskey は対象外")
	// instance レコードが無い host は false (FindByHost error)。
	assert.False(t, c.IsSuspended("unknown.example"))
}
