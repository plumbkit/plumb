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

// GenerateName returns a random two-word name in ADJECTIVE-NOUN form, all
// uppercase. Used to give each MCP session a memorable, human-readable
// identity that is stable for the session's lifetime and visible in the TUI.
//
// Example outputs: AZURE-FALCON, TINY-OTTER, WILD-NARWHAL.
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
// Auto-generated names are uppercase; user-provided names preserve their case.
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

var adjectives = []string{
	"AMBER", "ANCIENT", "ARCTIC", "AZURE", "BOLD", "BRAVE", "BRIGHT",
	"BRONZE", "CALM", "CLEVER", "COBALT", "COOL", "CORAL", "COSMIC",
	"CRISP", "CRYSTAL", "DAWN", "DEEP", "EAGER", "EMERALD", "FAINT",
	"FIERCE", "FLEET", "FOREST", "FROZEN", "GENTLE", "GIANT", "GOLDEN",
	"GRAND", "GREEN", "GREY", "HIDDEN", "HUMBLE", "ICY", "IDLE",
	"INDIGO", "JADE", "KEEN", "LIGHT", "LOFTY", "LONE", "LUCKY",
	"LUNAR", "MARBLE", "MIGHTY", "MISTY", "MORNING", "NOBLE", "OLD",
	"PALE", "PATIENT", "POLAR", "PROUD", "PURE", "QUIET", "RADIANT",
	"RAPID", "RARE", "RISING", "ROCKY", "ROYAL", "SAGE", "SCARLET",
	"SERENE", "SILVER", "SLEEK", "SLIM", "SMALL", "SMOOTH", "SOLAR",
	"SOLID", "STARK", "STILL", "STONE", "SWIFT", "TALL", "TEAL",
	"TINY", "TRUE", "VAST", "VELVET", "VIVID", "WARM", "WILD", "WISE",
}

var nouns = []string{
	"ANTELOPE", "BADGER", "BEAR", "BEAVER", "BISON", "BROOK", "CANYON",
	"COBRA", "COMET", "CONDOR", "CRANE", "DEER", "DINGO", "EAGLE",
	"FALCON", "FINCH", "FJORD", "FOX", "GECKO", "GLACIER", "GULL",
	"HAWK", "HERON", "HORSE", "HOUND", "JAGUAR", "LARK", "LEMUR",
	"LEOPARD", "LION", "LYNX", "MAPLE", "MARSH", "MEADOW", "MESA",
	"MINK", "MOOSE", "NARWHAL", "OTTER", "OWL", "PANTHER", "PEAK",
	"PINE", "RAVEN", "REEF", "RIDGE", "RIVER", "ROBIN", "SALMON",
	"SEAL", "SHARK", "SIERRA", "SLATE", "SPARK", "SPRUCE", "STAG",
	"STORM", "STREAM", "TIGER", "TUNDRA", "VALE", "VALLEY", "VINE",
	"VIPER", "VISTA", "WALRUS", "WARBLER", "WHALE", "WOLF", "WREN",
	"YAK", "ZEBRA",
}
