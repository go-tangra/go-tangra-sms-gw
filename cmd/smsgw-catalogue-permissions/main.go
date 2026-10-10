// Command smsgw-catalogue-permissions prints the module's permissions
// (resource:action) as a JSON array for its catalogue entry (spec 035). It
// depends only on the manifest package, so the release job runs it without
// building the UI.
package main

import (
	"encoding/json"
	"io"
	"os"

	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

func main() { os.Exit(cataloguePermissions(os.Stdout)) }

func cataloguePermissions(w io.Writer) int {
	if err := json.NewEncoder(w).Encode(smsgwmanifest.PermissionRefs()); err != nil {
		return 1
	}
	return 0
}
