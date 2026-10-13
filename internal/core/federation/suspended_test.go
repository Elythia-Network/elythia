package federation

import (
	"fmt"
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

// **`/.../` で書くと、バージョンの文字列に正規表現を当てる (Elythia 独自、#3362)。**
// fork が付けるサフィックスは semver ではプレリリースとして読まれ、fork 側の版で
// 範囲を書けないので、文字列そのものに当てる書き方を足した。
func TestMatchSuspendedSoftware_Regexp(t *testing.T) {
	entry := func(r string) []SuspendedSoftwareEntry {
		return []SuspendedSoftwareEntry{{Software: "misskey", VersionRange: r}}
	}
	tests := []struct {
		name string
		ver  *string
		rng  string
		want bool
	}{
		{"fork suffix match", strp("2024.11.1-cherrypick-4.12.0"), `/-cherrypick-4\.1[0-3]\./`, true},
		{"fork suffix miss", strp("2024.11.1-cherrypick-4.20.0"), `/-cherrypick-4\.1[0-3]\./`, false},
		// 末尾の i で大文字と小文字を区別しない。
		{"case-insensitive", strp("4.3.2+GLITCH"), "/glitch$/i", true},
		{"case-sensitive", strp("4.3.2+GLITCH"), "/glitch$/", false},
		// semver として読めないバージョンにも当てられる。
		{"unparsable version", strp("2024.08-dev"), "/^2024\\.08/", true},
		// バージョンが無い相手には当てない。
		{"nil version", nil, "/.*/", false},
		// コンパイルできないパターンは何にも当てない。
		{"invalid pattern", strp("1.2.3"), "/(/", false},
		// 閉じていないものは正規表現として読まず、semver の範囲として読む
		// (範囲としても不正なので当たらない)。
		{"not closed", strp("1.2.3"), "/1.2.3", false},
		{"slash only", strp("1.2.3"), "/", false},
		// 前後の空白は除く。
		{"padded", strp("2024.11.1-cherrypick-4.12.0"), "  /cherrypick/  ", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MatchSuspendedSoftware(strp("misskey"), tt.ver, entry(tt.rng)))
		})
	}
}

// コンパイルしたパターンは覚えておき、上限を超えたら捨てる。
func TestVersionRegexpCache(t *testing.T) {
	versionRegexps.Lock()
	saved := versionRegexps.m
	versionRegexps.m = make(map[string]compiledVersionRegexp)
	versionRegexps.Unlock()
	t.Cleanup(func() {
		versionRegexps.Lock()
		versionRegexps.m = saved
		versionRegexps.Unlock()
	})

	// 同じ範囲は 2 回目に覚えたものを返す (コンパイルし直さない)。
	first, ok := versionRegexp("/^1\\./")
	require.True(t, ok)
	second, _ := versionRegexp("/^1\\./")
	assert.Same(t, first, second, "覚えたパターンを使うこと")

	for i := 0; i < versionRegexpCacheMax+10; i++ {
		_, ok := versionRegexp(fmt.Sprintf("/^%d\\./", i))
		require.True(t, ok)
	}
	versionRegexps.Lock()
	n := len(versionRegexps.m)
	versionRegexps.Unlock()
	assert.LessOrEqual(t, n, versionRegexpCacheMax)
	// 上限で丸ごと捨てるので、捨てた後に覚えた分 (上限より十分少ない) だけが残る。
	assert.Positive(t, n)
	assert.Less(t, n, versionRegexpCacheMax/2, "上限に達したら捨てること")
	// 捨てた後でも同じ結果になる。
	assert.True(t, MatchSuspendedSoftware(strp("misskey"), strp("0.1.0"),
		[]SuspendedSoftwareEntry{{Software: "misskey", VersionRange: "/^0\\./"}}))
}

// **受信の checker は inboxSuspendedSoftware を読み、配送の一覧は読まない (#3362)。**
func TestInboxSuspendedChecker_ReadsInboxSetting(t *testing.T) {
	repo := testutil.NewMockInstanceRepository()
	name := "misskey"
	ver := "2024.10.0"
	require.NoError(t, repo.Create(&model.Instance{ID: "i1", Host: "old.example", SoftwareName: &name, SoftwareVersion: &ver}))
	meta := testutil.NewMockMetaRepository()
	meta.Meta = &model.Meta{
		DeliverSuspendedSoftware: []byte(`[]`),
		InboxSuspendedSoftware:   []byte(`[{"software":"misskey","versionRange":"<=2024.11.0"}]`),
	}
	assert.True(t, NewInboxSuspendedChecker(meta, repo).IsSuspended("old.example"))
	assert.False(t, NewSuspendedChecker(meta, repo).IsSuspended("old.example"), "配送の判定は受信の一覧を読まない")

	meta.Meta = &model.Meta{
		DeliverSuspendedSoftware: []byte(`[{"software":"misskey","versionRange":"*"}]`),
		InboxSuspendedSoftware:   []byte(`[]`),
	}
	assert.False(t, NewInboxSuspendedChecker(meta, repo).IsSuspended("old.example"), "受信の判定は配送の一覧を読まない")
	assert.True(t, NewSuspendedChecker(meta, repo).IsSuspended("old.example"))
}

// /.../ の形なのに正規表現として読めないもの (コンパイルできない、閉じる / の後に
// i 以外が続く) を不正とする。admin/update-meta が保存の時点で弾く (#3362)。
func TestInvalidVersionRegexp(t *testing.T) {
	for rng, want := range map[string]bool{
		"/(/":         true,
		"/x/I":        true,
		"/x/g":        true,
		"/x/ i":       true,
		"/a/b/x":      true,
		"/-fork-/":    false,
		"/-fork-/i":   false,
		"  /fork/i  ": false,
		"/a/b/":       false,
		// 閉じていないものは semver の範囲として読むので、ここでは弾かない。
		"/1.2.3":  false,
		"/":       false,
		"*":       false,
		"<=1.2.3": false,
		"":        false,
	} {
		assert.Equal(t, want, InvalidVersionRegexp(rng), "%q", rng)
	}
}
