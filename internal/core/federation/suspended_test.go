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
// バージョンの範囲は本家と同じ semver で読む (#3558)。範囲の読み方そのものは
// internal/misc/semverrange が本家の semver との対照表で守る。
func TestMatchSuspendedSoftware(t *testing.T) {
	entries := []SuspendedSoftwareEntry{{Software: "Mastodon", VersionRange: "4.2.0"}}
	wildcard := []SuspendedSoftwareEntry{{Software: "Mastodon", VersionRange: "*"}}
	upTo := []SuspendedSoftwareEntry{{Software: "misskey", VersionRange: "<=2024.11.0"}}
	tests := []struct {
		name    string
		swName  *string
		swVer   *string
		entries []SuspendedSoftwareEntry
		want    bool
	}{
		{"wildcard version", strp("mastodon"), strp("4.2.0"), wildcard, true},
		{"exact version", strp("pixelfed"), strp("0.12.4"), []SuspendedSoftwareEntry{{Software: "Pixelfed", VersionRange: "0.12.4"}}, true},
		{"no version match", strp("mastodon"), strp("4.3.0"), entries, false},
		{"different software", strp("misskey"), strp("2024.1.0"), wildcard, false},
		{"nil software name", nil, strp("4.2.0"), wildcard, false},
		{"nil version + wildcard", strp("mastodon"), nil, wildcard, true},
		{"nil version + padded wildcard", strp("mastodon"), nil, []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: " * "}}, true},
		{"nil version + specific", strp("mastodon"), nil, entries, false},
		{"empty entries", strp("mastodon"), strp("4.2.0"), nil, false},
		// 範囲で書けること (#3558)。以前は前方一致だけで、どれも当たらなかった。
		{"at or below: equal", strp("misskey"), strp("2024.11.0"), upTo, true},
		{"at or below: older", strp("misskey"), strp("13.14.2"), upTo, true},
		{"at or below: newer", strp("misskey"), strp("2025.4.0"), upTo, false},
		// 本家と同じく includePrerelease: true なので、プレリリースも範囲で比べる。
		{"at or below: prerelease", strp("misskey"), strp("2024.11.0-beta.3"), upTo, true},
		{"and range", strp("mastodon"), strp("4.2.9+glitch"), []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: ">=4.0.0 <4.3.0"}}, true},
		// 前方一致はもうしない (本家の semver では 4.2.0 は 4.2.0 にしか当たらない)。
		{"no prefix match", strp("pleroma"), strp("2.6.3-beta1"), []SuspendedSoftwareEntry{{Software: "pleroma", VersionRange: "2.6.3"}}, false},
		// x-range で前方一致に近いことは書ける。
		{"x-range", strp("akkoma"), strp("3.10.4"), []SuspendedSoftwareEntry{{Software: "akkoma", VersionRange: "3.10.x"}}, true},
		// "*" は semver として読めないバージョンにも当てる (本家は当てない。以前の
		// Elythia の「全バージョン」を残す、#3558)。
		{"unparsable version + wildcard", strp("friendica"), strp("2024.08"), []SuspendedSoftwareEntry{{Software: "friendica", VersionRange: "*"}}, true},
		// 読めないバージョンは "*" 以外の範囲には当たらない (本家と同じ)。
		{"unparsable version + range", strp("friendica"), strp("2024.08"), []SuspendedSoftwareEntry{{Software: "friendica", VersionRange: "<=2025"}}, false},
		// 空の範囲は本家と同じく全バージョンに当たる。TS 版の「名前だけ入れた」設定を
		// 移行しても配送停止が外れないこと (#3558)。
		{"empty range", strp("mastodon"), strp("4.2.0"), []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: ""}}, true},
		{"blank range", strp("mastodon"), strp("4.2.0"), []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: " \u3000 "}}, true},
		// 管理画面の「追加」が足す行は名前も空なので、どの相手にも当たらない。
		{"empty row", strp("mastodon"), strp("4.2.0"), []SuspendedSoftwareEntry{{Software: "", VersionRange: ""}}, false},
		// 空の範囲でも、バージョンが無い相手には当てない (本家と同じ)。
		{"empty range + nil version", strp("mastodon"), nil, []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: ""}}, false},
		// 空白は本家の trim と同じく除く (U+FEFF は除き、U+0085 は除かない)。
		{"nil version + BOM padded wildcard", strp("mastodon"), nil, []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: "\ufeff*"}}, true},
		{"nil version + NEL padded wildcard", strp("mastodon"), nil, []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: "*\u0085"}}, false},
		{"invalid range", strp("mastodon"), strp("4.2.0"), []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: ">="}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchSuspendedSoftware(tt.swName, tt.swVer, tt.entries))
		})
	}
}

