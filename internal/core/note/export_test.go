package note

import "time"

// SafeGoForTest exposes safeGo to package-external tests.
func SafeGoForTest(fn func()) { safeGo(fn) }

// ResolveMentionUserIDsForTest runs the mention → userID mapping the create
// path uses (DB lookup, then the WebFinger fetch) so the host rules (#2704,
// #3330) can be pinned without building a full note.
func (s *CreateService) ResolveMentionUserIDsForTest(mentions []Mention, authorHost *string) []string {
	r := s.lookupMentionsInDB(mentions, authorHost)
	s.fetchRemoteMentions(r)
	return r.userIDs()
}

// SetRemoteMentionFetchTimeoutForTest overrides the overall deadline of the
// remote mention fetches.
func (s *CreateService) SetRemoteMentionFetchTimeoutForTest(d time.Duration) {
	s.remoteMentionTimeout = d
}

// RemoteMentionFetchConcurrency exposes the fetch concurrency for tests.
const RemoteMentionFetchConcurrency = remoteMentionFetchConcurrency
