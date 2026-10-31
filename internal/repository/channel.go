package repository

import (
	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
)

// ChannelRepository provides data access for the `channel` table.
type ChannelRepository interface {
	Create(c *model.Channel) error
	FindByID(id string) (*model.Channel, error)
	// FindByIDs returns channels matching the given IDs.
	FindByIDs(ids []string) ([]*model.Channel, error)
	UpdateFields(channelID string, fields map[string]any) error
	IncrementCount(channelID, column string, delta int) error
	// IncrementUsersCountOnFirstPost adds 1 to usersCount when noteID is the
	// author's earliest note in the channel, i.e. the author's first post there.
	IncrementUsersCountOnFirstPost(channelID, authorID, noteID string) error
	List(filter model.ChannelListFilter) ([]*model.Channel, error)
}

type channelRepository struct {
	db *gorm.DB
}

// NewChannelRepository creates a new ChannelRepository.
func NewChannelRepository(db *gorm.DB) ChannelRepository {
	return &channelRepository{db: db}
}

func (r *channelRepository) Create(c *model.Channel) error {
	return r.db.Create(c).Error
}

func (r *channelRepository) FindByID(id string) (*model.Channel, error) {
	if !storable(id) {
		return nil, ErrNotFound
	}
	var c model.Channel
	if err := r.db.First(&c, "id = ?", id).Error; err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *channelRepository) FindByIDs(ids []string) ([]*model.Channel, error) {
	ids = storableIDs(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	var rows []*model.Channel
	if err := r.db.Where("id IN ?", ids).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

// UpdateFields applies a map of column → value updates to the channel row.
func (r *channelRepository) UpdateFields(channelID string, fields map[string]any) error {
	if len(fields) == 0 {
		return nil
	}
	return r.db.Model(&model.Channel{}).Where("id = ?", channelID).Updates(fields).Error
}

// IncrementCount adjusts a counter column on the channel row by delta.
// notesCount / usersCount などの集計列向け。
func (r *channelRepository) IncrementCount(channelID, column string, delta int) error {
	return r.db.Model(&model.Channel{}).
		Where("id = ?", channelID).
		UpdateColumn(column, gorm.Expr("\""+column+"\" + ?", delta)).Error
}

// IncrementUsersCountOnFirstPost adds 1 to the channel's usersCount when the
// note noteID (already stored, by authorID in channelID) is the author's
// earliest note in the channel.
//
// 本家は投稿を作った後に (userId, channelId) の note を数え、1 なら加算する。
// 同じ利用者がすばやく 2 回投稿すると、両方の数え上げが 2 を見て 1 回も加算
// されないことがある。ここでは「この投稿より id の小さい投稿がそのチャンネルに
// 無い」を条件にして、条件と加算を 1 文で書く (#3369)。id は時刻順の aidx なので、
// 同時の投稿でも多くの場合は最初の 1 件だけが加算する。
//
// ただし id は INSERT の前に作られ、INSERT は利用者の行のロック (ローカルの利用者
// では WithLockedUser) などを待つので、id の小さい A より先に B が commit することが
// ある。B の hook が A の commit 前に走ると B が加算し、その後 A の hook も加算して
// 2 増える。本家の取りこぼしとは逆向きの誤差で、数え直しの
// backfill channel-users-count で直る。
//
// 投稿そのものがまだ残っていることも条件に入れる。hook は投稿の作成後に非同期で
// 呼ばれるので、その前に消された投稿では加算しない (本家も数え上げが 0 になり
// 加算しない)。消して投稿し直すとまた 1 増えるのは本家と同じ。
//
// 前の投稿の有無は EXISTS でなく、ORDER BY id DESC LIMIT 1 の scalar subquery で
// 見る。EXISTS の中の ORDER BY / LIMIT は planner が外し、見積もりを外すと note の
// 全件を seq scan する。この形なら 000140 の ("channelId", "userId", id) の部分
// index を後ろから 1 件読むだけで済む (実測は migration のコメント)。
func (r *channelRepository) IncrementUsersCountOnFirstPost(channelID, authorID, noteID string) error {
	return r.db.Exec(`UPDATE "channel" SET "usersCount" = "usersCount" + 1
WHERE id = ?
  AND EXISTS (SELECT 1 FROM "note" WHERE id = ? AND "channelId" = ? AND "userId" = ?)
  AND (SELECT id FROM "note" WHERE "channelId" = ? AND "userId" = ? AND id < ? ORDER BY id DESC LIMIT 1) IS NULL`,
		channelID, noteID, channelID, authorID, channelID, authorID, noteID).Error
}

// List returns channels matching the filter, ordered by the requested sort.
// Cursor (SinceID / UntilID) 指定時は id 順 + WHERE id 範囲で frontend
// Paginator に対応する。Offset は無視。
func (r *channelRepository) List(filter model.ChannelListFilter) ([]*model.Channel, error) {
	// 列に入らない文字は保存された値に現れないので、一致しえない (#3025)。
	// **引く前に弾く** — LIKE のパターンに載せるとクエリごと落ちて 500 になる。
	if !storable(filter.Query) || !storable(filter.OwnerID) {
		return nil, nil
	}
	q := r.db.Model(&model.Channel{})
	if filter.OwnerID != "" {
		q = q.Where("\"userId\" = ?", filter.OwnerID)
	}
	if filter.Query != "" {
		// upstream channels/search と同じくエスケープする (#2518)。素通しだと
		// 利用者の入力した % / _ がワイルドカードとして解釈される。ILIKE の
		// ままなのも upstream と同じ (pg_bigm の対象は note 検索のみ)。
		like := "%" + escapeSQLLikePattern(filter.Query) + "%"
		if filter.SearchDescription {
			// upstream type=nameAndDescription: name OR description。
			q = q.Where("name ILIKE ? OR description ILIKE ?", like, like)
		} else {
			q = q.Where("name ILIKE ?", like)
		}
	}
	if filter.IsArchived != nil {
		q = q.Where("\"isArchived\" = ?", *filter.IsArchived)
	}
	if filter.LastNotedAtNotNull {
		q = q.Where("\"lastNotedAt\" IS NOT NULL")
	}
	cursor := filter.SinceID != "" || filter.UntilID != ""
	if filter.SinceID != "" {
		q = q.Where("id > ?", filter.SinceID)
	}
	if filter.UntilID != "" {
		q = q.Where("id < ?", filter.UntilID)
	}
	if cursor {
		q = q.Order(paginationOrder(filter.SinceID, filter.UntilID, "id"))
	} else {
		switch filter.SortBy {
		case "+lastNotedAt":
			q = q.Order("\"lastNotedAt\" ASC NULLS LAST")
		case "-lastNotedAt":
			q = q.Order("\"lastNotedAt\" DESC NULLS LAST")
		case "+id":
			q = q.Order("id ASC")
		case "-id":
			q = q.Order("id DESC")
		case "+name":
			q = q.Order("name ASC")
		case "-name":
			q = q.Order("name DESC")
		case "+notesCount":
			q = q.Order("\"notesCount\" ASC")
		case "-notesCount":
			q = q.Order("\"notesCount\" DESC")
		case "+usersCount":
			q = q.Order("\"usersCount\" ASC")
		case "-usersCount":
			q = q.Order("\"usersCount\" DESC")
		default:
			q = q.Order("\"lastNotedAt\" DESC NULLS LAST")
		}
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	q = q.Limit(limit)
	if !cursor && filter.Offset > 0 {
		q = q.Offset(filter.Offset)
	}
	var rows []*model.Channel
	if err := q.Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}
