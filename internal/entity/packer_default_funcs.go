package entity

import (
	"context"

	"github.com/elythia-network/elythia/internal/core/notification"
	"github.com/elythia-network/elythia/internal/misc/id"
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

// PackAnnouncement is (*Packer).PackAnnouncement on the default Packer.
func PackAnnouncement(a *model.Announcement, idGen id.Generator, isRead bool, viewerID string) map[string]any {
	return currentPacker().PackAnnouncement(a, idGen, isRead, viewerID)
}

// PackClip is (*Packer).PackClip on the default Packer.
func PackClip(cl *model.Clip, idGen id.Generator, owner *model.User, extras ClipExtras) map[string]any {
	return currentPacker().PackClip(cl, idGen, owner, extras)
}

// PackDriveFile is (*Packer).PackDriveFile on the default Packer.
func PackDriveFile(f *model.DriveFile, idGen id.Generator) DriveFileEntity {
	return currentPacker().PackDriveFile(f, idGen)
}

// PackDriveFileSelf is (*Packer).PackDriveFileSelf on the default Packer.
func PackDriveFileSelf(f *model.DriveFile, idGen id.Generator) DriveFileEntity {
	return currentPacker().PackDriveFileSelf(f, idGen)
}

// PackDriveFileWithRelations is (*Packer).PackDriveFileWithRelations on the default Packer.
func PackDriveFileWithRelations(f *model.DriveFile, idGen id.Generator, folder *model.DriveFolder, user *model.User) DriveFileEntity {
	return currentPacker().PackDriveFileWithRelations(f, idGen, folder, user)
}

// PackDriveFileWithRelationsSelf is (*Packer).PackDriveFileWithRelationsSelf on the default Packer.
func PackDriveFileWithRelationsSelf(f *model.DriveFile, idGen id.Generator, folder *model.DriveFolder, user *model.User) DriveFileEntity {
	return currentPacker().PackDriveFileWithRelationsSelf(f, idGen, folder, user)
}

// PackFollowing is (*Packer).PackFollowing on the default Packer.
func PackFollowing(
	f *model.Following,
	populateFollowee, populateFollower bool,
	lookup UserProfileLookup,
	idGen id.Generator,
) Following {
	return currentPacker().PackFollowing(f, populateFollowee, populateFollower, lookup, idGen)
}

// PackMeDetailed is (*Packer).PackMeDetailed on the default Packer.
func PackMeDetailed(u *model.User, profile *model.UserProfile, idGens ...id.Generator) MeDetailed {
	return currentPacker().PackMeDetailed(u, profile, idGens...)
}

// PackNote is (*Packer).PackNote on the default Packer.
func PackNote(n *model.Note, idGen id.Generator) NoteEntity {
	return currentPacker().PackNote(n, idGen)
}

// PackNoteWithInstance is (*Packer).PackNoteWithInstance on the default Packer.
func PackNoteWithInstance(ctx context.Context, n *model.Note, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, reactionReader BufferedReactionsReader) NoteEntity {
	return currentPacker().PackNoteWithInstance(ctx, n, idGen, instLookup, emojiLookup, reactionReader)
}

// PackNotes is (*Packer).PackNotes on the default Packer.
func PackNotes(ctx context.Context, notes []*model.Note, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, reactionReader BufferedReactionsReader) []NoteEntity {
	return currentPacker().PackNotes(ctx, notes, idGen, instLookup, emojiLookup, reactionReader)
}

// PackNotification is (*Packer).PackNotification on the default Packer.
func PackNotification(n *notification.Notification, user *model.User, note *model.Note, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, opts ...NotificationOption) map[string]any {
	return currentPacker().PackNotification(n, user, note, idGen, instLookup, emojiLookup, opts...)
}

// PackNotifications is (*Packer).PackNotifications on the default Packer.
func PackNotifications(items []NotificationItem, idGen id.Generator, instLookup InstanceLookup, emojiLookup EmojiLookup, opts ...NotificationOption) []map[string]any {
	return currentPacker().PackNotifications(items, idGen, instLookup, emojiLookup, opts...)
}

// PackPageWithContext is (*Packer).PackPageWithContext on the default Packer.
func PackPageWithContext(p *model.Page, ctx PackPageContext) map[string]any {
	return currentPacker().PackPageWithContext(p, ctx)
}

// PackUserDetailed is (*Packer).PackUserDetailed on the default Packer.
func PackUserDetailed(u *model.User, profile *model.UserProfile, idGens ...id.Generator) UserDetailed {
	return currentPacker().PackUserDetailed(u, profile, idGens...)
}

// PackUserForFollowStreamEvent is (*Packer).PackUserForFollowStreamEvent on the default Packer.
func PackUserForFollowStreamEvent(u *model.User, isFollowing, hasPendingFollowRequestFromYou bool, idGen id.Generator) UserDetailed {
	return currentPacker().PackUserForFollowStreamEvent(u, isFollowing, hasPendingFollowRequestFromYou, idGen)
}

// PackUserLite is (*Packer).PackUserLite on the default Packer.
func PackUserLite(u *model.User) UserLite {
	return currentPacker().PackUserLite(u)
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

// ResolvePageDriveFiles is (*Packer).ResolvePageDriveFiles on the default Packer.
func ResolvePageDriveFiles(p *model.Page, lookup PageDriveFileLookup, idGen id.Generator) ([]any, any) {
	return currentPacker().ResolvePageDriveFiles(p, lookup, idGen)
}

// StaticAvatarProxyURL is (*Packer).StaticAvatarProxyURL on the default Packer.
func StaticAvatarProxyURL(rawURL string) string {
	return currentPacker().StaticAvatarProxyURL(rawURL)
}

// StaticEmojiProxyURL is (*Packer).StaticEmojiProxyURL on the default Packer.
func StaticEmojiProxyURL(rawURL string) string {
	return currentPacker().StaticEmojiProxyURL(rawURL)
}
