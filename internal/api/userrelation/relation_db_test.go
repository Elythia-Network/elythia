package userrelation

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/elythia-network/elythia/internal/entity"
	"github.com/elythia-network/elythia/internal/model"
	"github.com/elythia-network/elythia/internal/repository"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// relationCombo is one viewer->target state. Each field is an index into the
// states listed in relationDims.
type relationCombo struct {
	follow      int // 0 none, 1 notify NULL, 2 notify normal + withReplies, 3 notify none
	followed    int // 0/1
	blocking    int // 0/1
	blocked     int // 0/1
	mute        int // 0 none, 1 no expiry, 2 expires later, 3 expired
	renoteMute  int // 0/1
	requestFrom int // 0/1: viewer -> target follow request
	requestTo   int // 0/1: target -> viewer follow request
	memo        int // 0 none, 1 empty text, 2 text
	followedMsg int // 0 profile has no followedMessage, 1 has one
}

// relationDims is the number of states of each relationCombo field, in field
// order.
var relationDims = []int{4, 2, 2, 2, 4, 2, 2, 2, 3, 2}

func comboFromDigits(d []int) relationCombo {
	return relationCombo{d[0], d[1], d[2], d[3], d[4], d[5], d[6], d[7], d[8], d[9]}
}

// relationCombos returns: no relation, every non-zero state of every field
// alone, everything at once (with each mute / follow / memo state), and a
// fixed pseudo-random sample of mixed states.
func relationCombos() []relationCombo {
	var out []relationCombo
	zero := make([]int, len(relationDims))
	out = append(out, comboFromDigits(zero))
	for i, n := range relationDims {
		for v := 1; v < n; v++ {
			d := make([]int, len(relationDims))
			d[i] = v
			out = append(out, comboFromDigits(d))
		}
	}
	for v := 0; v < 4; v++ {
		d := make([]int, len(relationDims))
		for i, n := range relationDims {
			d[i] = n - 1
		}
		d[0] = v
		d[4] = v
		d[8] = v % 3
		out = append(out, comboFromDigits(d))
	}
	rng := rand.New(rand.NewSource(3608))
	for k := 0; k < 64; k++ {
		d := make([]int, len(relationDims))
		for i, n := range relationDims {
			d[i] = rng.Intn(n)
		}
		out = append(out, comboFromDigits(d))
	}
	return out
}

