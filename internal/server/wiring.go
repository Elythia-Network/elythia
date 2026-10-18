package server

// wiring is the container of the parts setupRoutes assembles for one server
// generation (#3666 stage 2, #3684, #3686).
//
// Parts are grouped by layer, one field per group, and each group is built by
// its own function (buildRepos for repositories, buildCore for the core
// services). setupRoutes binds the fields to the local names it has always
// used, so moving a group out of setupRoutes does not rewrite the wiring below
// it. No DI framework is used: the builders are ordinary Go functions or
// *Server methods (buildRepos is a method and reads s.db, s.relationCache and
// s.userRepo; buildCore is a method that also receives w for the
// repositories), and a group that depends on another receives it through its
// arguments or through w.
type wiring struct {
	repos repos
	core  core
}
