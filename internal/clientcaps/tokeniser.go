package clientcaps

// Content classifies the kind of text a tool returns, because the characters-per-
// token ratio differs by content: dense code and structured JSON pack fewer
// characters per token than prose. The classification is assigned per tool in the
// tool model (see score.go), not sniffed from the bytes.
type Content string

const (
	ContentCode  Content = "code"
	ContentJSON  Content = "json"
	ContentProse Content = "prose"
)

// defaultCharsPerToken is the fallback ratio when a family or content type has no
// specific entry — the long-standing rough English/code average.
const defaultCharsPerToken = 4.0

// charsPerToken maps a tokeniser family and content type to an estimated number
// of characters per token. These are deliberately rough, defensible estimates;
// they are the model's one tunable knob and live here as pure data so a better
// number is a one-line change. Lower is more tokens (denser content); code and
// JSON pack tighter than prose across all families.
var charsPerToken = map[Family]map[Content]float64{
	FamilyClaude: {ContentCode: 3.5, ContentJSON: 3.0, ContentProse: 4.0},
	FamilyGPT:    {ContentCode: 3.6, ContentJSON: 3.1, ContentProse: 4.0},
	FamilyGemini: {ContentCode: 3.5, ContentJSON: 3.0, ContentProse: 4.0},
}

// tokensFor estimates how many tokens outputBytes of the given content occupy for
// the given family. It never panics on an unknown family/content and never
// divides by zero — both fall back to defaultCharsPerToken.
func tokensFor(family Family, content Content, outputBytes int) int {
	ratio := defaultCharsPerToken
	if byContent, ok := charsPerToken[family]; ok {
		if r, ok := byContent[content]; ok && r > 0 {
			ratio = r
		}
	}
	return int(float64(outputBytes) / ratio)
}
