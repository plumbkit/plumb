package tools

// XcodeHintFn returns an actionable note for a workspace or document URI.
// It is nil-safe at each call site and keeps filesystem inspection outside the
// tool package.
type XcodeHintFn func(uri string) string

// XcodeProofFn records that SourceKit-LSP returned a non-empty semantic result.
// It is deliberately separate from configuration and warm-up state: only a real
// definition, reference, or workspace-symbol response proves semantic readiness.
type XcodeProofFn func()

func recordXcodeProof(fn XcodeProofFn, proven bool) {
	if proven && fn != nil {
		fn()
	}
}

func appendXcodeHint(result, uri string, fn XcodeHintFn) string {
	if fn == nil {
		return result
	}
	return result + fn(uri)
}
