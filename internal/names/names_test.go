package names

import (
	"regexp"
	"strings"
	"testing"
)

func TestWordListsAreCleanAndLargeEnough(t *testing.T) {
	word := regexp.MustCompile(`^[a-z]+$`)
	for label, list := range map[string][]string{"adjectives": adjectives, "nouns": nouns} {
		if len(list) < 150 {
			t.Errorf("%s has %d words, want at least 150", label, len(list))
		}
		seen := map[string]bool{}
		for _, w := range list {
			if !word.MatchString(w) {
				t.Errorf("%s: %q is not a lowercase word", label, w)
			}
			if seen[w] {
				t.Errorf("%s: %q is listed twice", label, w)
			}
			seen[w] = true
		}
	}
}

func TestGenerateAvoidsTakenNames(t *testing.T) {
	taken := map[string]bool{}
	for range 5000 {
		name := Generate(func(n string) bool { return taken[n] })
		if taken[name] {
			t.Fatalf("Generate returned %q twice", name)
		}
		if err := Validate(name); err != nil {
			t.Fatalf("Generate returned %q, which is not a valid name: %v", name, err)
		}
		taken[name] = true
	}
}

func TestGenerateAddsANumberWhenNamesRunOut(t *testing.T) {
	// Every plain name is taken, so only numbered ones are free.
	plain := regexp.MustCompile(`^[a-z]+_[a-z]+$`)
	name := Generate(func(n string) bool { return plain.MatchString(n) })
	if !regexp.MustCompile(`^[a-z]+_[a-z]+2$`).MatchString(name) {
		t.Errorf("Generate returned %q, want a name ending in 2", name)
	}
}

func TestValidate(t *testing.T) {
	for _, good := range []string{"lobby", "front-desk", "Bay_7", "a.b", "7up"} {
		if err := Validate(good); err != nil {
			t.Errorf("Validate(%q) = %v, want it accepted", good, err)
		}
	}
	for _, bad := range []string{"", "-lobby", "front desk", "bay/7", "é", strings.Repeat("a", 64)} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) accepted it", bad)
		}
	}
}
