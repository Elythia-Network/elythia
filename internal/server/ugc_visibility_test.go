package server

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/elythia-network/elythia/internal/testutil"
)

// stubMetaRepo returns a fixed Fetch result. testutil の mock は Meta が nil の
// ときだけ error を返す形なので、「error かつ非 nil」「nil かつ error なし」を
// 作り分けるために別途置く。
type stubMetaRepo struct {
	meta *model.Meta
	err  error
}

func (s stubMetaRepo) Fetch() (*model.Meta, error)   { return s.meta, s.err }
func (s stubMetaRepo) Update(map[string]any) error   { return nil }
func (s stubMetaRepo) EnsureInitial(id string) error { return nil }

// 匿名 visitor への露出 gate は起動時に 1 度だけ meta を読む。**読めなかった
// ときに空文字を返すと gate が丸ごと素通しになる** ("none" でも "local" でも
// ないため)。DB が起動時に一瞬応答しなかっただけで露出制限が消えるのは割に
// 合わないので、制限側の既定へ倒すことを固定する (#2708 review H-2)。
func TestMetaUGCVisibility(t *testing.T) {
	withValue := testutil.NewMockMetaRepository()
	withValue.Meta = &model.Meta{UgcVisibilityForVisitor: "none"}

	cases := []struct {
		name string
		repo repository.MetaRepository
		want string
	}{
		{"meta repo not wired", nil, defaultUGCVisibilityForVisitor},
		{"fetch fails", stubMetaRepo{err: errors.New("db is starting up")}, defaultUGCVisibilityForVisitor},
		{"fetch returns nil meta", stubMetaRepo{}, defaultUGCVisibilityForVisitor},
		// error と一緒に値が返ってきても信用しない。m == nil の判定だけだと
		// ここが素通りする。
		{"fetch fails but returns a value", stubMetaRepo{
			meta: &model.Meta{UgcVisibilityForVisitor: "all"},
			err:  errors.New("db is starting up"),
		}, defaultUGCVisibilityForVisitor},
		{"column is empty", stubMetaRepo{meta: &model.Meta{}}, defaultUGCVisibilityForVisitor},
		{"configured value wins", withValue, "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, metaUGCVisibility(tc.repo))
		})
	}
}

// 既定は fail-open ("") ではなく制限側であること。値そのものは
// migration/000029 の列既定と揃えてある。
func TestDefaultUGCVisibilityForVisitor(t *testing.T) {
	assert.Equal(t, "local", defaultUGCVisibilityForVisitor,
		"匿名 visitor への露出既定が緩い側へ倒れている (#2708 review H-2)")
}
