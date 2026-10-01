package model

// TimelineDBFilter holds filtering conditions for timeline DB fallback queries.
// *bool フィールドは nil のときデフォルト値として扱う (WithRenotes nil=true 等)。
type TimelineDBFilter struct {
	WithFiles             bool
	WithRenotes           *bool    // nil=true
	WithReplies           *bool    // nil=false
	IncludeMyRenotes      *bool    // nil=true
	IncludeRenotedMyNotes *bool    // nil=true
	IncludeLocalRenotes   *bool    // nil=true
	ViewerID              string   // home/hybridフィルタで使用
	MutedChannelIDs       []string // 指定があれば channelId が IN (...) のノートを除外
	// MutedUserIDs は viewer が mute した user の note を SQL 段階で除外
	// する filter (#892 / #874 follow-up)。userId AND renoteUserId 両方を
	// check する (= upstream Misskey TS QueryService の muting JOIN と同
	// semantics)。Redis fanout 経路 (post-fetch) では in-memory 版
	// ApplyFilter が同等処理を担う。
	//
	// production code path では UseMutingSubquery=true で subquery 経路を
	// 使うので本フィールドは空のまま。test と非 viewer 駆動経路では literal
	// list を直接渡す override path として残す。
	MutedUserIDs []string
	// HideFollowersOnlyReplyFromNonFollowee が true のとき、返信先が followers
	// 限定の投稿で、viewer がその投稿者をフォローしておらず自分でもないノートを
	// 除外する。upstream timeline.ts の noteFilter と同じ判定を SQL 側でも行う。
	// post-fetch だけで弾くと、フィルタ後件数が limit を割って DB fallback が
	// 走り、そちらが除外していないノートを持ってきてしまう。
	HideFollowersOnlyReplyFromNonFollowee bool
	// UseMutingSubquery が true の場合、ViewerID を使って muting テーブルへ
	// の subquery で muted user filter を適用する (#894)。MutedUserIDs を
	// literal IN (...) で渡す方式に対し、bind parameter 数が viewer の
	// mute 件数に比例しないため heavy-mute viewer (>1000 mute) でも
	// PostgreSQL の planning コスト / parameter limit に達しない。
	// MutedUserIDs より優先 (両方 set されたら subquery が勝つ)。
	UseMutingSubquery bool
	// UseRenoteMutingSubquery が true の場合、ViewerID を使って
	// renote_muting テーブルへの subquery で **pure renote のみ** を除外
	// する filter を適用する (#903)。MutedUserIDs と異なり投稿者の plain
	// note は通す。upstream Misskey TS の
	// generateMutedUserRelatedRenotesQuery と同 semantics。
	UseRenoteMutingSubquery bool
	// BlockerIDs は viewer を block している user の id 一覧 (被block、#1681)。
	// note/reply/renote のいずれかの author が含まれる note を除外する
	// (upstream generateBlockedUserQueryForNotes)。viewer 単位で件数が少ない
	// (= 自分を block している相手) ため literal NOT IN で十分。
	BlockerIDs []string
	// MutedInstances は viewer が mute した instance host の一覧 (#1681)。
	// note/reply/renote のいずれかの author host が含まれる note を除外する
	// (upstream generateMutedUserQueryForNotes の mutedInstances 分岐)。host は
	// lowercase 前提 (AP canonical)。
	MutedInstances []string
	// FollowedChannelIDs は viewer がフォローしている channel の id 一覧
	// (mute 済は除外、#1686)。ListHomeTimeline でのみ使う。空でなければ home に
	// `channelId IN (...)` の followed channel note を含める (upstream
	// timeline.ts の followingChannelIds 分岐)。空なら従来どおり channelId IS NULL
	// の note のみ (= followed user の channel note は home から除外される)。
	FollowedChannelIDs []string
	// ExcludeRepliesToOthers は WithReplies に依らず「返信ではない or 自己
	// スレッド」だけを残す。upstream の notes/timeline (HTL) は withReplies
	// パラメータを持たず、この条件を無条件に付ける。per-follow の
	// `following.withReplies` は fanout (push) 側だけに効く仕様なので、
	// DB fallback では反映しない。
	//
	// WithReplies を false 固定にする方法だと Redis 経路の post-fetch
	// フィルタまで巻き込んで fanout が配った返信が消えるため、DB 専用の
	// フラグとして分けている。
	ExcludeRepliesToOthers bool
	// LocalUsersOnly restricts results to notes whose own author is local
	// (`userHost IS NULL`). Set for anonymous visitors under
	// meta.ugcVisibilityForVisitor=local (upstream
	// QueryService.generateUgcVisibilityQueryForVisitor). Attached reply /
	// renote targets are not affected.
	LocalUsersOnly bool
}

// PublicNotesFilter carries the optional filters of the upstream notes.ts
// public-note timeline (#2106 L4 / #2215). Pointer bools distinguish "unset"
// (no filter) from an explicit true/false, matching upstream's `!== undefined`.
type PublicNotesFilter struct {
	Local     bool  // userHost IS NULL (local-only notes)
	Reply     *bool // replyId IS [NOT] NULL
	Renote    *bool // renoteId IS [NOT] NULL
	WithFiles *bool // fileIds != / = '{}'
	Poll      *bool // hasPoll = TRUE / FALSE
}
