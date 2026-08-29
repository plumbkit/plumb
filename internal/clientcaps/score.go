package clientcaps

// ModelVersion is the savings-model version stamped on every row the scorer
// writes. The read path trusts any row with version > 0 over a recompute, so old
// rows keep the version they were scored under and history is never rewritten.
// Bump it whenever a change below alters scores. v1 reproduced the legacy
// profiles; v2 introduced this counterfactual model; v3 added ranged-read
// baselines; v4 stops crediting a capable client's PLAIN ranged read as
// efficiency — a native ranged Read reproduces that saving itself, so scoring
// it measured the agent's own restraint, not plumb's contribution. Credit
// stays where the mechanism is plumb-only: name-addressed access
// (read_symbol resolves a symbol name to a byte range without the client
// first reading the whole file to find it) and reconstruction-cost tools
// (catSemantic). A reader must never sum Capability/Efficiency across rows of
// different versions and present it as one figure — filter on this constant.
const ModelVersion = 4

// Savings is the two-axis result of scoring one tool call. Capability is work the
// client could not have done natively at all (dominant for thin clients);
// Efficiency is fewer tokens for the same result the client could have obtained
// itself (dominant for capable CLI agents). They are reported separately and
// summed for the headline figure.
type Savings struct {
	Capability int
	Efficiency int
}

// Total is the headline savings figure: capability plus efficiency.
func (s Savings) Total() int { return s.Capability + s.Efficiency }

// category selects which counterfactual branch a tool is scored under.
type category int

const (
	catNone      category = iota // no defensible counterfactual — scores zero
	catRead                      // capability-gated PLAIN ranged read a native ranged Read reproduces
	catReadNamed                 // capability-gated NAME-ADDRESSED read (plumb-only mechanism)
	catSearch                    // capability-gated content search
	catSemantic                  // LSP/semantic work; credited as native reconstruction cost
	catBatch                     // batching that avoids per-call protocol overhead
)

// toolModel is the per-tool scoring shape: its category, the content type of its
// output (for the tokeniser ratio), and — for semantic tools — the estimated cost
// of reconstructing the same answer natively (grep + read N files + reason), in
// tokens. These reconstruction estimates are the model's other tunable knob.
type toolModel struct {
	cat         category
	content     Content
	reconstruct int
}

// perCallOverhead is the protocol/round-trip cost (in tokens) of one separate
// tool call that a batching tool avoids. read_multiple_files of N paths saves
// (N-1) such round trips versus N individual reads.
const perCallOverhead = 80

// toolModels is the scoring model keyed by tool name. Tools absent from the map
// score zero — there is no defensible counterfactual for them yet (most write
// tools, git, utilities). The semantic reconstruct estimates reuse the
// established per-tool figures: a call hierarchy reconstructed by hand costs far
// more than a single get_definition.
var toolModels = map[string]toolModel{
	// Capability-gated PLAIN ranged reads / listings — a capable client's own
	// native ranged Read (or directory listing) can reproduce the same
	// byte-range saving itself, so v4 gives it no efficiency credit here.
	"read_file":  {cat: catRead, content: ContentCode},
	"find_files": {cat: catRead, content: ContentProse},

	// Capability-gated NAME-ADDRESSED read — the client asks for a symbol by
	// name, not a byte range it would otherwise have had to discover by
	// reading the whole file first. That resolution is plumb-only, so a
	// capable client still earns the efficiency delta.
	"read_symbol": {cat: catReadNamed, content: ContentCode},

	// Content search.
	"search_in_files": {cat: catSearch, content: ContentCode},

	// Batching — efficiency from avoided per-call overhead.
	"read_multiple_files": {cat: catBatch, content: ContentJSON},
	"transaction_apply":   {cat: catBatch, content: ContentJSON},

	// Semantic / LSP — native reconstruction cost, credited to every client.
	"call_hierarchy":    {cat: catSemantic, content: ContentCode, reconstruct: 1500},
	"type_hierarchy":    {cat: catSemantic, content: ContentCode, reconstruct: 800},
	"find_references":   {cat: catSemantic, content: ContentCode, reconstruct: 800},
	"workspace_symbols": {cat: catSemantic, content: ContentCode, reconstruct: 800},
	"file_outline":      {cat: catSemantic, content: ContentCode, reconstruct: 800},
	"explain_symbol":    {cat: catSemantic, content: ContentCode, reconstruct: 400},
	"get_definition":    {cat: catSemantic, content: ContentCode, reconstruct: 250},
	"diagnostics":       {cat: catSemantic, content: ContentProse, reconstruct: 100},
}

