package model

// NoteSearchTagFilter carries the optional reply/renote/poll/withFiles filters
// for the SearchByTag repository query (upstream notes/search-by-tag.ts、#1554)。
// Reply/Renote/Poll は nil で無条件、non-nil で IS (NOT) NULL / hasPoll = <bool>。
// WithFiles=true で fileIds != '{}' (添付ありのみ)。
type NoteSearchTagFilter struct {
	Reply     *bool
	Renote    *bool
	Poll      *bool
	WithFiles bool
	// LocalUsersOnly restricts results to notes whose own author is local
	// (`userHost IS NULL`), for anonymous visitors under
	// meta.ugcVisibilityForVisitor=local.
	LocalUsersOnly bool
}
