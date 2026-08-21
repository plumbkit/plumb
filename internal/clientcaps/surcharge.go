package clientcaps

// surcharge.go — the tool-schema surcharge estimate: the per-request token
// cost of the tool definitions served to a client, before it makes a single
// call. This is the honest counterweight to the read/efficiency savings
// scored in score.go — plumb's own tool surface is not free, and the banner
// this feeds must say so alongside any savings claim (PLAN-367).
//
// Methodology follows the PLAN-323 measurement (notes-system-improvements-
// 2026-08-15.md): sum the exact wire byte size of every advertised tool's
// name+description+inputSchema, then convert with charsPerToken. That
// conversion is an estimate; the byte counts a caller supplies are exact.

// surchargeCharsPerToken is the characters-per-token ratio used to convert
// the advertised tool-schema byte total into a token estimate. Schema text is
// dense, low-redundancy JSON — closer to the "compact code" end of the
// tokeniser spectrum than prose — and this is the ratio the PLAN-323
// measurement validated against a real tools/list payload; keep it in step
// with that log if the measurement is ever redone. Named distinctly from
// tokeniser.go's per-family charsPerToken map: this is a single fixed ratio
// for raw schema text, not a per-client/per-content lookup.
const surchargeCharsPerToken = 3.7

// SurchargeReport is the estimated per-request cost of the tool schemas a
// client's session actually advertises: NOT a daemon-observable, NOT a total
// to multiply by call volume — it is paid once by the client for every
// request regardless of how many tools that request calls, so summing it
// across calls would fabricate a number nothing on the wire produced. Report
// it as a rate ("~Xk tokens/request"), never as an aggregate.
type SurchargeReport struct {
	// Tokens is the estimated token cost of the visible tools' schemas.
	Tokens int
	// ToolCount is how many tools contributed — the size of the visible set.
	ToolCount int
	// TotalBytes is the exact wire byte total the estimate was derived from.
	TotalBytes int
}

// ProfileSurcharge estimates the per-request tool-schema surcharge for the
// tools visible under one served profile. schemaBytes maps every REGISTERED
// tool to the wire byte size of its tools/list entry (see
// mcp.Server.ToolSchemaBytes); visible reports whether a given tool name is
// actually advertised under the profile this connection serves (nil means
// "everything visible" — the full profile). Only visible tools contribute;
// a hidden tool costs this client nothing because it never reaches the wire.
func ProfileSurcharge(schemaBytes map[string]int, visible func(name string) bool) SurchargeReport {
	var r SurchargeReport
	for name, b := range schemaBytes {
		if visible != nil && !visible(name) {
			continue
		}
		r.TotalBytes += b
		r.ToolCount++
	}
	r.Tokens = int(float64(r.TotalBytes)/surchargeCharsPerToken + 0.5)
	return r
}
