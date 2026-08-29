package clientcaps

// surcharge.go — the tool-schema surcharge estimate: the per-request cost of
// the tool definitions served to a client, before it makes a single call.
// This is the honest counterweight to the read/efficiency savings scored in
// score.go — plumb's own tool surface is not free, and the banner this feeds
// must say so alongside any savings claim (PLAN-367).
//
// Methodology follows the PLAN-323 measurement (notes-system-improvements-
// 2026-08-15.md) for the CHAR count: sum the exact wire byte size of every
// advertised tool's name+description+inputSchema. TotalBytes is that exact,
// measured figure — treat it as the primary number.
//
// Tokens is a SEPARATE, EXPLICITLY-LABELLED ESTIMATE on top of it, not a
// second measurement. PLAN-323's own headline "~28,700 tokens" used an
// ASSUMED 3.7 chars/token with no tokenizer behind it — restated here so a
// reader does not inherit that as settled: a PLAN-367 review-round
// measurement (measured 2026-08-22, review round) tokenised the live 59-tool
// payload with a real cl100k tokenizer and got 106,401 chars / 23,276 tokens
// = 4.57 chars/token, ~24% fewer tokens than the 3.7 assumption implied.
// surchargeCharsPerToken uses that measured ratio; it is still an estimate (a
// different client's actual tokenizer vocabulary will differ, and the live
// registry's size drifts as tools are added), just no longer an invented one.

// surchargeCharsPerToken is the characters-per-token ratio used to convert
// the advertised tool-schema byte total into a token ESTIMATE — the measured
// cl100k ratio on a real 59-tool tools/list payload (106,401 chars / 23,276
// tokens, measured 2026-08-22, review round), not a guess. Different clients
// tokenise differently, so treat any Tokens figure this produces as an
// approximation labelled by this ratio and tokenizer, never as exact. Named
// distinctly from tokeniser.go's per-family charsPerToken map: this is a
// single fixed ratio for raw schema text, not a per-client/per-content
// lookup.
const surchargeCharsPerToken = 4.57

// SurchargeReport is the estimated per-request cost of the tool schemas a
// client's session actually advertises: NOT a daemon-observable, NOT a total
// to multiply by call volume — it is paid once by the client for every
// request regardless of how many tools that request calls, so summing it
// across calls would fabricate a number nothing on the wire produced. Report
// it as a rate ("~Xk tokens/request"), never as an aggregate.
type SurchargeReport struct {
	// TotalBytes is the exact, measured wire byte total — the PRIMARY figure;
	// report it alongside Tokens rather than letting the estimate stand alone.
	TotalBytes int
	// Tokens is an ESTIMATE derived from TotalBytes via surchargeCharsPerToken
	// (a measured cl100k ratio on one real payload, not a universal constant).
	// Label it as an estimate wherever it is displayed.
	Tokens int
	// ToolCount is how many tools contributed — the size of the visible set.
	ToolCount int
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