// seedCombo creates the target user and the rows of c, and returns the target
// and its profile.
func seedCombo(t *testing.T, db *gorm.DB, viewerID, targetID string, c relationCombo) (*model.User, *model.UserProfile) {
	t.Helper()
	u := &model.User{
		ID: targetID, Username: targetID, UsernameLower: strings.ToLower(targetID),
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}
	require.NoError(t, db.Create(u).Error)
	n := 0
	rid := func() string { n++; return fmt.Sprintf("%sx%d", targetID, n) }
	create := func(row any) { require.NoError(t, db.Create(row).Error) }

	normal, none := "normal", "none"
	switch c.follow {
	case 1:
		create(&model.Following{ID: rid(), FollowerID: viewerID, FolloweeID: targetID})
	case 2:
		create(&model.Following{ID: rid(), FollowerID: viewerID, FolloweeID: targetID, Notify: &normal, WithReplies: true})
	case 3:
		create(&model.Following{ID: rid(), FollowerID: viewerID, FolloweeID: targetID, Notify: &none})
	}
	if c.followed == 1 {
		create(&model.Following{ID: rid(), FollowerID: targetID, FolloweeID: viewerID, Notify: &normal, WithReplies: true})
	}
	if c.blocking == 1 {
		create(&model.Blocking{ID: rid(), BlockerID: viewerID, BlockeeID: targetID})
	}
	if c.blocked == 1 {
		create(&model.Blocking{ID: rid(), BlockerID: targetID, BlockeeID: viewerID})
	}
	later, earlier := time.Now().Add(time.Hour), time.Now().Add(-time.Hour)
	switch c.mute {
	case 1:
		create(&model.Muting{ID: rid(), MuterID: viewerID, MuteeID: targetID})
	case 2:
		create(&model.Muting{ID: rid(), MuterID: viewerID, MuteeID: targetID, ExpiresAt: &later})
	case 3:
		create(&model.Muting{ID: rid(), MuterID: viewerID, MuteeID: targetID, ExpiresAt: &earlier})
	}
	if c.renoteMute == 1 {
		create(&model.RenoteMuting{ID: rid(), MuterID: viewerID, MuteeID: targetID})
	}
	if c.requestFrom == 1 {
		create(&model.FollowRequest{ID: rid(), FollowerID: viewerID, FolloweeID: targetID})
	}
	if c.requestTo == 1 {
		create(&model.FollowRequest{ID: rid(), FollowerID: targetID, FolloweeID: viewerID})
	}
	switch c.memo {
	case 1:
		create(&model.UserMemo{ID: rid(), UserID: viewerID, TargetUserID: targetID, Memo: ""})
	case 2:
		create(&model.UserMemo{ID: rid(), UserID: viewerID, TargetUserID: targetID, Memo: "memo of " + targetID})
	}
	// 相手から自分へのメモと、相手から自分へのミュート・リノートミュートは
	// 自分から見た関係に出てはいけない (向きを取り違えると拾う)。
	create(&model.UserMemo{ID: rid(), UserID: targetID, TargetUserID: viewerID, Memo: "reverse"})
	create(&model.Muting{ID: rid(), MuterID: targetID, MuteeID: viewerID})
	create(&model.RenoteMuting{ID: rid(), MuterID: targetID, MuteeID: viewerID})

	p := &model.UserProfile{UserID: targetID}
	if c.followedMsg == 1 {
		msg := "thanks " + targetID
		p.FollowedMessage = &msg
	}
	return u, p
}

// expectedRelationJSON is the relation block c must produce, written out from
// the rules of upstream UserEntityService.pack (not from either code path).
func expectedRelationJSON(c relationCombo, targetID string) map[string]any {
	following := c.follow != 0
	notify := "none"
	if c.follow == 2 {
		notify = "normal"
	}
	want := map[string]any{
		"isFollowing":                    following,
		"isFollowed":                     c.followed == 1,
		"isBlocking":                     c.blocking == 1,
		"isBlocked":                      c.blocked == 1,
		"isMuted":                        c.mute == 1 || c.mute == 2,
		"isRenoteMuted":                  c.renoteMute == 1,
		"hasPendingFollowRequestFromYou": c.requestFrom == 1,
		"hasPendingFollowRequestToYou":   c.requestTo == 1,
		"notify":                         notify,
		"withReplies":                    c.follow == 2,
		"memo":                           nil,
	}
	switch c.memo {
	case 1:
		want["memo"] = ""
	case 2:
		want["memo"] = "memo of " + targetID
	}
	if following {
		if c.followedMsg == 1 {
			want["followedMessage"] = "thanks " + targetID
		} else {
			want["followedMessage"] = nil
		}
	}
	return want
}

var relationJSONKeys = []string{
	"isFollowing", "isFollowed", "isBlocking", "isBlocked", "isMuted", "isRenoteMuted",
	"hasPendingFollowRequestFromYou", "hasPendingFollowRequestToYou",
	"notify", "withReplies", "memo", "followedMessage",
}

