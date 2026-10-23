package entity

import (
	"github.com/elythia-network/elythia/internal/model"
)

// 以下は (5-5) で既定の Packer へ委ねる形にした公開関数 (#3788)。呼び出し側を
// (5-6) 以降で系統ごとに Packer を受け取る形へ移し、(5-10) で消す。

// BadgeEmojiProxyURL is (*Packer).BadgeEmojiProxyURL on the default Packer.
func BadgeEmojiProxyURL(rawURL string) string {
	return currentPacker().BadgeEmojiProxyURL(rawURL)
}

// IdenticonURL is (*Packer).IdenticonURL on the default Packer.
func IdenticonURL(u *model.User) string {
	return currentPacker().IdenticonURL(u)
}

// ProxyAvatarURLString is (*Packer).ProxyAvatarURLString on the default Packer.
func ProxyAvatarURLString(rawURL string) string {
	return currentPacker().ProxyAvatarURLString(rawURL)
}

// ProxyEmojiURLString is (*Packer).ProxyEmojiURLString on the default Packer.
func ProxyEmojiURLString(rawURL string) string {
	return currentPacker().ProxyEmojiURLString(rawURL)
}

// ProxyMediaURL is (*Packer).ProxyMediaURL on the default Packer.
func ProxyMediaURL(rawURL string) string {
	return currentPacker().ProxyMediaURL(rawURL)
}

// ProxyMediaURLPtr is (*Packer).ProxyMediaURLPtr on the default Packer.
func ProxyMediaURLPtr(p *string) *string {
	return currentPacker().ProxyMediaURLPtr(p)
}

// ProxyUserSuppliedMediaURLPtr is (*Packer).ProxyUserSuppliedMediaURLPtr on the default Packer.
func ProxyUserSuppliedMediaURLPtr(p *string) *string {
	return currentPacker().ProxyUserSuppliedMediaURLPtr(p)
}

// StaticAvatarProxyURL is (*Packer).StaticAvatarProxyURL on the default Packer.
func StaticAvatarProxyURL(rawURL string) string {
	return currentPacker().StaticAvatarProxyURL(rawURL)
}

// StaticEmojiProxyURL is (*Packer).StaticEmojiProxyURL on the default Packer.
func StaticEmojiProxyURL(rawURL string) string {
	return currentPacker().StaticEmojiProxyURL(rawURL)
}
