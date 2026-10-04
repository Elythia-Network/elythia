package model

import "time"

// InstanceSignatureCapability represents the `instance_signature_capability`
// table.
//
// mk-go 独自テーブルで、リモートインスタンスがどの署名方式に対応しているかを
// host 単位に記録する。TS は本テーブルを認識しない (instance_secret /
// user_publickey_extra と同じ扱い)。
//
// 各列は独立した観測系統から独立したタイミングで書かれる。ある系統の記録が別系統
// の記録を消さないよう、更新は必ず列単位の部分 upsert で行うこと (#2393)。
type InstanceSignatureCapability struct {
	Host string `gorm:"column:host;type:varchar(128);primaryKey" json:"host"`

	// Ed25519DeclaredAt は actor の assertionMethod[] (FEP-521a Multikey) に
	// Ed25519 鍵があることを最後に確認した時刻。key rotation で assertionMethod
	// が消えてもクリアしない: host 単位の値であり、同一 host の別 actor がまだ
	// 保持している可能性があるため。「最後に確認した時刻」として保持する。
	Ed25519DeclaredAt *time.Time `gorm:"column:ed25519DeclaredAt;type:timestamp with time zone" json:"ed25519DeclaredAt"`

	// InboundAlg は直近に verify を通した inbound HTTP Signature の鍵種別。
	// 署名 header が名乗った algorithm ではなく検証に成功した鍵の種別を入れる
	// (algorithm は "" / hs2019 がありうるため名乗りは実態とずれる)。
	InboundAlg        *string    `gorm:"column:inboundAlg;type:varchar(32)" json:"inboundAlg"`
	InboundObservedAt *time.Time `gorm:"column:inboundObservedAt;type:timestamp with time zone" json:"inboundObservedAt"`

	// LDSignatureSeenAt は LD-Signature (RsaSignature2017) を伴う activity を
	// 最後に受信した時刻。
	LDSignatureSeenAt *time.Time `gorm:"column:ldSignatureSeenAt;type:timestamp with time zone" json:"ldSignatureSeenAt"`

	// Ed25519AcceptedAt は Ed25519 で署名した配送が 2xx を返した最後の時刻。
	//
	// 「相手が Ed25519 を検証できた」証拠としては読めないことに注意。mk-go 自身が
	// そうであるように、inbox が署名検証より先に 202 を返す実装 (verify-in-worker)
	// では、非同期に drop されても 2xx は返る。ここで言えるのは「Ed25519 で送って
	// 同期的には拒否されなかった」まで。同期的な拒否は deliver 側の 4xx degrade
	// (#1067 / #1071) が別途拾う。
	Ed25519AcceptedAt *time.Time `gorm:"column:ed25519AcceptedAt;type:timestamp with time zone" json:"ed25519AcceptedAt"`

	UpdatedAt time.Time `gorm:"column:updatedAt;type:timestamp with time zone;not null" json:"updatedAt"`
}

func (InstanceSignatureCapability) TableName() string {
	return "instance_signature_capability"
}

// Signature algorithm labels stored in the `inboundAlg` column. HTTP Signature
// の algorithm 文字列ではなく検証に成功した鍵の種別を表す語彙なので、値域は
// この 2 つに閉じる。
const (
	SignatureAlgRSA     = "rsa"
	SignatureAlgEd25519 = "ed25519"
)

// SupportsEd25519 reports whether any of the three signals indicates that the
// remote host can handle Ed25519 HTTP Signatures.
//
// 宣言 / 配送成功 / 受信観測のいずれか 1 つでも立っていれば true。3 系統はそれぞれ
// 取りこぼす条件が違う (design 参照) ので、OR で束ねて初めて実態に近づく。
func (c *InstanceSignatureCapability) SupportsEd25519() bool {
	if c == nil {
		return false
	}
	if c.Ed25519DeclaredAt != nil || c.Ed25519AcceptedAt != nil {
		return true
	}
	return c.InboundAlg != nil && *c.InboundAlg == SignatureAlgEd25519
}

// SupportsLDSignature reports whether an LD-Signature has ever been observed
// from the remote host.
func (c *InstanceSignatureCapability) SupportsLDSignature() bool {
	return c != nil && c.LDSignatureSeenAt != nil
}
