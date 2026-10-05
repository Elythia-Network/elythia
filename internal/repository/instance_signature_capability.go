package repository

import (
	"time"

	"github.com/elythia-network/elythia/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// InstanceSignatureCapabilityRepository provides data access for the mk-go-only
// `instance_signature_capability` table (#2393).
//
// Record* メソッドはどれも「自分の系統の列 + updatedAt だけ」を更新する部分
// upsert である。3 系統 (宣言 / 受信観測 / 配送観測) は独立したタイミングで書かれ
// るため、行まるごとを書く upsert にすると後から来た観測が他系統の記録を NULL で
// 潰してしまう。
type InstanceSignatureCapabilityRepository interface {
	// FindByHost returns the row for host, or gorm.ErrRecordNotFound.
	FindByHost(host string) (*model.InstanceSignatureCapability, error)
	// FindManyByHosts returns the rows matching any of the given hosts. 一覧
	// 表示が host ごとに 1 クエリ投げる N+1 を避けるための bulk lookup。空入力は
	// nil を返す。
	FindManyByHosts(hosts []string) ([]*model.InstanceSignatureCapability, error)
	// RecordInboundAlg stores the key type of a successfully verified inbound
	// HTTP Signature.
	RecordInboundAlg(host, alg string, at time.Time) error
	// RecordLDSignature marks that an activity carrying an LD-Signature was
	// received from host.
	RecordLDSignature(host string, at time.Time) error
	// RecordEd25519Accepted marks that an Ed25519-signed delivery to host was
	// answered with a 2xx.
	RecordEd25519Accepted(host string, at time.Time) error
	// RecordEd25519Declared marks that host's actor published an Ed25519 key
	// via assertionMethod (FEP-521a Multikey).
	RecordEd25519Declared(host string, at time.Time) error
}

type instanceSignatureCapabilityRepository struct {
	db *gorm.DB
}

// NewInstanceSignatureCapabilityRepository creates a new
// InstanceSignatureCapabilityRepository.
func NewInstanceSignatureCapabilityRepository(db *gorm.DB) InstanceSignatureCapabilityRepository {
	return &instanceSignatureCapabilityRepository{db: db}
}

func (r *instanceSignatureCapabilityRepository) FindByHost(host string) (*model.InstanceSignatureCapability, error) {
	if !storable(host) {
		return nil, ErrNotFound
	}
	var row model.InstanceSignatureCapability
	if err := r.db.Where("host = ?", host).First(&row).Error; err != nil {
		return nil, err
	}
	return &row, nil
}

func (r *instanceSignatureCapabilityRepository) FindManyByHosts(hosts []string) ([]*model.InstanceSignatureCapability, error) {
	if len(hosts) == 0 {
		return nil, nil
	}
	var rows []*model.InstanceSignatureCapability
	if err := r.db.Where("host IN ?", hosts).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (r *instanceSignatureCapabilityRepository) RecordInboundAlg(host, alg string, at time.Time) error {
	if host == "" || alg == "" {
		return nil
	}
	return r.upsert(&model.InstanceSignatureCapability{
		Host:              host,
		InboundAlg:        &alg,
		InboundObservedAt: &at,
		UpdatedAt:         at,
	}, "inboundAlg", "inboundObservedAt")
}

func (r *instanceSignatureCapabilityRepository) RecordLDSignature(host string, at time.Time) error {
	if host == "" {
		return nil
	}
	return r.upsert(&model.InstanceSignatureCapability{
		Host:              host,
		LDSignatureSeenAt: &at,
		UpdatedAt:         at,
	}, "ldSignatureSeenAt")
}

func (r *instanceSignatureCapabilityRepository) RecordEd25519Accepted(host string, at time.Time) error {
	if host == "" {
		return nil
	}
	return r.upsert(&model.InstanceSignatureCapability{
		Host:              host,
		Ed25519AcceptedAt: &at,
		UpdatedAt:         at,
	}, "ed25519AcceptedAt")
}

func (r *instanceSignatureCapabilityRepository) RecordEd25519Declared(host string, at time.Time) error {
	if host == "" {
		return nil
	}
	return r.upsert(&model.InstanceSignatureCapability{
		Host:              host,
		Ed25519DeclaredAt: &at,
		UpdatedAt:         at,
	}, "ed25519DeclaredAt")
}

// upsert inserts row, or updates only the named columns (plus updatedAt) when
// the host already exists.
//
// DoUpdates に載せる列を呼び出し側が明示するのが要点。clause.Assignments で
// 更新列を絞らないと GORM は全列を書きに行き、その観測系統が知らない列 (他系統の
// 記録) を zero value で上書きしてしまう。
func (r *instanceSignatureCapabilityRepository) upsert(row *model.InstanceSignatureCapability, columns ...string) error {
	assignments := make(map[string]any, len(columns)+1)
	for _, col := range columns {
		assignments[col] = columnValue(row, col)
	}
	assignments["updatedAt"] = row.UpdatedAt
	return r.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "host"}},
		DoUpdates: clause.Assignments(assignments),
	}).Create(row).Error
}

// columnValue maps a column name to the corresponding field of row. upsert が
// 更新値を組み立てるためだけの補助で、値域は upsert の呼び出し側が渡す列名に閉じる。
func columnValue(row *model.InstanceSignatureCapability, column string) any {
	switch column {
	case "ed25519DeclaredAt":
		return row.Ed25519DeclaredAt
	case "inboundAlg":
		return row.InboundAlg
	case "inboundObservedAt":
		return row.InboundObservedAt
	case "ldSignatureSeenAt":
		return row.LDSignatureSeenAt
	case "ed25519AcceptedAt":
		return row.Ed25519AcceptedAt
	default:
		return nil
	}
}
