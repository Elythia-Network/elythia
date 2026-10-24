package server

import (
	"errors"
	"fmt"
	"reflect"
)

// localCache is a cache of one server generation that can drop everything it
// holds in this process (#3666 stage 4, #3721). DropLocal must not tell the
// other processes (no publish, no invalidation hook): the caller decides who
// else has to drop, and a drop that spreads would make every process that
// drops its caches start a storm of notifications.
//
// 名前を Flush にしないのは、paramshadow.Recorder やリアクションのバッファ
// (corereaction の bufferedWriter) で Flush が「溜めた分を書き出す」という逆の
// 意味で使われているため。
type localCache interface {
	DropLocal()
}

// reloadOnDrop is an item of the cache list whose DropLocal reads its values
// again instead of dropping them (#3777). captchaのproviderの組と、リアクションの
// バッファの切り替えがこれで、どちらもmetaUpdatedと同じ読み直しの関数を渡す。
//
// 捨てずに読み直すのは、空にした状態が安全でない側に倒れるため。providerの組が
// 空ならVerifyは成功を返す(captcha無効と同じ)。読めなければ、読み直しの関数が
// 前の値を据え置く。読み直しはcachedMetaから読むので、一覧ではcachedMetaより
// 後に登録する。
type reloadOnDrop func()

// DropLocal reads the values again.
func (r reloadOnDrop) DropLocal() { r() }

// cacheOrigin is where the values a cache holds come from. It tells which
// caches have to be dropped when that source changes under the process (the
// database restored from a backup, a file rebuilt).
type cacheOrigin string

const (
	// cacheFromDB is a cache of rows (or of what is built from rows).
	cacheFromDB cacheOrigin = "db"
	// cacheFromConfig is a cache of what is built from the configuration.
	cacheFromConfig cacheOrigin = "config"
	// cacheFromExternal is a cache of what other servers answered.
	cacheFromExternal cacheOrigin = "external"
	// cacheFromFile is a cache of files on the disk.
	cacheFromFile cacheOrigin = "file"
)

func (o cacheOrigin) valid() bool {
	switch o {
	case cacheFromDB, cacheFromConfig, cacheFromExternal, cacheFromFile:
		return true
	}
	return false
}

// cachePart is one named cache of the cache list.
type cachePart struct {
	// name identifies the cache. It is unique within one generation and
	// stays the same across releases, so tests can refer to it.
	name   string
	origin cacheOrigin
	cache  localCache
}

// cacheList is the ordered list of the named caches of one server generation
// (#3666 stage 4, #3721). dropAll drops them in the order they were added, so
// a cache built from another cache is added after it (the /nodeinfo
// documents after the meta row: dropped the other way round, a request in
// between rebuilds the document from the stale row).
//
// The zero value is an empty list ready to use. Nothing in production calls
// dropAll: a maintenance rebuilds the whole generation (#3802), and the list
// is the base of the check that every cache of a generation is registered
// (#3777). The precise invalidations (the meta row on
// internal:metaUpdated, the emoji (name, host) entries, the role of one user)
// stay as they are; the list adds a way to drop everything, it replaces none
// of them.
//
// 本番では dropAll を呼ばない。段階 (6) で、メンテナンスの作り直しの代わりにキャッシュだけを
// 捨てる形を検討する予定だったが、作り直しが短いので範囲を狭めないと決めた (#3802、
// docs/architecture.md 4.4)。
type cacheList struct {
	parts []cachePart
	// err is the first invalid registration. newServer fails the
	// construction with it (constructionError).
	err error
}

// errCachePart is wrapped by every registration error of cacheList.
var errCachePart = errors.New("server: invalid cache")

// add appends the cache c named name, whose values come from origin.
//
// 不正な登録 (名前が空、由来が不明、値が nil、名前の重複、同じ値の 2 回目) は
// 構築の失敗にする。lifecycle と同じく、その場で panic しない。不正な登録は
// 一覧に入れない (捨てる手順を持たない部品と違い、入れなくても後始末は要らない)。
func (l *cacheList) add(name string, origin cacheOrigin, c localCache) {
	switch {
	case name == "":
		l.fail(fmt.Errorf("%w: a cache has no name", errCachePart))
	case !origin.valid():
		l.fail(fmt.Errorf("%w: cache %q has an unknown origin %q", errCachePart, name, origin))
	case isNilCache(c):
		l.fail(fmt.Errorf("%w: cache %q is nil", errCachePart, name))
	case l.hasName(name):
		l.fail(fmt.Errorf("%w: cache %q is added twice", errCachePart, name))
	case l.hasValue(c):
		l.fail(fmt.Errorf("%w: cache %q is a value already added under another name", errCachePart, name))
	default:
		l.parts = append(l.parts, cachePart{name: name, origin: origin, cache: c})
	}
}

// isNilCache reports whether c is nil or holds a nil of a kind that can be
// nil (a typed nil, which a plain c == nil does not catch). dropAll would
// panic on such a value.
func isNilCache(c localCache) bool {
	if c == nil {
		return true
	}
	v := reflect.ValueOf(c)
	switch v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice, reflect.UnsafePointer:
		return v.IsNil()
	}
	return false
}

func (l *cacheList) fail(err error) {
	if l.err == nil {
		l.err = err
	}
}

func (l *cacheList) hasName(name string) bool {
	for _, p := range l.parts {
		if p.name == name {
			return true
		}
	}
	return false
}

// hasValue reports whether c was already added. Only comparable values are
// compared (every cache is a pointer today): == panics when both hold the
// same dynamic type and the values are not comparable. The value is checked,
// not the type, since a struct of a comparable type can hold a slice in an
// interface field.
func (l *cacheList) hasValue(c localCache) bool {
	if !reflect.ValueOf(c).Comparable() {
		return false
	}
	for _, p := range l.parts {
		if p.cache == c {
			return true
		}
	}
	return false
}

// names returns the names of the caches in drop order.
func (l *cacheList) names() []string {
	names := make([]string, len(l.parts))
	for i, p := range l.parts {
		names[i] = p.name
	}
	return names
}

// dropAll drops every cache of the list in this process, in list order.
func (l *cacheList) dropAll() {
	for _, p := range l.parts {
		p.cache.DropLocal()
	}
}

// addCache adds the cache c named name, whose values come from origin, to the
// cache list of the generation (#3721, docs/architecture.md 4.4).
func (s *Server) addCache(name string, origin cacheOrigin, c localCache) {
	s.caches.add(name, origin, c)
}
