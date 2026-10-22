package server

import (
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/elythia-network/elythia/internal/entitycompat"
)

// TestCaches_ListMatchesTheDropLocalTypes checks that the types of the values
// in the cache list are exactly the types that declare DropLocal in the
// sources the cache-check gate reads (#3777). The gate makes every type that
// holds sync parts either have DropLocal or be classified in its table; this
// test makes every type with DropLocal be in the list. Together, a cache that
// is not in the list fails one of them.
//
// 型の集合は、ゲートと同じentitycompat.ScanCacheTypesで読む(一覧を書き写すと、
// 片方だけ直してずれる)。タイムラインのJSONはopt-inなので、有効にして組む。
// **比べるのは型の集合なので、同じ型の2つ目の値を登録し忘れても気付けない。**
// 読み直しの項目(reloadOnDrop)は2つが同じ型なので、名前はTestCaches_ReloadedFromMeta
// が見る。型引数の付いた型は名前に型引数が入るので一致しない(今は無い)。
func TestCaches_ListMatchesTheDropLocalTypes(t *testing.T) {
	// 組み立ての前に読む。ハーネスは作業ディレクトリを一時ディレクトリへ移す。
	scan, err := entitycompat.ScanCacheTypes(repoRootDir(t))
	require.NoError(t, err)
	require.Contains(t, scan.DropLocal, "internal/repository.CachedMetaRepository", "前提: DropLocalの型を読めていない")

	h := newMetaRuntimeHarness(t, func(s *Server) {
		s.config.EnableTimelineCache = true
		s.config.TimelineCacheTTLSeconds = 3600
	})

	const module = "github.com/elythia-network/elythia/"
	listed := map[string][]string{}
	for _, p := range h.srv.caches.parts {
		typ := reflect.TypeOf(p.cache)
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		key := strings.TrimPrefix(typ.PkgPath(), module) + "." + typ.Name()
		listed[key] = append(listed[key], p.name)
	}
	for key, names := range listed {
		assert.True(t, scan.DropLocal[key], "%s (listed as %q) is not a type the gate reads as having DropLocal", key, names)
	}
	for key := range scan.DropLocal {
		assert.Contains(t, listed, key, "%s has DropLocal but no value of it is in the cache list (add it with addCache)", key)
	}
}
