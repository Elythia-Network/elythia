package emojiapplication

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/elythia-network/elythia/internal/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 申請経路が列に入らない値を弾く (#3022)。
//
// 直している失敗形は「**利用者の入力で 5xx が立つ**」。`emoji_application` の列に
// 入らない値を渡すと、長さ超過は SQLSTATE 22001、NUL は 22021 (本番の pgx
// extended protocol。テストハーネスは `PreferSimpleProtocol: true` なので 08P01) で
// 落ち、**同じ書き込みに乗っている他の列まで巻き添えになる**。
// `admin/emoji/*` は #3018 で同じ規則に揃えてある。

func runesOf(n int, r string) string { return strings.Repeat(r, n) }

// 列に入らない本文は ErrTooLong。**NUL は長さと同じ述語で見る** (`colfit.Fits`)。
func TestCreateRejectsUnstorableFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateInput)
		want   error
	}{
		{"license が 1 文字超過", func(in *CreateInput) {
			in.License = runesOf(applicationLicenseMaxRunes+1, "あ")
		}, ErrTooLong},
		{"license に NUL", func(in *CreateInput) { in.License = "a\x00b" }, ErrTooLong},
		{"category が 1 文字超過", func(in *CreateInput) {
			in.Category = runesOf(applicationCategoryMaxRunes+1, "あ")
		}, ErrTooLong},
		{"category に NUL", func(in *CreateInput) { in.Category = "a\x00b" }, ErrTooLong},
		{"comment が 1 文字超過", func(in *CreateInput) {
			in.Comment = runesOf(applicationCommentMaxRunes+1, "あ")
		}, ErrTooLong},
		{"comment に NUL", func(in *CreateInput) { in.Comment = "a\x00b" }, ErrTooLong},
		// **name の NUL は pattern が弾く** (`^[a-zA-Z0-9_]+$`)。pattern を緩めた
		// ときに穴が開かないよう、ここで固定しておく。
		{"name に NUL", func(in *CreateInput) { in.Name = "su\x00shi" }, ErrInvalidName},
		{"name が 1 文字超過", func(in *CreateInput) {
			in.Name = strings.Repeat("a", applicationNameMaxRunes+1)
		}, ErrInvalidName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			tc.mutate(&in)
			_, err := newService(t, newFakeApps(), &fakeEmojis{}, nil).Create(in)
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// リモート申請の `remoteHost` / `remoteName` も見る。**書き込みより前に
// `checkRemoteEmoji` が SELECT で使う**ので、NUL があると照会の時点で落ちる。
func TestCreateRemoteRejectsUnstorableFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*CreateInput)
	}{
		{"host が 1 文字超過", func(in *CreateInput) {
			in.RemoteHost = runesOf(applicationRemoteHostMaxRunes+1, "あ")
		}},
		{"host に NUL", func(in *CreateInput) { in.RemoteHost = "a\x00b" }},
		{"name が 1 文字超過", func(in *CreateInput) {
			in.RemoteName = runesOf(applicationRemoteNameMaxRunes+1, "あ")
		}},
		{"name に NUL", func(in *CreateInput) { in.RemoteName = "a\x00b" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := remoteInput()
			tc.mutate(&in)
			emojis := &fakeEmojis{remote: &model.Emoji{ID: "e-remote", Name: "sushi_remote"}}
			_, err := newService(t, newFakeApps(), emojis, nil).Create(in)
			require.ErrorIs(t, err, ErrTooLong)
		})
	}
}

// 上限ちょうどは通し、値を切らずに保存する。**全角で作る** — byte で数える実装なら
// 「ちょうど」のケースが落ちる (varchar はコードポイントで数える)。
//
// **前後に空白を付けた形も通す。** 保存されるのは trim 後の値なので、生の値で
// 判定すると「上限ちょうど + 末尾の改行」が弾かれる (`Reject` と同じ不変条件)。
func TestCreateAcceptsFieldsAtLimit(t *testing.T) {
	license := runesOf(applicationLicenseMaxRunes, "い")
	category := runesOf(applicationCategoryMaxRunes, "あ")
	comment := runesOf(applicationCommentMaxRunes, "う")
	name := strings.Repeat("a", applicationNameMaxRunes)
	alias := runesOf(applicationAliasMaxRunes, "え")

	for _, tc := range []struct {
		name string
		pad  string
	}{
		{"ちょうど", ""},
		{"前後に空白", "\n  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := validInput()
			in.License = tc.pad + license + tc.pad
			in.Category = tc.pad + category + tc.pad
			in.Comment = tc.pad + comment + tc.pad
			in.Name = tc.pad + name + tc.pad
			in.Aliases = []string{tc.pad + alias + tc.pad}

			app, err := newService(t, newFakeApps(), &fakeEmojis{}, nil).Create(in)
			require.NoError(t, err)
			assert.Equal(t, license, app.License)
			require.NotNil(t, app.Category)
			assert.Equal(t, category, *app.Category)
			require.NotNil(t, app.Comment)
			assert.Equal(t, comment, *app.Comment)
			assert.Equal(t, name, app.Name)
			assert.Equal(t, []string{alias}, []string(app.Aliases),
				"trim の前に長さを見ると、上限ちょうどの alias が黙って落ちる")
		})
	}
}

