// Command elythiajsparamdefs writes the type checks that tie the paramDefs of
// Elythia's own endpoints (internal/api/paramschema/elythia_paramdefs.json) to
// the request types of elythia-js. Run it from the repository root (make
// elythia-js-paramdefs); see internal/api/paramschema/elythiajs.
package main

import (
	"fmt"
	"os"

	"github.com/elythia-network/elythia/internal/api/paramschema"
	"github.com/elythia-network/elythia/internal/api/paramschema/elythiajs"
)

func main() {
	b, err := elythiajs.Render(paramschema.ElythiaParamDefs())
	if err == nil {
		err = os.WriteFile(elythiajs.Path, b, 0o644)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
