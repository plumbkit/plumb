package clientcaps

// ModelVersion is the savings-model version stamped on every row the scorer
// writes. The read path trusts any row with version > 0 over a recompute, so old
// rows keep the version they were scored under and history is never rewritten.
// Bump it whenever a change below alters scores. v1 reproduced the legacy
// profiles; v2 introduced this counterfactual model; v3 added ranged-read baselines.
const ModelVersion = 3

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
	catNone     category = iota // no defensible counterfactual — scores zero
	catRead                     // capability-gated file read / list / find
	catSearch                   // capability-gated content search
	catSemantic                 // LSP/semantic work; credited as native reconstruction cost
	catBatch                    // batching that avoids per-call protocol overhead
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
	// Capability-gated reads / listings.
	"read_file":      {cat: catRead, content: ContentCode},
	"read_symbol":    {cat: catRead, content: ContentCode},
	"find_files":     {cat: catRead, content: ContentProse},
	"list_files":     {cat: catRead, content: ContentProse},
	"list_directory": {cat: catRead, content: ContentProse},

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
	"list_symbols":      {cat: catSemantic, content: ContentCode, reconstruct: 800},
	"explain_symbol":    {cat: catSemantic, content: ContentCode, reconstruct: 400},
	"find_symbol":       {cat: catSemantic, content: ContentCode, reconstruct: 400},
	"get_definition":    {cat: catSemantic, content: ContentCode, reconstruct: 250},
	"diagnostics":       {cat: catSemantic, content: ContentProse, reconstruct: 100},
}

// Score computes the two-axis savings for one completed tool call. A failed call
// (output cleared upstream) scores zero. baselineBytes is the whole-file size a
// read/symbol tool reported in its header, letting a capable client be credited
// the efficiency of a ranged read (zero when absent or for non-read tools).
// batchSize is the number of items a batching tool processed (paths/operations
// length from input_json), ignored for non-batching tools. The model is described
// in docs/internal/tokens_saved_redesign.md §4.2.
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
		return scoreCapabilityGated(caps.NativeFileRead, out, baseline)
	case catSearch:
		return scoreCapabilityGated(caps.NativeSearch, out, baseline)
	case catSemantic:
		return scoreSemantic(caps, m.reconstruct, out)
	case catBatch:
		return scoreBatch(batchSize)
	default:
		return Savings{}
	}
}

// scoreCapabilityGated credits the full delivered context as capability when the
// client has no native equivalent. A capable client would have read the whole
// file itself, so its saving is the efficiency delta a ranged or symbol read
// avoided pulling into context (baseline minus output); a whole-file read saves
// nothing because baseline equals output.
func scoreCapabilityGated(hasNative bool, outputTokens, baselineTokens int) Savings {
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
