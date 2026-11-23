//go:build !(js && wasm)

package main

import (
	"fmt"
	"os"
)

// The panel module only makes sense in a browser; on other platforms this
// stub keeps `go build ./...` and `go test ./...` working.
func main() {
	fmt.Fprintln(os.Stderr, "akey-wasm: build with GOOS=js GOARCH=wasm (see `make wasm`)")
	os.Exit(2)
}
