# GOROOT for the Go toolchain installed by the `golang` package.
# Adds /opt/go/bin to PATH so `go`, `gofmt`, etc. resolve.
#
# GOPATH is intentionally not set here — modern Go modules don't need
# it, and per-user GOPATH (~/go by default) doesn't belong in a
# system-wide profile.
export GOROOT="/opt/go"
export PATH="$GOROOT/bin:$PATH"
