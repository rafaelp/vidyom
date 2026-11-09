// Command vidyomd is not implemented yet. See docs/plans/00-master-plan.md.
package main

import (
	"fmt"
	"os"

	"github.com/rafaelp/vidyom/internal/contract/version"
)

func main() {
	fmt.Fprintf(os.Stderr, "vidyomd %s: not implemented yet\n", version.Version)
	os.Exit(1)
}
