package charger

import "testing"

func TestVersionFrom(t *testing.T) {
	for location, want := range map[string]string{
		"ftp://files.example/firmware/fw-2.4.1.bin":    "fw-2.4.1",
		"https://files.example/fw/acme-3.0.tar.gz?x=1": "acme-3.0",
		"ftp://user:secret@files.example/2.4.1":        "2.4.1",
		"ftp://files.example/firmware/v5":              "v5",
		"fw-2.4.1.hex":                                 "fw-2.4.1",
		"https://files.example/":                       "",
		"https://files.example":                        "",
		"ftp://files.example/" + "a-very-long-firmware-name-that-goes-on-and-on-and-on-1.0.bin": "a-very-long-firmware-name-that-goes-on-and-on-and-",
	} {
		if got := versionFrom(location); got != want {
			t.Errorf("versionFrom(%q) = %q, want %q", location, got, want)
		}
	}
}
