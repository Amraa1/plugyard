package upload

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// local is the policy of a charger whose CSMS is on this machine.
var local = Policy{CSMS: netip.MustParseAddr("127.0.0.1")}

// elsewhere is the policy of a charger whose CSMS is on another machine.
// The address is one set aside for documentation.
var elsewhere = Policy{CSMS: netip.MustParseAddr("192.0.2.10")}

func TestUploadsGoOnlyWhereThePolicyLetsThem(t *testing.T) {
	var puts atomic.Int32
	files := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		puts.Add(1)
	}))
	defer files.Close()
	// The same server by name: "localhost" in place of 127.0.0.1.
	byName := strings.Replace(files.URL, "127.0.0.1", "localhost", 1)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	send := func(policy Policy, location string) error {
		return Send(ctx, policy, location+"/d/", "CP-1.log", []byte("x"))
	}
	refused := func(err error) *RefusedError {
		var r *RefusedError
		errors.As(err, &r)
		return r
	}

	// The machine of the CSMS needs no setting, by address or by name:
	// every loopback address is the same machine.
	for _, location := range []string{files.URL, byName} {
		if err := send(local, location); err != nil {
			t.Errorf("upload to %s with the CSMS on this machine: %v", location, err)
		}
	}
	if got := puts.Load(); got != 2 {
		t.Fatalf("the server took %d files, want 2", got)
	}

	// Another machine is refused, and the message says how to allow it.
	err := send(elsewhere, files.URL)
	if r := refused(err); r == nil || r.Never || r.Host != "127.0.0.1" {
		t.Fatalf("upload to another machine than the CSMS's = %v", err)
	}
	for _, want := range []string{
		"uploads to 127.0.0.1 are not allowed: it is not the machine the CSMS is on",
		`"uploads: { allow: [127.0.0.1] }"`, `"plugyard run --allow-upload 127.0.0.1"`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal is missing %q: %v", want, err)
		}
	}
	// A name shows with the address it stands for.
	if err := send(elsewhere, byName); refused(err) == nil || !strings.Contains(err.Error(), "uploads to localhost (") {
		t.Errorf("upload to a name that is refused = %v", err)
	}
	// With no CSMS address known, nothing but the allowed hosts is reached.
	if err := send(Policy{}, files.URL); refused(err) == nil {
		t.Errorf("upload with no CSMS address known = %v", err)
	}
	if got := puts.Load(); got != 2 {
		t.Fatalf("a refused upload reached the server: %d files", got)
	}

	// Allowed by address, and by name whatever its case.
	for location, allow := range map[string]string{files.URL: "127.0.0.1", byName: "LocalHost"} {
		policy := elsewhere
		policy.Allow = []string{"files.example.com", allow}
		if err := send(policy, location); err != nil {
			t.Errorf("upload to %s with %s allowed: %v", location, allow, err)
		}
	}
	// A name that is allowed does not allow the address behind it under
	// another name.
	policy := elsewhere
	policy.Allow = []string{"localhost"}
	if err := send(policy, files.URL); refused(err) == nil {
		t.Errorf("upload to an address whose name alone is allowed = %v", err)
	}

	// Link-local addresses, where cloud metadata is served, are refused
	// whatever the settings.
	for _, host := range []string{"169.254.169.254", "[fe80::1]", "[fd00:ec2::254]", "0.0.0.0"} {
		policy := Policy{CSMS: netip.MustParseAddr("169.254.169.254"), Allow: []string{strings.Trim(host, "[]")}}
		err := send(policy, "http://"+host)
		if r := refused(err); r == nil || !r.Never || !strings.Contains(err.Error(), "are never allowed") {
			t.Errorf("upload to %s = %v, want it never allowed", host, err)
		}
	}
	// The same holds for FTP.
	if err := Send(ctx, elsewhere, "ftp://127.0.0.1:2121/d/", "CP-1.log", nil); refused(err) == nil {
		t.Errorf("FTP upload to another machine than the CSMS's = %v", err)
	}
}

// A server that is allowed cannot send the upload on to one that is not.
func TestUploadsDoNotFollowARedirectToAnotherMachine(t *testing.T) {
	var reached atomic.Bool
	inner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Store(true) }))
	defer inner.Close()
	// The redirect leads to this machine under a name, which only the
	// address of the first server is allowed for.
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, strings.Replace(inner.URL, "127.0.0.1", "localhost", 1)+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer outer.Close()

	policy := elsewhere
	policy.Allow = []string{"127.0.0.1"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := Send(ctx, policy, outer.URL+"/d/", "CP-1.log", []byte("x"))

	// Where "localhost" is also 127.0.0.1 the first connection has
	// approved that address, and the redirect goes through; where it is
	// ::1 it is refused. Either way nothing else is reached.
	var r *RefusedError
	if err != nil && !errors.As(err, &r) {
		t.Errorf("upload through a redirect = %v", err)
	}
	if r != nil && reached.Load() {
		t.Error("the redirect was refused and still reached the server")
	}

	// A redirect to a machine that was never approved is refused.
	away := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://192.0.2.77/d/CP-1.log", http.StatusTemporaryRedirect)
	}))
	defer away.Close()
	err = Send(ctx, local, away.URL+"/d/", "CP-1.log", []byte("x"))
	if !errors.As(err, &r) || r.Host != "192.0.2.77" {
		t.Errorf("upload redirected to another machine = %v, want it refused", err)
	}
}
