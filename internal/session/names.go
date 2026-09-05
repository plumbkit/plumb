package session

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strconv"
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
	switch {
	case name == "":
		return "", fmt.Errorf("%w: name is required", ErrInvalidName)
	case strings.EqualFold(name, reservedName):
		return "", fmt.Errorf("%w: name %q is reserved for the mailbox's next-arrival address", ErrInvalidName, reservedName)
	case len(name) > MaxNameLength:
		return "", fmt.Errorf("%w: name is too long: max %d characters", ErrInvalidName, MaxNameLength)
	case strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-"):
		return "", fmt.Errorf("%w: name must not start or end with '-'", ErrInvalidName)
	case strings.Contains(name, "--"):
		return "", fmt.Errorf("%w: name must not contain consecutive hyphens", ErrInvalidName)
	}
	if err := checkNameCharset(name); err != nil {
		return "", fmt.Errorf("%w: %w", ErrInvalidName, err)
	}
	return name, nil
}

// ErrInvalidName is returned by NormaliseName — and so by Register and Rename —
// when a name breaks a validation rule, as opposed to merely colliding with
// another session's (ErrNameTaken) or failing on I/O. Match it with errors.Is.
//
// The distinction is load-bearing for identity recovery, which reacts to the
// three cases differently: a collision is transient and must be retried against
// an untouched durable record, an invalid stored name can never succeed and so
// is the one case where replacing the record is the repair, and an I/O failure
// is neither — treating it as "invalid" would overwrite a perfectly good
// identity because a disk was briefly busy.
var ErrInvalidName = errors.New("invalid session name")

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

// nameDrawAttempts is how many random draws freeName makes before falling back
// to a numeric suffix. The pool is ~6k names, so against any realistic number
// of live sessions the first draw lands; the retries cost one slice scan each
// and only run in the rare collision case.
const nameDrawAttempts = 8

// freeName returns a generated name that neither a live session other than
// selfID nor a reservation for another session holds.
//
// Termination still holds with reservations in play, and by the same argument
// as before: both live sessions and reservations are finite sets, so at most
// len(live)+len(reserved) suffixes can be occupied and the loop returns by
// i = len(live)+len(reserved)+2 at the latest. Reservations accumulate over a
// database's lifetime where live sessions do not, which is why the bound is
// stated rather than assumed — the adjective/noun pool CAN fill, and the
// suffix path is what makes that survivable rather than fatal.
func freeName(live []Info, selfID string, reserved Reserved) string {
	free := func(n string) bool {
		return !nameTaken(live, n, selfID) && !reserved.taken(n, selfID)
	}
	for range nameDrawAttempts {
		if n := generateName(); free(n) {
			return n
		}
	}
	base := generateName()
	for i := 2; ; i++ {
		if n := withSuffix(base, i); free(n) {
			return n
		}
	}
}

// withSuffix appends "-n" to base, trimming base so the result fits
// MaxNameLength and never ends in a hyphen — NormaliseName rejects both, and a
// generated name has to survive being passed back through it.
//
// The hyphen trim is unconditional, not just after truncation: a base that is
// entirely hyphens is short enough to skip the trim yet still produces "----2".
// A base that trims away to nothing falls back to a letter, since a bare
// "-2" leads with a hyphen. Neither is reachable from generateName's
// adjective-noun output, but withSuffix must not depend on its caller for the
// legality of what it returns.
//
// It does NOT sanitise an arbitrary string — an interior "--" survives and
// NormaliseName would reject it. The contract is that a legal (or empty) base
// yields a legal name.
func withSuffix(base string, n int) string {
	suffix := "-" + strconv.Itoa(n)
	if room := MaxNameLength - len(suffix); len(base) > room {
		base = base[:max(room, 0)]
	}
	if base = strings.Trim(base, "-"); base == "" {
		base = "s"
	}
	return base + suffix
}

// nameTaken reports whether a live session other than selfID answers to name.
//
// The comparison is case-INSENSITIVE even though the mailbox matches addressees
// with SQLite's case-sensitive '='. That is deliberate: being stricter than
// delivery can only reject confusable names, never admit an ambiguous address.
func nameTaken(live []Info, name, selfID string) bool {
	for _, info := range live {
		if info.ID != selfID && strings.EqualFold(info.Name, name) {
			return true
		}
	}
	return false
}
