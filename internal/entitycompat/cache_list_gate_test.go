package entitycompat

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// キャッシュの一覧(docs/architecture.md 4.4、#3721)への登録漏れを落とす(#3777)。
//
// 一覧の登録(internal/serverのaddCache)は、キャッシュを足した人が書き忘れても
// 何も落ちない。並行で共有するメモリには同期が要り、-raceがそれを強制するので、
// 普通に書いたキャッシュは同期の部品(sync.Mutex・RWMutex・Map・Once、sync/atomic、
// singleflight.Group、lru.Cache、expirable.LRU)をフィールドか、パッケージの変数に
// 持つ。そこで、走査が拾うもの(CacheTypeの説明)を全部、cacheEntriesの表で人に
// 分類させる。
//
//   - kindDropLocal: DropLocal()を持つ。一覧に登録したことはinternal/serverの
//     TestCaches_ListMatchesTheDropLocalTypesが実行時に確かめる
//   - kindDroppedByOwner: 同じパッケージでDropLocal()を持つ型からフィールドの型を
//     辿って届き、その型のDropLocalが捨てるか、DropLocalの後の次の読み込みで置き換わる
//     (fedrule.compiledRuleは、集合を古いことにされて次の評価で読み直した集合に替わる)。
//     辿って届くだけでは捨てるとは限らない
//     (mentionFetchGuardのように、持ち主が捨てないものもある)ので、人が確かめて書く
//   - それ以外の種類: 一覧に入れない理由
//
// **表は全部のフィールドの名前を持つ。** どのフィールドを足しても(mutexで守る値を
// ポインタで足す形も)一致しなくなって落ちるので、足した人が分類を確かめ直す。
// kindDropLocalとkindDroppedByOwnerの行は、DropLocalが捨てずに残す状態(実行中の
// 読み込み、間引きなど)をkeepsに理由付きで書く。keepsに無いフィールドの状態は、
// DropLocalが捨てるか、構築のときに決まる依存や設定。この区別は人が読んで書くもので、
// 検査は名前の一致だけを見る。
//
// **見るのは「普通に書いたときに踏む形」だけ。** 同期の部品を別のパッケージの
// 型に包んで持つ(フィールドの型がinternalの別の型)形は、包む側を数えない。包まれる
// 側の型が数えられて分類される。意図的な回避は振る舞いのテストで受ける
// (CLAUDE.md Section 4)。

// cacheEntryKind is how an entry of the scan relates to the cache list.
type cacheEntryKind string

const (
	// kindDropLocal declares DropLocal and is in the cache list.
	kindDropLocal cacheEntryKind = "dropLocal"
	// kindDroppedByOwner is reached from a type of its package that declares
	// DropLocal, and that DropLocal drops it, or makes it be replaced on the
	// next load.
	kindDroppedByOwner cacheEntryKind = "droppedByOwner"
	// kindNotCache holds no value read from the database, the configuration,
	// other servers or files: locks, in-flight deduplication, lifecycles,
	// counters and metrics.
	kindNotCache cacheEntryKind = "notCache"
	// kindWriteBuffer holds writes (or notices to other processes) not yet
	// made. Dropping them loses them; stage 6 of #3666 decides whether to
	// flush or drop them when the database is restored.
	kindWriteBuffer cacheEntryKind = "writeBuffer"
	// kindFollowsRegistered is built from a registered cache and checks it on
	// each use, so dropping that cache drops it too.
	kindFollowsRegistered cacheEntryKind = "followsRegistered"
	// kindReloadOnDrop is read again by a reload item of the cache list
	// (reloadOnDrop in internal/server).
	kindReloadOnDrop cacheEntryKind = "reloadOnDrop"
	// kindPerConnection is the state of one streaming connection or channel.
	// It is read from the database when the connection or channel starts and
	// stays while it is open; stage 6 of #3666 closes or re-reads the streams.
	kindPerConnection cacheEntryKind = "perConnection"
	// kindPerCall is built for one call (one verification, one notification,
	// one resolution) and dropped with it.
	kindPerCall cacheEntryKind = "perCall"
	// kindThrottle remembers when to do something again (rate limits,
	// backoffs). Dropping it only lifts the throttle.
	kindThrottle cacheEntryKind = "throttle"
	// kindContentKeyed is keyed by the content it was computed from, so it
	// cannot become stale.
	kindContentKeyed cacheEntryKind = "contentKeyed"
	// kindProcessGlobal is package-level state that outlives the generations
	// (hooks wired at construction, settings written once, marks). Stage 5 of
	// #3666 passes these as dependencies.
	kindProcessGlobal cacheEntryKind = "processGlobal"
)

