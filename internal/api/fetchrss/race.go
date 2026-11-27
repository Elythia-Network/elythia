//go:build race

package fetchrss

// raceSlowdown scales the time limit of parsing a feed in -race builds.
const raceSlowdown = 10
