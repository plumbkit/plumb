package session

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"unicode"
)

// MaxNameLength is the longest generated session name length. Custom names
// use the same cap so the TUI can reserve one stable visual envelope.
const MaxNameLength = 25

// GenerateName returns a random two-word name in adjective-noun form. Used to
// give each MCP session a memorable, human-readable identity that is stable for
// the session's lifetime and visible in the TUI.
//
// Example outputs: azure-falcon, tiny-otter, wild-narwhal.
//
// The word lists are intentionally short and universally safe for work. With
// ~80 adjectives and ~80 nouns there are ~6 400 combinations — enough to make
// collisions across simultaneous sessions very unlikely.
func GenerateName() string {
	adj := adjectives[randIndex(len(adjectives))]
	noun := nouns[randIndex(len(nouns))]
	return adj + "-" + noun
}

func randIndex(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// NormaliseName validates a user-provided session name and returns the stored
// form. Names may contain ASCII letters (any case), digits, and hyphens.
// User-provided names preserve their case.
func NormaliseName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("name is required")
	}
	if len(name) > MaxNameLength {
		return "", fmt.Errorf("name is too long: max %d characters", MaxNameLength)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "", fmt.Errorf("name must not start or end with '-'")
	}
	if strings.Contains(name, "--") {
		return "", fmt.Errorf("name must not contain consecutive hyphens")
	}
	for _, r := range name {
		if r > unicode.MaxASCII {
			return "", fmt.Errorf("name may contain only ASCII letters, digits, and hyphens")
		}
		isLetter := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		isDigit := r >= '0' && r <= '9'
		isHyphen := r == '-'
		if !isLetter && !isDigit && !isHyphen {
			return "", fmt.Errorf("name may contain only letters, digits, and hyphens; got '%c'", r)
		}
	}
	return name, nil
}

// MaxPurposeLength is the longest accepted session purpose tag.
const MaxPurposeLength = 32

// NormalisePurpose validates an optional, human-readable session purpose tag and
// returns the stored form. Purposes may contain ASCII letters (any case), digits,
// and hyphens, up to MaxPurposeLength characters. Surrounding whitespace is
// trimmed; case is preserved. An empty (or whitespace-only) input is valid and
// normalises to "", meaning "no purpose set".
func NormalisePurpose(purpose string) (string, error) {
	purpose = strings.TrimSpace(purpose)
	if purpose == "" {
		return "", nil
	}
	if len(purpose) > MaxPurposeLength {
		return "", fmt.Errorf("purpose is too long: max %d characters", MaxPurposeLength)
	}
	for _, r := range purpose {
		isLetter := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		isDigit := r >= '0' && r <= '9'
		if !isLetter && !isDigit && r != '-' {
			return "", fmt.Errorf("purpose may contain only letters, digits, and hyphens; got '%c'", r)
		}
	}
	return purpose, nil
}

var adjectives = []string{
	"amber", "ancient", "arctic", "azure", "bold", "brave", "bright",
	"bronze", "calm", "clever", "cobalt", "cool", "coral", "cosmic",
	"crisp", "crystal", "dawn", "deep", "eager", "emerald", "faint",
	"fierce", "fleet", "forest", "frozen", "gentle", "giant", "golden",
	"grand", "green", "grey", "hidden", "humble", "icy", "idle",
	"indigo", "jade", "keen", "light", "lofty", "lone", "lucky",
	"lunar", "marble", "mighty", "misty", "morning", "noble", "old",
	"pale", "patient", "polar", "proud", "pure", "quiet", "radiant",
	"rapid", "rare", "rising", "rocky", "royal", "sage", "scarlet",
	"serene", "silver", "sleek", "slim", "small", "smooth", "solar",
	"solid", "stark", "still", "stone", "swift", "tall", "teal",
	"tiny", "true", "vast", "velvet", "vivid", "warm", "wild", "wise",
}

var nouns = []string{
	"antelope", "badger", "bear", "beaver", "bison", "brook", "canyon",
	"cobra", "comet", "condor", "crane", "deer", "dingo", "eagle",
	"falcon", "finch", "fjord", "fox", "gecko", "glacier", "gull",
	"hawk", "heron", "horse", "hound", "jaguar", "lark", "lemur",
	"leopard", "lion", "lynx", "maple", "marsh", "meadow", "mesa",
	"mink", "moose", "narwhal", "otter", "owl", "panther", "peak",
	"pine", "raven", "reef", "ridge", "river", "robin", "salmon",
	"seal", "shark", "sierra", "slate", "spark", "spruce", "stag",
	"storm", "stream", "tiger", "tundra", "vale", "valley", "vine",
	"viper", "vista", "walrus", "warbler", "whale", "wolf", "wren",
	"yak", "zebra",
}