func TestParseSuspendedSoftware(t *testing.T) {
	assert.Empty(t, ParseSuspendedSoftware(nil))
	assert.Empty(t, ParseSuspendedSoftware([]byte("invalid")))
	assert.Empty(t, ParseSuspendedSoftware([]byte(`{"software":"mastodon"}`)), "配列でなければ空")
	assert.Equal(t, []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: "*"}},
		ParseSuspendedSoftware([]byte(`[{"software":"mastodon","versionRange":"*"}]`)))
	// **一部だけ壊れていても、読めた要素は使う (#3558)。** 表示は読めた分を使うので、
	// 全体を捨てると「画面では配送停止なのに配送が続く」状態になる。読めない要素は
	// 中途半端な値 (versionRange が空) で残さず、丸ごと飛ばす。
	assert.Equal(t, []SuspendedSoftwareEntry{{Software: "mastodon", VersionRange: "*"}},
		ParseSuspendedSoftware([]byte(`[{"software":"mastodon","versionRange":"*"},{"software":"x","versionRange":1}]`)))
}

func suspendedMeta(entries string) *testutil.MockMetaRepository {
	m := testutil.NewMockMetaRepository()
	m.Meta = &model.Meta{DeliverSuspendedSoftware: []byte(entries)}
	return m
}

func TestSuspendedChecker_EmptyHostOrUnwired(t *testing.T) {
	c := NewSuspendedChecker(suspendedMeta(`[{"software":"mastodon","versionRange":"*"}]`), testutil.NewMockInstanceRepository())
	assert.False(t, c.IsSuspended(""))
	assert.False(t, NewSuspendedChecker(nil, nil).IsSuspended("example.com"))
}

func TestSuspendedChecker_EmptyEntriesOrMetaError(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	assert.False(t, NewSuspendedChecker(suspendedMeta(`[]`), repo).IsSuspended("example.com"))
	failing := testutil.NewMockMetaRepository() // Meta が nil なので Fetch は失敗する
	assert.False(t, NewSuspendedChecker(failing, repo).IsSuspended("example.com"))
}

// IsSuspended は host の instance を引いて software をマッチさせる。
func TestSuspendedChecker_IsSuspended_MatchAndMiss(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	name := "mastodon"
	ver := "4.2.0"
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "blocked.example", SoftwareName: &name, SoftwareVersion: &ver}))
	other := "misskey"
	require.NoError(t, repo.Create(&model.Instance{ID: "i2", Host: "ok.example", SoftwareName: &other, SoftwareVersion: &ver}))

	c := NewSuspendedChecker(suspendedMeta(`[{"software":"mastodon","versionRange":"*"}]`), repo)
	assert.True(t, c.IsSuspended("blocked.example"), "mastodon は suspend 対象")
	assert.False(t, c.IsSuspended("ok.example"), "misskey は対象外")
	// instance レコードが無い host は false (FindByHost error)。
	assert.False(t, c.IsSuspended("unknown.example"))
}

// **設定を変えたら、作り直さなくても次の判定から効くこと (#3558)。** 以前は
// 起動時に読んだ設定を持ち続けたので、再起動するまで配送に効かなかった。
func TestSuspendedChecker_ReadsCurrentSetting(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	name := "misskey"
	ver := "2024.10.0"
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "old.example", SoftwareName: &name, SoftwareVersion: &ver}))
	meta := suspendedMeta(`[]`)
	c := NewSuspendedChecker(meta, repo)
	require.False(t, c.IsSuspended("old.example"))

	meta.Meta = &model.Meta{DeliverSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`)}
	assert.True(t, c.IsSuspended("old.example"), "足した設定が次の判定から効くこと")

	meta.Meta = &model.Meta{DeliverSuspendedSoftware: []byte(`[]`)}
	assert.False(t, c.IsSuspended("old.example"), "外した設定も次の判定から効くこと")
}
