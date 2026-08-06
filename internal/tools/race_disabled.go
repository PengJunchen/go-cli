//go:build !race

package tools

// raceEnabled is false for normal (non -race) builds.
const raceEnabled = false
