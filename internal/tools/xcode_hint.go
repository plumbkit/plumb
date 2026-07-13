package tools

// XcodeHintFn returns an actionable note for a workspace or document URI.
// It is nil-safe at each call site and keeps filesystem inspection outside the
// tool package.
type XcodeHintFn func(uri string) string

func appendXcodeHint(result, uri string, fn XcodeHintFn) string {
	if fn == nil {
		return result
	}
	return result + fn(uri)
}
