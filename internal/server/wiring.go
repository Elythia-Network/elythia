package server

// wiring is the container of the parts setupRoutes assembles for one server
// generation (#3666 stage 2, #3684, #3686, #3688, #3690, #3692, #3694).
//
// Parts are grouped by layer, one field per group, and each group is built by
// its own function (buildRepos for repositories, buildCore for the core
// services, buildFederation for the federation parts, buildQueue for the
// queue processors, buildStream for the streaming registry, manager and the
// publishers handed out from the stream section, buildHandlers for the API
// handlers). setupRoutes binds the
// fields to the local names it has always used, so moving a group out of
// setupRoutes does not rewrite the wiring below it. No DI framework is used:
// the builders are ordinary Go functions or *Server methods (buildRepos is a
// method and reads s.db, s.relationCache and s.userRepo; buildCore,
// buildFederation, buildQueue and buildHandlers are methods that also receive
// w for the repositories; buildStream is a plain function of the stream
// pubsub and the ID generator). A group that depends on an earlier group receives it as an
// argument, from the variable that holds the earlier builder's result, never
// by reading the earlier field back from w (docs/architecture.md 4.2).
//
// The ordered lists of the parts to start and stop (#3699, #3703,
// docs/architecture.md 4.3) and of the caches that can drop what they hold
// (#3721, docs/architecture.md 4.4) live on the Server, not here: the parts
// are added through s.addStartCloser and the other registrations and the
// caches through s.addCache.
//
// 以前は 2 つの一覧を指す lifecycle / caches も持っていた。メンテナンスで作り直す
// 範囲を部品の単位に絞るとき (#3666 段階 (6)) に使う予定だったが、作り直しが短いので
// 狭めないと決め (#3802)、読まれないまま残っていたので消した (#3808)。
type wiring struct {
	repos    repos
	core     core
	fed      fed
	queue    queueProcs
	stream   streamParts
	handlers apiHandlers
}
