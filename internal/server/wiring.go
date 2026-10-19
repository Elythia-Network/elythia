package server

// wiring is the container of the parts setupRoutes assembles for one server
// generation (#3666 stage 2, #3684, #3686, #3688, #3690, #3692).
//
// Parts are grouped by layer, one field per group, and each group is built by
// its own function (buildRepos for repositories, buildCore for the core
// services, buildFederation for the federation parts, buildQueue for the
// queue processors, buildStream for the streaming registry, manager and the
// publishers handed out from the stream section). setupRoutes binds the
// fields to the local names it has always used, so moving a group out of
// setupRoutes does not rewrite the wiring below it. No DI framework is used:
// the builders are ordinary Go functions or *Server methods (buildRepos is a
// method and reads s.db, s.relationCache and s.userRepo; buildCore,
// buildFederation and buildQueue are methods that also receive w for the
// repositories; buildStream is a plain function of the stream pubsub and the
// ID generator). A group that depends on an earlier group receives it as an
// argument, from the variable that holds the earlier builder's result, never
// by reading the earlier field back from w (docs/architecture.md 4.2).
type wiring struct {
	repos  repos
	core   core
	fed    fed
	queue  queueProcs
	stream streamParts
}
