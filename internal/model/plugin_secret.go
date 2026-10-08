package model

import "time"

// PluginSecret represents the Elythia-only `plugin_secret` table (#3470): one
// encrypted secret value of a server plugin.
//
// 平文は持たない。暗号化と復号は internal/core/pluginsecret が行う。
type PluginSecret struct {
	PluginName string `gorm:"column:pluginName;type:varchar(32);primaryKey" json:"pluginName"`
	Name       string `gorm:"column:name;type:varchar(64);primaryKey" json:"name"`
	// SecretCiphertext is the sealed value. **JSON に出さない。** 暗号文でも、
	// 鍵が漏れた時点で平文と同じになる。
	SecretCiphertext []byte    `gorm:"column:ciphertext;type:bytea;not null" json:"-"`
	UpdatedAt        time.Time `gorm:"column:updatedAt;type:timestamp with time zone;not null" json:"updatedAt"`
}

func (PluginSecret) TableName() string {
	return "plugin_secret"
}
