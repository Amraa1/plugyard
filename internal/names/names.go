// Package names makes up names for chargers, in the way Docker names
// containers: an adjective and a noun, such as brisk_volt.
package names

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"regexp"
	"strconv"
)

var valid = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// Validate reports whether a name given by the user can be used.
func Validate(name string) error {
	switch {
	case name == "":
		return errors.New("the name is empty")
	case len(name) > 63:
		return errors.New("the name is longer than 63 characters")
	case !valid.MatchString(name):
		return fmt.Errorf("invalid name %q: use letters, digits, '_', '.' and '-', starting with a letter or digit", name)
	}
	return nil
}

// Generate returns a random name for which taken reports false. When free
// names are hard to find it appends a number, so it always succeeds.
func Generate(taken func(string) bool) string {
	for range 20 {
		if name := random(); !taken(name) {
			return name
		}
	}
	base := random()
	for n := 2; ; n++ {
		if name := base + strconv.Itoa(n); !taken(name) {
			return name
		}
	}
}

func random() string {
	return adjectives[rand.IntN(len(adjectives))] + "_" + nouns[rand.IntN(len(nouns))]
}

var adjectives = []string{
	"able", "agile", "alert", "amber", "ample", "apt", "ardent", "avid", "azure", "balmy",
	"bold", "brave", "breezy", "bright", "brisk", "busy", "calm", "candid", "careful", "cheery",
	"chief", "civil", "clean", "clear", "clever", "cool", "cosmic", "cosy", "crisp", "curious",
	"dandy", "daring", "deft", "direct", "eager", "early", "earnest", "easy", "elated", "epic",
	"exact", "fair", "fancy", "fast", "fine", "firm", "fit", "fleet", "fluent", "fond",
	"frank", "free", "fresh", "friendly", "frosty", "gentle", "giant", "glad", "golden", "good",
	"graceful", "grand", "great", "green", "handy", "happy", "hardy", "hearty", "honest", "humble",
	"ideal", "jolly", "jovial", "keen", "kind", "lively", "loyal", "lucid", "lucky", "lunar",
	"mellow", "merry", "mighty", "mild", "modest", "neat", "nifty", "nimble", "noble", "novel",
	"open", "patient", "peppy", "placid", "plucky", "polite", "prime", "proud", "pure", "quick",
	"quiet", "rapid", "ready", "regal", "robust", "rosy", "royal", "rustic", "sage", "sharp",
	"shiny", "silent", "silver", "simple", "sleek", "smart", "smooth", "snappy", "snug", "solar",
	"solid", "sound", "spry", "stable", "steady", "stellar", "stout", "sturdy", "sunny", "super",
	"sure", "swift", "tidy", "trim", "true", "trusty", "upbeat", "valid", "vivid", "warm",
	"wise", "witty", "worthy", "young", "zesty", "zippy", "active", "brawny", "dapper", "gallant",
}

// The nouns are electrical terms and units, and surnames of people who
// worked out how electricity behaves.
var nouns = []string{
	"ampere", "anode", "arc", "armature", "battery", "breaker", "bridge", "brush", "busbar", "cable",
	"capacitor", "cathode", "cell", "charge", "choke", "circuit", "coil", "conductor", "contact", "core",
	"coulomb", "current", "cycle", "diode", "dynamo", "electrode", "electron", "farad", "feeder", "field",
	"filament", "flux", "fuse", "gauss", "generator", "grid", "ground", "henry", "hertz", "impulse",
	"inductor", "insulator", "inverter", "ion", "joule", "junction", "kelvin", "lamp", "lead", "line",
	"load", "loop", "lumen", "magnet", "mains", "meter", "motor", "neutron", "node", "ohm",
	"outlet", "phase", "photon", "pilot", "plug", "pole", "proton", "pulse", "pylon", "quantum",
	"rail", "rectifier", "relay", "resistor", "rotor", "shunt", "siemens", "socket", "solenoid", "spark",
	"stator", "surge", "switch", "terminal", "thyristor", "toroid", "transformer", "transistor", "turbine", "varistor",
	"volt", "watt", "weber", "winding", "wire", "alternator", "ballast", "beacon", "antenna", "dipole",
	"alvarez", "arago", "ayrton", "bardeen", "bell", "blodgett", "bose", "brattain", "braun", "clarke",
	"cooper", "curie", "daniell", "davy", "edison", "faraday", "ferranti", "fleming", "franklin", "galvani",
	"gilbert", "gramme", "hall", "heaviside", "helmholtz", "hopkinson", "kilby", "kirchhoff", "lamme", "latimer",
	"lenz", "lorentz", "marconi", "maxwell", "meitner", "moog", "morse", "noether", "noyce", "oersted",
	"peltier", "planck", "popov", "seebeck", "shockley", "steinmetz", "sturgeon", "swan", "tesla", "volta",
}
