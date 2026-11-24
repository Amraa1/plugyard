// Package schemas holds the JSON schemas of plugyard's three file types, so
// that editors can complete and check them, and so that the program can
// print them without a network.
package schemas

import (
	"embed"
	"fmt"
	"sort"
)

//go:embed *.schema.json
var files embed.FS

// baseURL is where the schemas are published.
const baseURL = "https://raw.githubusercontent.com/Amraa1/plugyard/main/schemas/"

// kinds maps what a user calls a file type to its schema file.
var kinds = map[string]string{
	"chargerfile": "chargerfile.schema.json",
	"site":        "site.schema.json",
	"scenario":    "scenario.schema.json",
}

// Kinds returns the names of the file types that have a schema, sorted.
func Kinds() []string {
	out := make([]string, 0, len(kinds))
	for kind := range kinds {
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

// Get returns the schema of a file type.
func Get(kind string) ([]byte, error) {
	name, ok := kinds[kind]
	if !ok {
		return nil, fmt.Errorf("there is no schema for %q", kind)
	}
	return files.ReadFile(name)
}

// URL returns the address a file type's schema is published at.
func URL(kind string) string { return baseURL + kinds[kind] }
