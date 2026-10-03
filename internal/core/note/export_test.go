package note

// SafeGoForTest exposes safeGo to package-external tests.
func SafeGoForTest(fn func()) { safeGo(fn) }

// ResolveMentionUserIDsForTest exposes the mention → userID mapping so the
// IDN-host regression (#2704) can be pinned without building a full note.
func (s *CreateService) ResolveMentionUserIDsForTest(mentions []Mention, authorHost *string) []string {
	return s.resolveMentionUserIDs(mentions, authorHost)
}
