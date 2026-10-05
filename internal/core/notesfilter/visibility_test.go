package notesfilter_test

import (
	"testing"

	"github.com/elythia-network/elythia/internal/core/notesfilter"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/testutil"
	"github.com/stretchr/testify/assert"
)

func TestFilterVisible(t *testing.T) {
	pub := &model.Note{ID: "pub", UserID: "author", Visibility: model.NoteVisibilityPublic}
	fol := &model.Note{ID: "fol", UserID: "author", Visibility: model.NoteVisibilityFollowers}
	spec := &model.Note{ID: "spec", UserID: "author", Visibility: model.NoteVisibilitySpecified, VisibleUserIDs: []string{"allowed"}}
	notes := []*model.Note{pub, fol, spec}

	idsOf := func(rows []*model.Note) []string {
		out := make([]string, 0, len(rows))
		for _, n := range rows {
			out = append(out, n.ID)
		}
		return out
	}

	// 空入力はそのまま返す。
	assert.Empty(t, notesfilter.FilterVisible(nil, nil, nil))

	// anonymous viewer は public のみ。followingRepo nil でも fail-closed。
	assert.Equal(t, []string{"pub"}, idsOf(notesfilter.FilterVisible(nil, notes, nil)))

	// follower は public + followers。
	fRepo := testutil.NewMockFollowingRepository()
	fRepo.Followings["f1"] = &model.Following{ID: "f1", FollowerID: "follower", FolloweeID: "author"}
	assert.Equal(t, []string{"pub", "fol"}, idsOf(notesfilter.FilterVisible(&model.User{ID: "follower"}, notes, fRepo)))

	// specified の対象 viewer は public + specified。
	assert.Equal(t, []string{"pub", "spec"}, idsOf(notesfilter.FilterVisible(&model.User{ID: "allowed"}, notes, fRepo)))

	// author 本人は全 visibility を閲覧可。
	assert.Equal(t, []string{"pub", "fol", "spec"}, idsOf(notesfilter.FilterVisible(&model.User{ID: "author"}, notes, fRepo)))
}