// alias は**要素ごとに落とす**。1 つが長すぎる / NUL を含むだけで他まで捨てない。
func TestCreateDropsUnstorableAliases(t *testing.T) {
	in := validInput()
	in.Aliases = []string{
		"ok",
		runesOf(applicationAliasMaxRunes+1, "あ"),
		"a\x00b",
		runesOf(applicationAliasMaxRunes, "い"),
	}

	app, err := newService(t, newFakeApps(), &fakeEmojis{}, nil).Create(in)
	require.NoError(t, err)
	assert.Equal(t, []string{"ok", runesOf(applicationAliasMaxRunes, "い")}, []string(app.Aliases),
		"NUL を含む要素は切らずに落とす (切ると別の名前になる)")
}

// 却下理由も列に入るかを見る (#3022)。ここは長さも NUL も無検証だった。
func TestRejectRejectsUnstorableReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"1 文字超過", runesOf(applicationRejectReasonMaxRunes+1, "あ")},
		{"NUL", "a\x00b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := newFakeApps()
			app := &model.EmojiApplication{
				ID: "a1", UserID: "u1", Kind: model.EmojiApplicationKindOwn,
				Status: model.EmojiApplicationPending, Name: "sushi",
			}
			apps.rows[app.ID] = app

			_, err := newService(t, apps, &fakeEmojis{}, nil).
				Reject(context.Background(), "a1", "mod1", tc.reason)
			require.ErrorIs(t, err, ErrTooLong)
			// **却下を保存しない。** 500 で落ちると押した却下が消えるのと同じ。
			assert.Equal(t, model.EmojiApplicationPending, apps.rows["a1"].Status)
		})
	}
}

// 上限ちょうどの却下理由は通る。**空白を付けても通る** — 保存されるのは
// `optionalString` が trim した値なので、生の値で判定すると末尾の改行だけで
// 上限ちょうどの理由が弾かれる。
func TestRejectAcceptsReasonAtLimit(t *testing.T) {
	reason := runesOf(applicationRejectReasonMaxRunes, "あ")
	for _, tc := range []struct {
		name string
		sent string
	}{
		{"ちょうど", reason},
		{"末尾に空白", reason + "\n  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			apps := newFakeApps()
			apps.rows["a1"] = &model.EmojiApplication{
				ID: "a1", UserID: "u1", Kind: model.EmojiApplicationKindOwn,
				Status: model.EmojiApplicationPending, Name: "sushi",
			}

			app, err := newService(t, apps, &fakeEmojis{}, nil).
				Reject(context.Background(), "a1", "mod1", tc.sent)
			require.NoError(t, err)
			require.NotNil(t, app.RejectReason)
			assert.Equal(t, reason, *app.RejectReason, "保存されるのは trim 後の値")
		})
	}
}

// 枠のリセット理由も NUL を通さない (長さは元から見ている)。
func TestResetQuotaRejectsUnstorableReason(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reason string
	}{
		{"1 文字超過", runesOf(quotaResetReasonMaxLen+1, "あ")},
		{"NUL", "a\x00b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resets := &fakeResets{}
			svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
			svc.SetQuotaResetRepo(resets)

			_, _, err := svc.ResetQuota("u1", "mod1", tc.reason)
			require.ErrorIs(t, err, ErrTooLong)
			assert.Empty(t, resets.created, "弾いたのにリセットを書いている")
		})
	}
}

// 上限ちょうどのリセット理由は通る。
func TestResetQuotaAcceptsReasonAtLimit(t *testing.T) {
	resets := &fakeResets{}
	svc := NewService(newFakeApps(), &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil)
	svc.SetQuotaResetRepo(resets)

	_, row, err := svc.ResetQuota("u1", "mod1", runesOf(quotaResetReasonMaxLen, "あ"))
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, quotaResetReasonMaxLen, len([]rune(row.Reason)))
}