// Score computes the two-axis savings for one completed tool call. A failed call
// (output cleared upstream) scores zero. baselineBytes is the whole-file size a
// read/symbol tool reported in its header, letting a capable client be credited
// the efficiency of a ranged or name-addressed read (zero when absent or for
// non-read tools). batchSize is the number of items a batching tool processed
// (paths/operations length from input_json), ignored for non-batching tools.
// The model is described internally.
func Score(tool, clientName string, outputBytes, baselineBytes, batchSize int, success bool) Savings {
	if !success {
		return Savings{}
	}
	m, ok := toolModels[tool]
	if !ok || m.cat == catNone {
		return Savings{}
	}
	caps := Lookup(clientName)
	out := tokensFor(caps.Tokeniser, m.content, outputBytes)
	baseline := tokensFor(caps.Tokeniser, m.content, baselineBytes)

	switch m.cat {
	case catRead:
		return scoreCapabilityGatedNative(caps.NativeFileRead, out)
	case catReadNamed:
		return scoreCapabilityGatedNamed(caps.NativeFileRead, out, baseline)
	case catSearch:
		return scoreCapabilityGatedNamed(caps.NativeSearch, out, baseline)
	case catSemantic:
		return scoreSemantic(caps, m.reconstruct, out)
	case catBatch:
		return scoreBatch(batchSize)
	default:
		return Savings{}
	}
}

// scoreCapabilityGatedNative scores a PLAIN ranged read: a thin client (no
// native file read at all) is credited the delivered context as capability,
// exactly as before. A capable client earns nothing here — its own native
// ranged Read could have fetched the same bytes, so crediting the delta would
// be scoring the agent's restraint rather than anything plumb contributed.
// This is the v4 change; see ModelVersion.
func scoreCapabilityGatedNative(hasNative bool, outputTokens int) Savings {
	if !hasNative {
		return Savings{Capability: outputTokens}
	}
	return Savings{}
}

// scoreCapabilityGatedNamed scores a NAME-ADDRESSED read or search: a thin
// client is credited the delivered context as capability; a capable client
// still earns the efficiency delta (baseline minus output) because the
// mechanism — resolving a name to the relevant bytes without first reading or
// grepping the whole file — is not something its own native tools reproduce
// unassisted.
func scoreCapabilityGatedNamed(hasNative bool, outputTokens, baselineTokens int) Savings {
	if !hasNative {
		return Savings{Capability: outputTokens}
	}
	if delta := baselineTokens - outputTokens; delta > 0 {
		return Savings{Efficiency: delta}
	}
	return Savings{}
}

// scoreSemantic credits the native reconstruction cost net of plumb's own output.
// A client that can reconstruct the answer itself (native LSP, or file/search
// access to grep and read) gets it as an efficiency delta; a thin client that
// could not reconstruct it at all gets it as capability.
func scoreSemantic(caps Capabilities, reconstruct, outputTokens int) Savings {
	value := reconstruct - outputTokens
	if value <= 0 {
		return Savings{}
	}
	if caps.NativeLSP || caps.NativeFileRead || caps.NativeSearch {
		return Savings{Efficiency: value}
	}
	return Savings{Capability: value}
}

// scoreBatch credits the per-call overhead avoided by processing batchSize items
// in one call instead of batchSize separate calls.
func scoreBatch(batchSize int) Savings {
	if batchSize <= 1 {
		return Savings{}
	}
	return Savings{Efficiency: perCallOverhead * (batchSize - 1)}
}
