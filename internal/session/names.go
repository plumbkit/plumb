package session

import (
	"crypto/rand"
	"errors"
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
// The word lists are intentionally short and universally safe for work, which
// makes the pool a few thousand combinations — small enough that simultaneous
// sessions DO collide. A draw is therefore not an address on its own:
// session.Register re-draws under the session-directory flock until the name is
// free, and that check, not the pool size, is what makes a live name unique.
func GenerateName() string {
	adj := adjectives[randIndex(len(adjectives))]
	noun := nouns[randIndex(len(nouns))]
	return adj + "-" + noun
}

// generateName is the draw Register's uniqueness loop uses. It is a variable so
// a test can stub a constant draw and exercise the collision and suffix paths,
// which a real random draw would essentially never reach.
var generateName = GenerateName

func randIndex(n int) int {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// reservedName is the mailbox's "whoever arrives next" sentinel — the literal
// of collab.AddresseeNext, duplicated rather than imported to keep this
// low-level package free of a dependency on internal/collab (there is no edge
// between them in either direction today, and this is not worth adding one).
//
// A session may not take it. leave_note's resolveTarget tests the sentinel
// first, so a session actually named "next" could never be addressed directly
// while shadowing the broadcast address for everyone else.
const reservedName = "next"

// NormaliseName validates a user-provided session name and returns the stored
// form. Names may contain ASCII letters (any case), digits, and hyphens.
// User-provided names preserve their case.
func NormaliseName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("name is required")
	}
	if strings.EqualFold(name, reservedName) {
		return "", fmt.Errorf("name %q is reserved for the mailbox's next-arrival address", reservedName)
	}
	if len(name) > MaxNameLength {
		return "", fmt.Errorf("name is too long: max %d characters", MaxNameLength)
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return "", errors.New("name must not start or end with '-'")
	}
	if strings.Contains(name, "--") {
		return "", errors.New("name must not contain consecutive hyphens")
	}
	if err := checkNameCharset(name); err != nil {
		return "", err
	}
	return name, nil
}

// checkNameCharset enforces the charset rule — ASCII letters (any case), digits
// and hyphens. Split out of NormaliseName to keep that function under the
// cyclomatic-complexity cap as the rule set grows.
func checkNameCharset(name string) error {
	for _, r := range name {
		if r > unicode.MaxASCII {
			return errors.New("name may contain only ASCII letters, digits, and hyphens")
		}
		isLetter := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z')
		isDigit := r >= '0' && r <= '9'
		isHyphen := r == '-'
		if !isLetter && !isDigit && !isHyphen {
			return fmt.Errorf("name may contain only letters, digits, and hyphens; got '%c'", r)
		}
	}
	return nil
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
