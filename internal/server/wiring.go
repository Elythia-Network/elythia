package server

// wiring is the container of the parts setupRoutes assembles for one server
// generation (#3666 stage 2, #3684).
//
// Parts are grouped by layer, one field per group, and each group is built by
// its own function (buildRepos for repositories). setupRoutes binds the
// fields to the local names it has always used, so moving a group out of
// setupRoutes does not rewrite the wiring below it. No DI framework is used:
// the builders are ordinary Go functions or *Server methods (buildRepos is a
// method and reads s.db, s.relationCache and s.userRepo), and a group that
// depends on another receives it through its arguments or through w.
type wiring struct {
	repos repos
}
