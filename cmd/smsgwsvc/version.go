package main

import (
	"fmt"
	"os"

	"github.com/go-tangra/go-tangra-portal/sdk/v4/pkg/gatewayclient"
)

// version is stamped at build time (-ldflags "-X main.version=..."); "dev"
// for local builds. `smsgwsvc version` prints it and exits.
var version = "dev"

func init() {
	if len(os.Args) == 2 && (os.Args[1] == "version" || os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println(version)
		os.Exit(0)
	}
}

// Report the release to the gateway at registration.
func init() { gatewayclient.SetBuildVersion(version) }