// 上限の値そのものを固定する (#3022)。**at-limit のテストは定数を使って入力を
// 作る**ので、定数を小さくする変異は入力まで一緒に縮んで素通りする (実測)。
// DDL 側の実値との突き合わせは
// `internal/repository/emoji_application_column_limits_test.go`。
func TestApplicationColumnLimitConstants(t *testing.T) {
	assert.Equal(t, 128, applicationNameMaxRunes, "emoji_application.name は varchar(128)")
	assert.Equal(t, 128, applicationCategoryMaxRunes, "emoji_application.category は varchar(128)")
	assert.Equal(t, 128, applicationAliasMaxRunes, "emoji_application.aliases は varchar(128)[]")
	assert.Equal(t, 1024, applicationLicenseMaxRunes, "emoji_application.license は varchar(1024)")
	assert.Equal(t, 2048, applicationCommentMaxRunes, "emoji_application.comment は varchar(2048)")
	assert.Equal(t, 128, applicationRemoteHostMaxRunes, "emoji_application.remoteHost は varchar(128)")
	assert.Equal(t, 128, applicationRemoteNameMaxRunes, "emoji_application.remoteName は varchar(128)")
	assert.Equal(t, 2048, applicationRejectReasonMaxRunes, "emoji_application.rejectReason は varchar(2048)")
	assert.Equal(t, 32, applicationFileIDMaxRunes, "emoji_application.fileId は varchar(32)")
	assert.Equal(t, 32, applicationIDMaxRunes, "emoji_application.id は varchar(32)")
	assert.Equal(t, 1024, quotaResetReasonMaxLen, "emoji_application_quota_reset.reason は varchar(1024)")
}

// `fileId` も列と照会の前に見る (#3022 のレビュー H1)。**NUL を含む id は
// `FindByID` がその時点で落ち、`IsNotFound` でもないので 500 になっていた。**
func TestCreateRejectsUnstorableFileID(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fileID string
	}{
		{"NUL", "f\x001"},
		{"1 文字超過", strings.Repeat("a", applicationFileIDMaxRunes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &countingFiles{}
			svc := NewService(newFakeApps(), &fakeEmojis{}, files, &fixedID{}, nil, nil)
			in := validInput()
			in.FileID = tc.fileID

			_, err := svc.Create(in)
			require.ErrorIs(t, err, ErrFileGone)
			// **引く前に弾く。** 引いてしまうと、その SELECT が落ちて 500 になる。
			assert.Zero(t, files.calls, "存在しえない id で drive を引いている")
		})
	}
}

// countingFiles records lookups so tests can assert the id never reaches the DB.
type countingFiles struct {
	okFiles
	calls int
}

func (f *countingFiles) FindByID(id string) (*model.DriveFile, error) {
	f.calls++
	return f.okFiles.FindByID(id)
}

// 申請 ID も引く前に弾く (#3022 のレビュー M1)。NUL を含む id は SELECT が
// その時点で落ち、`IsNotFound` でもないので 500 になっていた。
func TestApplicationIDNULIsNotFound(t *testing.T) {
	bad := "a\x00b"
	// **fake も実 DB と同じ壊れ方をする。** not-found を返す fake だと、guard を
	// 外しても同じ error になって検出できない (実測)。
	newApps := func() *countingApps { return &countingApps{fakeApps: newFakeApps(), nulErr: errNULLookup} }

	t.Run("cancel", func(t *testing.T) {
		apps := newApps()
		err := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).Cancel(bad, "u1")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("reject", func(t *testing.T) {
		apps := newApps()
		_, err := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).
			Reject(context.Background(), bad, "mod1", "理由")
		require.ErrorIs(t, err, ErrNotFound)
	})
	t.Run("approve", func(t *testing.T) {
		apps := newApps()
		_, err := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).
			Approve(context.Background(), bad, "mod1")
		require.ErrorIs(t, err, ErrNotFound)
	})
}

// errNULLookup mimics what PostgreSQL does with a NUL in a query parameter:
// **not-found ではない error** が返る (本番は SQLSTATE 22021)。
var errNULLookup = errors.New("invalid message format")

// 上限ちょうどの id は引きに行く (弾くのは列に入らないものだけ)。
// **not-found になること自体は判定材料にならない** — 引く前に弾いても同じ error に
// なるので、fake が id を受け取ったかで見る。
func TestApplicationIDAtLimitReachesLookup(t *testing.T) {
	apps := &countingApps{fakeApps: newFakeApps()}
	id := strings.Repeat("a", applicationIDMaxRunes)

	err := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).Cancel(id, "u1")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Equal(t, []string{id}, apps.findCalls, "上限ちょうどの id を引く前に弾いている")
}

// 列に入らない id は引きに行かない (引くと SELECT がそこで落ちる)。
func TestApplicationIDNULDoesNotReachLookup(t *testing.T) {
	apps := &countingApps{fakeApps: newFakeApps()}

	err := NewService(apps, &fakeEmojis{}, &okFiles{}, &fixedID{}, nil, nil).Cancel("a\x00b", "u1")
	require.ErrorIs(t, err, ErrNotFound)
	assert.Empty(t, apps.findCalls, "存在しえない id で申請を引いている")
}

// countingApps records the ids handed to FindByID and can fail like the DB does
// for a NUL-containing id.
type countingApps struct {
	*fakeApps
	findCalls []string
	nulErr    error
}

func (a *countingApps) FindByID(id string) (*model.EmojiApplication, error) {
	a.findCalls = append(a.findCalls, id)
	if a.nulErr != nil && strings.ContainsRune(id, 0) {
		return nil, a.nulErr
	}
	return a.fakeApps.FindByID(id)
}
