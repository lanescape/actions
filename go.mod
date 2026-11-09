module github.com/lanescape/actions

go 1.27

toolchain go1.27.3

// lanescape/runners pins a commit of its implementation branch until that
// merges and is tagged; move the pin then, in a pull request of its own
// (specs/0002-releases.md).
require github.com/lanescape/runners v0.0.0-20261109022649-3b97febbb169

require (
	github.com/go-jose/go-jose/v4 v4.1.9 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
)