var cacheEntryKinds = []cacheEntryKind{
	kindDropLocal, kindDroppedByOwner, kindNotCache, kindWriteBuffer, kindFollowsRegistered,
	kindReloadOnDrop, kindPerConnection, kindPerCall, kindThrottle, kindContentKeyed, kindProcessGlobal,
}

// cacheEntry is one row of cacheEntries.
type cacheEntry struct {
	kind cacheEntryKind
	// reason says where the values come from and why the entry is (or is not)
	// dropped. kindDropLocalの行では空でよい(一覧の名前とdocs/architecture.md 4.4に
	// 書いてある)。
	reason string
	// fields is CacheType.Fingerprint: the sorted names of all the fields.
	fields []string
	// keeps are the fields whose state DropLocal intentionally keeps, with
	// the reason. kindDropLocalとkindDroppedByOwnerの行だけが持つ。
	keeps map[string]string
}

// cacheTypeLowerBounds name an existing entry for each detection branch of
// ScanCacheTypes. どの枝も、抽出が空振りしたら落ちるように実在するものを名指しする。
var cacheTypeLowerBounds = map[string]string{
	SyncPartMutex:        "internal/repository.RelationCache",
	SyncPartRWMutex:      "internal/repository.CachedMetaRepository",
	SyncPartMap:          "internal/server/middleware.tokenCache",
	SyncPartOnce:         "internal/core/notification.NoteSource",
	SyncPartAtomic:       "internal/api/emojis.Handler",
	SyncPartSingleflight: "internal/api/hashtags.Handler",
	SyncPartLRU:          "internal/server.twemojiBadgeHandler",
	SyncPartExpirable:    "internal/core/urlpreview.previewCache",
}