// Apply with Relation wired reads the whole block in one statement and writes
// exactly what the per-relation queries write, for every combination of
// following (and its notify / withReplies), being followed, blocks in both
// directions, active / expiring / expired mutes, renote mutes, follow requests
// in both directions, memos and followedMessage.
func TestApply_SingleStatementMatchesEveryCombination(t *testing.T) {
	base := openDB(t)
	prefix := "ura1s"
	viewerID := prefix + "v"
	require.NoError(t, base.Create(&model.User{
		ID: viewerID, Username: viewerID, UsernameLower: viewerID,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	t.Cleanup(func() {
		for _, table := range []string{"following", "blocking", "muting", "renote_muting", "follow_request", "user_memo", "user"} {
			base.Exec(`DELETE FROM "`+table+`" WHERE id LIKE ?`, prefix+"%")
		}
	})

	db, n := countingDB(t, base)
	each := reposOn(db)
	single := reposOn(db)
	single.Relation = repository.NewUserRelationRepository(db)

	for i, c := range relationCombos() {
		targetID := fmt.Sprintf("%st%03d", prefix, i)
		target, profile := seedCombo(t, base, viewerID, targetID, c)

		var want, got entity.UserDetailed
		wantFollowing := each.Apply(&want, viewerID, target, profile)
		n.Store(0)
		gotFollowing := single.Apply(&got, viewerID, target, profile)
		require.Equal(t, int64(1), n.Load(), "combo %+v: one statement", c)

		assert.Equal(t, wantFollowing, gotFollowing, "combo %+v", c)
		assert.Equal(t, c.follow != 0, gotFollowing, "combo %+v", c)
		wj, _ := json.Marshal(want)
		gj, _ := json.Marshal(got)
		require.JSONEq(t, string(wj), string(gj), "combo %+v", c)

		var gotMap map[string]any
		require.NoError(t, json.Unmarshal(gj, &gotMap))
		wantRel := expectedRelationJSON(c, targetID)
		for _, k := range relationJSONKeys {
			wv, wok := wantRel[k]
			gv, gok := gotMap[k]
			require.Equal(t, wok, gok, "combo %+v: presence of %s", c, k)
			require.Equal(t, wv, gv, "combo %+v: %s", c, k)
		}
	}
}

// The self view and the anonymous view stay as before: self gets only the
// memo, anonymous gets nothing, and neither reads the relation statement.
func TestApply_SingleStatementSelfAndAnonymous(t *testing.T) {
	base := openDB(t)
	prefix := "ura2s"
	viewerID := prefix + "v"
	require.NoError(t, base.Create(&model.User{
		ID: viewerID, Username: viewerID, UsernameLower: viewerID,
		AvatarDecorations: datatypes.JSON([]byte("[]")),
	}).Error)
	require.NoError(t, base.Create(&model.UserMemo{ID: prefix + "m", UserID: viewerID, TargetUserID: viewerID, Memo: "self"}).Error)
	t.Cleanup(func() {
		base.Exec(`DELETE FROM "user_memo" WHERE id LIKE ?`, prefix+"%")
		base.Exec(`DELETE FROM "user" WHERE id LIKE ?`, prefix+"%")
	})
	r := reposOn(base)
	r.Relation = repository.NewUserRelationRepository(base)
	viewer := &model.User{ID: viewerID}

	var self entity.UserDetailed
	assert.False(t, r.Apply(&self, viewerID, viewer, &model.UserProfile{UserID: viewerID}))
	assert.Nil(t, self.IsFollowing)
	assert.Nil(t, self.Notify)
	require.NotNil(t, self.Memo)
	assert.Equal(t, "self", *self.Memo)

	var anon entity.UserDetailed
	assert.False(t, r.Apply(&anon, "", viewer, nil))
	assert.Nil(t, anon.IsFollowing)
	assert.Nil(t, anon.Memo)
}

// An id with NUL can match no row, so the reader answers "no relation"
// without sending a statement that PostgreSQL would reject.
func TestReadUserRelation_UnstorableIDIsNoRelation(t *testing.T) {
	db, n := countingDB(t, openDB(t))
	rr := repository.NewUserRelationRepository(db)
	s, err := rr.ReadUserRelation("a\x00b", "c", time.Now())
	require.NoError(t, err)
	assert.Equal(t, repository.UserRelationState{}, s)
	s, err = rr.ReadUserRelation("c", "a\x00b", time.Now())
	require.NoError(t, err)
	assert.Equal(t, repository.UserRelationState{}, s)
	assert.Equal(t, int64(0), n.Load())
}
