package entity

import "github.com/shiroha-a/mk/internal/model"

// FillUserLites resolves UserLite.instance and UserLite.emojis of lites[i]
// from users[i] with batched lookups, mirroring the per-user parts of upstream
// UserEntityService.packMany. users and lites must have the same length; nil
// entries are skipped. A nil lookup leaves that part out.
//
// 本家 pack は利用者ごとに federatedInstanceCache と populateEmojis を引く
// (どちらもキャッシュ付き)。mk-go はキャッシュを持たないので、一覧では
// instance を 1 回、絵文字をホストごとに 1 回でまとめて引き、利用者の数に
// 比例して問い合わせが増えないようにする。
//
// 対を struct にせず 2 本の slice で受けるのは、entity に model.User を持つ型を
// 置くと公開 shape の IP ゲート (entitycompat) が「入れ子で IP を出しうる型」と
// して拾うため。
func FillUserLites(instances InstanceLookup, emojis EmojiLookup, users []*model.User, lites []*UserLite) {
	if len(users) == 0 || len(users) != len(lites) {
		return
	}
	present := make([]*model.User, 0, len(users))
	carriers := make([]*model.Note, 0, len(users))
	for i, u := range users {
		if u == nil || lites[i] == nil {
			continue
		}
		present = append(present, u)
		// EmojiResolver は note の作者の絵文字を集めるので、利用者だけを持つ
		// note を渡す (PackNotifications と同じやり方)。
		carriers = append(carriers, &model.Note{User: u})
	}
	if len(present) == 0 {
		return
	}
	instResolver := NewInstanceResolver(instances, present...)
	emojiResolver := NewEmojiResolver(emojis, carriers)
	for i, u := range users {
		if u == nil || lites[i] == nil {
			continue
		}
		instResolver.FillUserLite(lites[i])
		emojiResolver.PopulateUserEmojis(u, lites[i])
	}
}
