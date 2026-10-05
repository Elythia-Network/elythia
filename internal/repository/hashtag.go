package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// HashtagRepository は `hashtag` テーブルへのアクセスを提供する (#680)。
//
// Misskey TS の HashtagService.updateHashtag 相当を mk-go に移植するための
// repository 層。RecordMention / RecordAttach は冪等で、同じ user を二度
// 渡してもカウンタは増えない (mentionedUserIds 配列で dedup する)。
type HashtagRepository interface {
	// FindByName は name で 1 行引く。見つからなければ gorm.ErrRecordNotFound。
	FindByName(name string) (*model.Hashtag, error)

	// RecordMention はユーザー `userID` がハッシュタグ `name` を **言及した**
	// 事実を記録する。冪等: 同じ user を再度渡しても count は増えない。
	// `isLocal` は user.Host == nil で判定して呼び出し側が渡す (このメソッド
	// 自体は user 行を fetch しない)。
	//
	// row が存在しなければ INSERT して count=1 で初期化する (id は idGen で
	// 採番)。既存 row があれば mentionedUserIds に append して count を 1
	// 増やす (NOT ANY ガードで重複防止)。
	RecordMention(id, name, userID string, isLocal bool) error

	// RecordAttach は user の profile に hashtag を attached した記録を残す。
	// semantics は RecordMention と同じだが対象列が attached* 系。
	RecordAttach(id, name, userID string, isLocal bool) error

	// RecordDetach は RecordAttach の逆。user `userID` が hashtag `name` を
	// profile から外した際に attachedUserIds から除去し count を減算する。
	// 該当 user が list に居ない場合は no-op (冪等)。upstream の
	// updateHashtag(inc=false) 相当。
	RecordDetach(name, userID string, isLocal bool) error
}

type hashtagRepository struct {
	db *gorm.DB
}

// NewHashtagRepository creates a new HashtagRepository.
func NewHashtagRepository(db *gorm.DB) HashtagRepository {
	return &hashtagRepository{db: db}
}

func (r *hashtagRepository) FindByName(name string) (*model.Hashtag, error) {
	if !storable(name) {
		return nil, ErrNotFound
	}
	var h model.Hashtag
	if err := r.db.Where("name = ?", name).First(&h).Error; err != nil {
		return nil, err
	}
	return &h, nil
}

// RecordMention は 2 段の SQL で冪等な「行確保 → dedup-add」を実装する。
//
// 段階 1: INSERT ON CONFLICT DO NOTHING で「同名 hashtag 行が必ず 1 つ存在する」
// 状態を作る。並行 2 リクエストがあっても name UNIQUE 前提で 1 行に収束する。
// なお、新規 INSERT 時は count=0 / userIds=空 で挿入する (count 更新は段階 2 が
// 担当する。INSERT 時に count=1 で挿入してしまうと、既に存在した行に対する
// 並行 INSERT が DO NOTHING で skip された後で UPDATE が走ると、後続の dedup
// guard が効いて二重 count を防げてしまうので、count は常に「step 2 で計算」
// する単一系統に集約する)。
//
// 段階 2: UPDATE ... WHERE NOT (? = ANY(...)) で「まだ list に居なければ
// append + count++」を atomic に行う。WHERE ガードのおかげで多重 RecordMention
// が走っても結果は idempotent (count は user 種類数だけ増える)。
//
// `local` / `remote` 列は isLocal フラグで分岐。total 列は常に更新する。
func (r *hashtagRepository) RecordMention(id, name, userID string, isLocal bool) error {
	return r.recordImpl(id, name, userID, isLocal,
		"mentionedUserIds", "mentionedUsersCount",
		"mentionedLocalUserIds", "mentionedLocalUsersCount",
		"mentionedRemoteUserIds", "mentionedRemoteUsersCount",
	)
}

func (r *hashtagRepository) RecordAttach(id, name, userID string, isLocal bool) error {
	return r.recordImpl(id, name, userID, isLocal,
		"attachedUserIds", "attachedUsersCount",
		"attachedLocalUserIds", "attachedLocalUsersCount",
		"attachedRemoteUserIds", "attachedRemoteUsersCount",
	)
}

// RecordDetach removes userID from a hashtag's attached* arrays and decrements
// the counts. It is the inverse of RecordAttach and is idempotent: if the user
// is not present (or the row does not exist), it is a no-op.
func (r *hashtagRepository) RecordDetach(name, userID string, isLocal bool) error {
	subIDsCol, subCountCol := "attachedRemoteUserIds", "attachedRemoteUsersCount"
	if isLocal {
		subIDsCol, subCountCol = "attachedLocalUserIds", "attachedLocalUsersCount"
	}
	// total / sub を 1 UPDATE で減算する。WHERE ガードで「list に居る時だけ」
	// 実行するので多重 detach は冪等。column 名は固定 const のみで injection 安全。
	query := `
UPDATE "hashtag" SET
  "attachedUserIds"   = array_remove("attachedUserIds", ?),
  "attachedUsersCount" = "attachedUsersCount" - 1,
  "` + subIDsCol + `"   = array_remove("` + subIDsCol + `", ?),
  "` + subCountCol + `" = "` + subCountCol + `" - 1
WHERE name = ? AND (? = ANY("attachedUserIds"))`
	return r.db.Exec(query, userID, userID, name, userID).Error
}

// recordImpl は RecordMention / RecordAttach 共通実装。column 名だけ差し替え。
func (r *hashtagRepository) recordImpl(
	id, name, userID string, isLocal bool,
	totalIDsCol, totalCountCol,
	localIDsCol, localCountCol,
	remoteIDsCol, remoteCountCol string,
) error {
	return r.db.Transaction(func(tx *gorm.DB) error {
		// 段階 1: 行確保。
		if err := tx.Exec(
			`INSERT INTO "hashtag" (id, name) VALUES (?, ?) ON CONFLICT (name) DO NOTHING`,
			id, name,
		).Error; err != nil {
			return err
		}
		// 段階 1.5: #2106 L55: 同一 (name, userID) への concurrent record (連投 / inbox 同時処理)
		// で array_append + count++ が二重発行され、attachedUserIds に重複が入り count が膨らむ
		// race を防ぐため、対象 hashtag 行を FOR UPDATE で直列化する。2 つ目の tx は 1 つ目の
		// commit を待ってから NOT (...=ANY) を再評価する (append 済の userID を見て skip)。
		if err := tx.Exec(`SELECT 1 FROM "hashtag" WHERE name = ? FOR UPDATE`, name).Error; err != nil {
			return err
		}
		// 段階 2: 該当 user がまだ list に居なければ append + count++。
		// total / (local|remote) を 1 度の UPDATE で更新する。
		// 文字列連結で column 名を組むのは固定 const のみなので injection 安全。
		var subIDsCol, subCountCol string
		if isLocal {
			subIDsCol, subCountCol = localIDsCol, localCountCol
		} else {
			subIDsCol, subCountCol = remoteIDsCol, remoteCountCol
		}
		query := `
UPDATE "hashtag" SET
  "` + totalIDsCol + `"   = array_append("` + totalIDsCol + `", ?),
  "` + totalCountCol + `" = "` + totalCountCol + `" + 1,
  "` + subIDsCol + `"     = array_append("` + subIDsCol + `", ?),
  "` + subCountCol + `"   = "` + subCountCol + `" + 1
WHERE name = ? AND NOT (? = ANY("` + totalIDsCol + `"))`
		return tx.Exec(query, userID, userID, name, userID).Error
	})
}
