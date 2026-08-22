package setup

import "github.com/plumbkit/plumb/internal/clienttemplates"

// ClientTemplates re-exports internal/clienttemplates.ByClient under setup's
// established name — kept for API stability (internal/cli and this package's
// own tests reference it). PLAN-366 relocated the embedded template bodies to
// internal/clienttemplates (a Foundation-layer package) so internal/mcp can
// render the SAME per-client body into the MCP initialize `instructions`
// field without importing this Domain-layer package (internal/arch's layer
// rule forbids Transport importing Domain). setup remains the place client
// setup code reaches for these bodies; clienttemplates is the shared source.
var ClientTemplates = clienttemplates.ByClient

// TemplateForClient returns client's own managed-block body and true, or
// ("", false) when client has no per-client template registered. Delegates to
// internal/clienttemplates.ForClient — see ClientTemplates' doc comment.
func TemplateForClient(client string) (string, bool) {
	return clienttemplates.ForClient(client)
}
