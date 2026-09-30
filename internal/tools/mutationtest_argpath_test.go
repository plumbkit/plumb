package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// TestArgNamingTree_OtherSpellingsOfTheTreeBeingLeft: a volume that folds case or
// Unicode normalisation (macOS's default APFS) gives one directory several
// spellings, and git prints the one on disk while an argument may carry another.
// Comparing spellings, even lowercased, missed an NFC argument under an NFD
// directory, so a command was moved while its argument still named the tree it
// left. Each spelling that reaches the tree being left must be refused.
func TestArgNamingTree_OtherSpellingsOfTheTreeBeingLeft(t *testing.T) {
	root := evalTempDir(t)
	from := filepath.Join(root, "Café") // NFD, as a file created that way is stored
	dest := filepath.Join(from, "wt")
	writeTree(t, root, map[string]string{"Café/wt/x": "x", "Café/test.sh": "x"})
	for name, spelled := range map[string]string{
		"NFC":        filepath.Join(root, "Café", "test.sh"),
		"other case": filepath.Join(root, "cAFÉ", "test.sh"),
	} {
		t.Run(name, func(t *testing.T) {
			if a, err := os.Stat(filepath.Join(from, "test.sh")); err != nil {
				t.Fatal(err)
			} else if b, err := os.Stat(spelled); err != nil || !os.SameFile(a, b) {
				t.Skipf("this volume keeps %s spellings apart: no other spelling of the file exists", name)
			}
			if got, ok := argNamingTree(TaskCommand{Steps: [][]string{{"sh", spelled}}}, from, dest); !ok || got != spelled {
				t.Errorf("a %s spelling of a path in the tree being left must be found; got (%q, %v)", name, got, ok)
			}
			inDest := filepath.Join(root, "Café", "wt", "x")
			if got, ok := argNamingTree(TaskCommand{Steps: [][]string{{"cat", inDest}}}, from, dest); ok {
				t.Errorf("a path in the destination, however spelled, must not be refused; got %q", got)
			}
		})
	}
}