func scanCacheTypes(t *testing.T) *CacheTypeScan {
	t.Helper()
	scan, err := ScanCacheTypes(repoRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

// TestCacheTypesAreClassified checks that every entry of the scan has a row
// in cacheEntries with the same fields and a kind that matches what the scan
// sees.
func TestCacheTypesAreClassified(t *testing.T) {
	scan := scanCacheTypes(t)
	keys := make([]string, 0, len(scan.Types))
	for key := range scan.Types {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		ct := scan.Types[key]
		row, ok := cacheEntries[key]
		if !ok {
			t.Errorf("%s (%s, holds %v) is not in cacheEntries. "+
				"値のキャッシュならDropLocal()を付けてinternal/serverの一覧(addCache)に登録し、kindDropLocalで書く。"+
				"キャッシュでないなら、種類と理由(値がどこから来るか)を書く(docs/architecture.md 4.4)。fields: %s",
				key, ct.Pos, sortedParts(ct), goStrings(ct.Fingerprint))
			continue
		}
		if !slices.Equal(row.fields, ct.Fingerprint) {
			added, removed := diffNames(ct.Fingerprint, row.fields)
			t.Errorf("%s (%s): fields added %q, removed %q. "+
				"足したフィールドがキャッシュ(DBや外から読んだ値)なら、DropLocalで捨てるか、keepsに残す理由を書く。"+
				"除外した型なら、まだ除外の理由が成り立つかを確かめる。fields: %s",
				key, ct.Pos, added, removed, goStrings(ct.Fingerprint))
		}
		switch row.kind {
		case kindDropLocal:
			if !scan.DropLocal[key] {
				t.Errorf("cacheEntries[%q] is kindDropLocal, but the type has no DropLocal", key)
			}
		case kindDroppedByOwner:
			if scan.OwnedBy[key] == "" {
				t.Errorf("cacheEntries[%q] is kindDroppedByOwner, but no type of its package with DropLocal reaches it", key)
			}
		default:
			if scan.DropLocal[key] {
				t.Errorf("cacheEntries[%q] is %s, but the type has DropLocal; register it and make the row kindDropLocal", key, row.kind)
			}
		}
		if ct.Var && (row.kind == kindDropLocal || row.kind == kindDroppedByOwner) {
			t.Errorf("cacheEntries[%q] is a package-level var; no DropLocal drops it", key)
		}
	}
}

// TestCacheEntriesHaveNoDeadRows fails on rows of cacheEntries that the scan
// does not find, and on rows that are not filled in correctly.
func TestCacheEntriesHaveNoDeadRows(t *testing.T) {
	scan := scanCacheTypes(t)
	keys := make([]string, 0, len(cacheEntries))
	for key := range cacheEntries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		row := cacheEntries[key]
		if scan.Types[key] == nil {
			t.Errorf("cacheEntries[%q]: the scan does not find it any more; remove the row", key)
		}
		if !slices.Contains(cacheEntryKinds, row.kind) {
			t.Errorf("cacheEntries[%q]: unknown kind %q", key, row.kind)
		}
		if row.kind != kindDropLocal && strings.TrimSpace(row.reason) == "" {
			t.Errorf("cacheEntries[%q]: the reason is empty", key)
		}
		if len(row.keeps) > 0 && row.kind != kindDropLocal && row.kind != kindDroppedByOwner {
			t.Errorf("cacheEntries[%q]: keeps is only for the rows DropLocal drops", key)
		}
		for field, why := range row.keeps {
			if !slices.Contains(row.fields, field) {
				t.Errorf("cacheEntries[%q]: keeps names %q, which is not a field", key, field)
			}
			if strings.TrimSpace(why) == "" {
				t.Errorf("cacheEntries[%q]: keeps[%q] has no reason", key, field)
			}
		}
	}
}

// TestCacheTypeScanFindsEveryBranch pins an entry per detection branch and per
// kind of entry, so a scan that stops matching fails instead of passing on
// nothing.
func TestCacheTypeScanFindsEveryBranch(t *testing.T) {
	scan := scanCacheTypes(t)
	parts := make([]string, 0, len(cacheTypeLowerBounds))
	for part := range cacheTypeLowerBounds {
		parts = append(parts, part)
	}
	sort.Strings(parts)
	for _, part := range parts {
		key := cacheTypeLowerBounds[part]
		ct := scan.Types[key]
		if ct == nil || !ct.Parts[part] {
			t.Errorf("the scan does not find %s in %s", part, key)
		}
	}
	if !scan.DropLocal["internal/repository.CachedMetaRepository"] {
		t.Error("the scan does not find DropLocal of internal/repository.CachedMetaRepository")
	}
	// DropLocalはfuncの型にも付く(一覧の読み直しの項目)。
	if !scan.DropLocal["internal/server.reloadOnDrop"] {
		t.Error("the scan does not find DropLocal of internal/server.reloadOnDrop")
	}
	// 同期の部品を持たないDropLocalの型も表に載る。
	if ct := scan.Types["internal/api/meta.Handler"]; ct == nil || len(ct.Parts) != 0 {
		t.Error("the scan does not list internal/api/meta.Handler, a DropLocal type without sync parts")
	}
	if got := scan.OwnedBy["internal/api/meta.metaResponseCache"]; got != "internal/api/meta.Handler" {
		t.Errorf("internal/api/meta.metaResponseCache is reached from %q, want internal/api/meta.Handler", got)
	}
	// 途中に同期の部品を持たない型(policyProvider)を挟んでも辿る。
	if got := scan.OwnedBy["internal/core/role.policyProviderRuntime"]; got != "internal/core/role.Service" {
		t.Errorf("internal/core/role.policyProviderRuntime is reached from %q, want internal/core/role.Service", got)
	}
	// 辿って届く型は、同期の部品を持たなくてもmapを持てば表に載る。
	if ct := scan.Types["internal/repository.relationKindCache"]; ct == nil || len(ct.Parts) != 0 {
		t.Error("the scan does not list internal/repository.relationKindCache, a reached type with maps and no sync parts")
	}
	// 指紋は全部のフィールドを持つ(同期の部品でもmapでもないものも)。
	if ct := scan.Types["internal/core/captcha.Service"]; ct == nil || !slices.Contains(ct.Fingerprint, "client") {
		t.Error("the fingerprint of internal/core/captcha.Service does not have its field client")
	}
	// パッケージの変数: 宣言の型の名前の無いstruct、値の形から引く型、ポインタ。
	vars := map[string]string{
		"internal/api/meta.setupLatch":            SyncPartAtomic,
		"internal/core/federation.versionRegexps": SyncPartMutex,
		"internal/core/wordmute.parsedCache":      SyncPartLRU,
		// sync.OnceValueの呼び出しは型を値の形から引けないので、別に数える。
		"internal/api/admin.metaColumnNames": SyncPartOnce,
	}
	for key, part := range vars {
		if ct := scan.Types[key]; ct == nil || !ct.Var || !ct.Parts[part] {
			t.Errorf("the scan does not find the package-level var %s holding %s", key, part)
		}
	}
	// 同じvarのブロックの他の名前も指紋に入る。
	if ct := scan.Types["plugin.registryMu"]; ct == nil || !slices.Contains(ct.Fingerprint, "var:registry") {
		t.Error("the fingerprint of plugin.registryMu does not have the var registry of its block")
	}
}

// TestCacheScanExclusionsAreUsed fails on an exclusion (an excluded directory,
// testdata or the entitycompat fixtures) that matches no tracked non-test .go
// file, so the exclusions do not outlive what they exclude.
func TestCacheScanExclusionsAreUsed(t *testing.T) {
	root := repoRoot(t)
	out, err := exec.Command("git", "-C", root, "ls-files", "--", "internal", "plugin").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	used := map[string]int{}
	for _, rel := range strings.Fields(string(out)) {
		if strings.HasSuffix(rel, ".go") && !strings.HasSuffix(rel, "_test.go") {
			used[cacheScanExclusion(rel)]++
		}
	}
	want := []string{"testdata", "fixture"}
	for dir := range CacheScanExcludedDirs {
		want = append(want, dir)
	}
	sort.Strings(want)
	for _, w := range want {
		if used[w] == 0 {
			t.Errorf("the exclusion %q matches no tracked non-test .go file; remove it", w)
		}
	}
}

func sortedParts(ct *CacheType) []string {
	parts := make([]string, 0, len(ct.Parts))
	for p := range ct.Parts {
		parts = append(parts, p)
	}
	sort.Strings(parts)
	return parts
}

// goStrings formats names as a Go string slice literal to paste into the
// table.
func goStrings(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = `"` + n + `"`
	}
	return "[]string{" + strings.Join(quoted, ", ") + "}"
}

