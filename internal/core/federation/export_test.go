package federation

import (
	"time"

	"github.com/shiroha-a/mk/internal/activitypub"
	"github.com/shiroha-a/mk/internal/model"
)

// ExtractEmojiTags exposes the unexported extractEmojiTags for external tests.
var ExtractEmojiTags = extractEmojiTags

// UpsertEmojis exposes the unexported upsertEmojis method for external tests.
func (r *Resolver) UpsertEmojis(tags []activitypub.EmojiTag, host string) []string {
	return r.upsertEmojis(tags, host)
}

// ExtractAttachments exposes the unexported extractAttachments for external tests (#378).
var ExtractAttachments = extractAttachments

// UpsertAttachments exposes the unexported upsertAttachments method for external tests.
func (r *Resolver) UpsertAttachments(docs []activitypub.Document, userID, host *string) []string {
	return r.upsertAttachments(docs, userID, host)
}

// SetProbeBudget shrinks the per-document image-probe budget so tests do not
// have to burn attachmentProbeBudget of wall clock.
func (r *Resolver) SetProbeBudget(d time.Duration) { r.probeBudget = d }

// CollectAttachedFileTypes exposes the unexported collectAttachedFileTypes for external tests.
func (r *Resolver) CollectAttachedFileTypes(fileIDs []string) []string {
	return r.collectAttachedFileTypes(fileIDs)
}

// ExtractMentionTags exposes the unexported extractMentionTags for external tests (#397).
var ExtractMentionTags = extractMentionTags

// MergeMentionIDs exposes the unexported mergeMentionIDs for external tests (#397).
var MergeMentionIDs = mergeMentionIDs

// ResolveMentionedUserIDs exposes the unexported resolveMentionedUserIDs for external tests.
func (r *Resolver) ResolveMentionedUserIDs(hrefs []string) ([]string, error) {
	return r.resolveMentionedUserIDs(hrefs)
}

// SpecifiedAudience exposes the unexported specifiedAudience for external tests.
var SpecifiedAudience = specifiedAudience

// ExceedsRemoteMentionLimit exposes the unexported exceedsRemoteMentionLimit
// for external tests, with a fixed limit.
func ExceedsRemoteMentionLimit(n *model.Note, mentions, tagHrefs []string, limit int) bool {
	return exceedsRemoteMentionLimit(n, mentions, tagHrefs, func() int { return limit })
}

// ExceedsRemoteMentionLimitFunc exposes exceedsRemoteMentionLimit with a lazy
// limit, so tests can observe whether the limit is looked up.
var ExceedsRemoteMentionLimitFunc = exceedsRemoteMentionLimit

// ProcessRemoteMove exposes the unexported processRemoteMove for external
// tests (#2414)。refreshActor 経由では届かないゲート (クールダウン / 連鎖上限 /
// URI 不一致) を直接突くために公開する。
func (r *Resolver) ProcessRemoteMove(src *model.User, prevMovedAt *time.Time, visited map[string]bool) {
	r.processRemoteMove(src, prevMovedAt, visited, nil)
}

// KeyFetchFailureCount exposes the size of the key-refresh backoff map so tests
// can assert that expired entries are pruned (they are otherwise unobservable).
func (r *Resolver) KeyFetchFailureCount() int {
	r.keysMu.RLock()
	defer r.keysMu.RUnlock()
	return len(r.keyFetchFailures)
}

// MarkKeyFetchFailed exposes markKeyFetchFailed for external tests.
func (r *Resolver) MarkKeyFetchFailed(userID string) { r.markKeyFetchFailed(userID) }

// FeaturedPinLimit exposes the pin cap so external tests do not hardcode it.
// **同じ値を push 側 (`handleAdd`) と pull 側 (`resolveFeaturedNotes`) が使う**
// ので、片方だけ変えたらテストが落ちる形にしておく。
const FeaturedPinLimit = featuredPinLimit
