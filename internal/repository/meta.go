package repository

import (
	"errors"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// defaultMetaID is the row id used when the singleton meta row has to be
// recreated on the fly. 本家 MetaService.fetch() は 'x' 固定で upsert する。
const defaultMetaID = "x"

// defaultRepositoryURL is the source repository advertised to clients when the
// singleton meta row is created.
//
// AGPL-3.0 section 13 が求める「動いているコードに対応するソース」の案内先で、
// frontend は /about-mkgo と MkSourceCodeAvailablePopup でこれを読む。列 DEFAULT
// (migration/000029) は upstream 互換のため misskey-dev/misskey のままだが、GORM は
// *string の nil を NULL として明示挿入するので**列 DEFAULT は効かない**。ここで
// 入れないと新規インスタンスの案内が空 (= 警告だけ) になる (#2700)。
//
// 値は config.MkGoRepositoryURL と同じ。repository 層は config に依存しない方針
// なので定数を持ち直しており、一致は meta_test.go の
// TestDefaultRepositoryURLMatchesConfig で固定する。
const defaultRepositoryURL = "https://github.com/shiroha-a/mk"

// defaultFeedbackURL is the feedback destination advertised to clients when the
// singleton meta row is created.
//
// `defaultRepositoryURL` と同じ理由で Go 側に持つ — `000029` は `repositoryUrl` と
// **隣り合う 2 行**で `feedbackUrl` の列 DEFAULT も設定しているが、どちらも GORM の
// NULL 明示挿入に負ける。#2700 は前者だけを直したので、こちらは NULL のまま残っていた。
//
// 空だと `/about` のフィードバック行が消え (`v-if="instance.feedbackUrl"`)、**nodeinfo の
// metadata からも欠ける** ので、他インスタンスや一覧サイトからも見えない (#2891)。
//
// 値は config.MkGoFeedbackURL と同じ。一致は meta_test.go の
// TestDefaultFeedbackURLMatchesConfig で固定する。
const defaultFeedbackURL = defaultRepositoryURL + "/issues/new"

// MetaRepository provides data access for server metadata.
type MetaRepository interface {
	Fetch() (*model.Meta, error)
	Update(fields map[string]any) error
	EnsureInitial(id string) error
}

type metaRepository struct {
	db *gorm.DB
}

// NewMetaRepository creates a new MetaRepository.
func NewMetaRepository(db *gorm.DB) MetaRepository {
	return &metaRepository{db: db}
}

func (r *metaRepository) Fetch() (*model.Meta, error) {
	var meta model.Meta
	err := r.db.First(&meta).Error
	if err == nil {
		return &meta, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	// 本家 MetaService.fetch() は行が無ければその場で upsert して作る。mk-go は
	// 起動時の EnsureInitial だけだったため、起動後に行が消えると (DB リセットや
	// 外部ツールによる truncate) 全 API が 500 のまま復旧しなかった。
	if err := r.EnsureInitial(defaultMetaID); err != nil {
		return nil, err
	}
	if err := r.db.First(&meta).Error; err != nil {
		return nil, err
	}
	return &meta, nil
}

func (r *metaRepository) Update(fields map[string]any) error {
	return r.db.Model(&model.Meta{}).Where("TRUE").Updates(fields).Error
}

// EnsureInitial creates the singleton meta row if it does not exist yet.
// Called at server startup so that fresh installs can proceed with
// /api/admin/accounts/create (initial setup) without hitting a 500, and from
// Fetch as a fail-safe when the row disappears afterwards.
func (r *metaRepository) EnsureInitial(id string) error {
	var meta model.Meta
	err := r.db.First(&meta).Error
	if err == nil {
		return nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	// Fetch からも呼ばれるので、同時に複数のリクエストがここへ入りうる。
	// 本家 MetaService.fetch() が upsert を使っているのと同じ理由で、
	// 主キー衝突を無視して冪等にする。
	repositoryURL := defaultRepositoryURL
	feedbackURL := defaultFeedbackURL
	return r.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&model.Meta{
		ID:            id,
		RepositoryURL: &repositoryURL,
		FeedbackURL:   &feedbackURL,
	}).Error
}
