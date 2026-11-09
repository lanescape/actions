module github.com/lanescape/actions

go 1.27

toolchain go1.27.3

require (
	// lanescape/runners pins a commit of its implementation branch until that
	// merges and is tagged; move the pin then, in a pull request of its own
	// (specs/0002-releases.md).
	github.com/lanescape/runners v0.0.0-20261109125402-69940f3c6d9c
	go.yaml.in/yaml/v3 v3.0.6
)

require (
	github.com/coreos/go-oidc/v3 v3.21.0 // indirect
	github.com/go-jose/go-jose/v4 v4.1.9 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/jonboulle/clockwork v0.5.0 // indirect
	golang.org/x/oauth2 v0.37.0 // indirect
)