// diffNames returns the names only in got and only in want.
func diffNames(got, want []string) (added, removed []string) {
	for _, n := range got {
		if !slices.Contains(want, n) {
			added = append(added, n)
		}
	}
	for _, n := range want {
		if !slices.Contains(got, n) {
			removed = append(removed, n)
		}
	}
	return added, removed
}

// TestScanCacheTypesShapes runs the scan on a small repository of its own and
// checks the shapes it has to recognize.
func TestScanCacheTypesShapes(t *testing.T) {
	root := t.TempDir()
	write := func(rel, src string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/a/a.go", `package a

import (
	"sync"
	xatomic "sync/atomic"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

type Embedded struct {
	sync.Mutex
	items map[string]int
}

type Aliased struct {
	n xatomic.Int64
}

type Nested struct {
	inner struct {
		mu sync.RWMutex
		m  map[string]int
	}
	done  struct{}
	other int
}

type Generic[K comparable, V any] struct {
	c *lru.Cache[K, V]
	e *expirable.LRU[K, V]
	g singleflight.Group
	o sync.Once
	s sync.Map
}

type Plain struct {
	m map[string]int
}

type Owner struct {
	child *link
	tab   table
}

type table struct {
	rows map[string]int
}

func (o *Owner) DropLocal() {}

type link struct {
	leaf []*leaf
}

type leaf struct {
	mu sync.Mutex
}

type fn func()

func (fn) DropLocal() {}

type GenOne[T any] struct {
	mu sync.Mutex
}

func (g *GenOne[T]) DropLocal() {}

type GenTwo[K comparable, V any] struct {
	mu sync.Mutex
}

func (g GenTwo[K, V]) DropLocal() {}
`)
	// 別名を付けずにimportしたgolang-lruは、パッケージ名(lru)で書く。
	write("internal/a/unaliased.go", `package a

import "github.com/hashicorp/golang-lru/v2"

type Unaliased struct {
	c *lru.Cache[string, int]
}
`)
	write("internal/a/vars.go", `package a

import (
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
)

var latch = struct {
	sync.Mutex
	m map[string]int
}{m: map[string]int{}}

var (
	mu      sync.Mutex
	guarded = map[string]int{}
)

var memo *lru.Cache[string, int]

var held = &leaf{}

var made = newLeaf()

func newLeaf() *leaf { return nil }

var plain = map[string]int{}

var locks = make(map[string]*sync.Mutex)

var _ = &leaf{}

var hot = sync.OnceValue(func() map[string]int { return nil })

var called = newLeaf
`)
	// fixtureとして読まないのはinternal/entitycompatの下だけで、他の場所の同じ名前は読む。
	write("internal/entitycompat/somefixture/f.go", "package somefixture\n\nimport \"sync\"\n\ntype InFixture struct{ mu sync.Mutex }\n")
	write("internal/a/otherfixture/f.go", "package otherfixture\n\nimport \"sync\"\n\ntype Scanned struct{ mu sync.Mutex }\n")
	write("internal/a/a_test.go", "package a\n\nimport \"sync\"\n\ntype InTest struct{ mu sync.Mutex }\n")
	write("internal/a/testdata/x.go", "package x\n\nimport \"sync\"\n\ntype InTestdata struct{ mu sync.Mutex }\n")
	write("internal/cli/c.go", "package cli\n\nimport \"sync\"\n\ntype InCLI struct{ mu sync.Mutex }\n")
	write("internal/b/b.go", "package b\n\nimport \"sync\"\n\ntype Untracked struct{ mu sync.Mutex }\n")
	for _, args := range [][]string{
		{"init", "-q"},
		{"add", "internal/a/a.go", "internal/a/unaliased.go", "internal/a/vars.go", "internal/a/a_test.go",
			"internal/a/testdata/x.go", "internal/entitycompat/somefixture/f.go", "internal/a/otherfixture/f.go",
			"internal/cli/c.go"},
	} {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	scan, err := ScanCacheTypes(root)
	if err != nil {
		t.Fatal(err)
	}
	if scan.Files != 4 {
		t.Errorf("read %d files, want 4 (a.go, unaliased.go, vars.go and otherfixture/f.go under internal/a)", scan.Files)
	}
	want := map[string][]string{
		"internal/a.Embedded":  {"Mutex", "items"},
		"internal/a.Aliased":   {"n"},
		"internal/a.Nested":    {"done", "inner.m", "inner.mu", "other"},
		"internal/a.Generic":   {"c", "e", "g", "o", "s"},
		"internal/a.leaf":      {"mu"},
		"internal/a.GenOne":    {"mu"},
		"internal/a.GenTwo":    {"mu"},
		"internal/a.Unaliased": {"c"},
		// DropLocalを持つ型は、同期の部品を持たなくても載る。
		"internal/a.Owner": {"child", "tab"},
		// 辿って届く型は、mapを持てば載る(linkはどちらも持たないので載らない)。
		"internal/a.table":                {"rows"},
		"internal/a/otherfixture.Scanned": {"mu"},
		"internal/a.latch":                {"Mutex", "m"},
		"internal/a.mu":                   {"var:guarded"},
		"internal/a.memo":                 nil,
		"internal/a.held":                 nil,
		"internal/a.locks":                nil,
		"internal/a.hot":                  nil,
	}
	for key, fp := range want {
		ct := scan.Types[key]
		if ct == nil {
			t.Errorf("%s is not found", key)
			continue
		}
		if !slices.Equal(ct.Fingerprint, fp) {
			t.Errorf("%s: fingerprint %q, want %q", key, ct.Fingerprint, fp)
		}
	}
	if len(scan.Types) != len(want) {
		var got []string
		for key := range scan.Types {
			got = append(got, key)
		}
		sort.Strings(got)
		t.Errorf("found %q, want only the keys of %v", got, want)
	}
	if ct := scan.Types["internal/a.Generic"]; ct != nil {
		for _, part := range []string{SyncPartLRU, SyncPartExpirable, SyncPartSingleflight, SyncPartOnce, SyncPartMap} {
			if !ct.Parts[part] {
				t.Errorf("internal/a.Generic: %s is not found", part)
			}
		}
	}
	if ct := scan.Types["internal/a.Aliased"]; ct != nil && !ct.Parts[SyncPartAtomic] {
		t.Error("internal/a.Aliased: sync/atomic imported under another name is not found")
	}
	for _, key := range []string{"internal/a.Owner", "internal/a.fn", "internal/a.GenOne", "internal/a.GenTwo"} {
		if !scan.DropLocal[key] {
			t.Errorf("DropLocal = %v, want %s", scan.DropLocal, key)
		}
	}
	if ct := scan.Types["internal/a.Unaliased"]; ct != nil && !ct.Parts[SyncPartLRU] {
		t.Error("internal/a.Unaliased: golang-lru imported without a name is not found")
	}
	for key, part := range map[string]string{
		"internal/a.latch": SyncPartMutex, "internal/a.mu": SyncPartMutex,
		"internal/a.memo": SyncPartLRU, "internal/a.held": SyncPartScannedType,
		"internal/a.hot": SyncPartOnce,
	} {
		if ct := scan.Types[key]; ct != nil && (!ct.Var || !ct.Parts[part]) {
			t.Errorf("%s: want a var holding %s, got var=%v parts=%v", key, part, ct.Var, ct.Parts)
		}
	}
	if got := scan.OwnedBy["internal/a.leaf"]; got != "internal/a.Owner" {
		t.Errorf("internal/a.leaf is reached from %q, want internal/a.Owner", got)
	}
}
