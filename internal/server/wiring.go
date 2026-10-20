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
// lifecycle points at the Server's ordered list of the named parts that have
// to be stopped, and started when they have a start (#3699, #3703,
// docs/architecture.md 4.3), set in the literal that defines w. Nothing reads
// it yet: the parts are added through the registrations of Server
// (s.addStartCloser and the others built from the part value, or
// s.addWorker) and started and stopped through s.lifecycle. It is kept for
// stage 6 of #3666, which narrows what a maintenance rebuild recreates.
//
// caches points at the Server's ordered list of the named caches that can
// drop what they hold in this process (#3721, docs/architecture.md 4.4), the
// same way. The caches are added through s.addCache.
//
// 今は誰も読まないので、消してもテストは落ちない。段階 (6) で、作り直す範囲を
// 部品の単位に絞るときに使う。
type wiring struct {
	repos     repos
	core      core
	fed       fed
	queue     queueProcs
	stream    streamParts
	handlers  apiHandlers
	lifecycle *lifecycle
	caches    *cacheList
}
