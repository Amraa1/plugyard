package upload

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ftpServer is an FTP server that knows just enough to take one file: log
// in, change directory, and store over a passive connection.
type ftpServer struct {
	addr string

	mu       sync.Mutex
	user     string
	password string
	dir      string
	files    map[string]string
	// encrypted counts the files that arrived over an encrypted
	// connection.
	encrypted int
}

func startFTP(t *testing.T, password string) *ftpServer {
	return startFTPOn(t, "127.0.0.1:0", password, nil, false)
}

// startFTPOn starts the server on an address. With a TLS configuration it
// offers FTPS: after AUTH TLS, or from the first byte when implicit.
func startFTPOn(t *testing.T, address, password string, secure *tls.Config, implicit bool) *ftpServer {
	t.Helper()
	ln, err := net.Listen("tcp", address)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", address, err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &ftpServer{addr: ln.Addr().String(), files: map[string]string{}}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, password, secure, implicit)
		}
	}()
	return s
}

func (s *ftpServer) serve(conn net.Conn, password string, secure *tls.Config, implicit bool) {
	defer func() { conn.Close() }()
	encrypted := false
	if implicit {
		conn, encrypted = tls.Server(conn, secure), true
	}
	say := func(format string, args ...any) { fmt.Fprintf(conn, format+"\r\n", args...) }
	say("220 ready")

	var data net.Listener
	defer func() {
		if data != nil {
			data.Close()
		}
	}()
	lines := bufio.NewScanner(conn)
	for lines.Scan() {
		command, arg, _ := strings.Cut(lines.Text(), " ")
		switch strings.ToUpper(command) {
		case "AUTH":
			if secure == nil {
				say("502 not implemented")
				continue
			}
			say("234 go ahead")
			conn, encrypted = tls.Server(conn, secure), true
			lines = bufio.NewScanner(conn)
		case "PBSZ", "PROT":
			say("200 ok")
		case "USER":
			if secure != nil && !encrypted {
				say("530 encrypt first")
				continue
			}
			s.mu.Lock()
			s.user = arg
			s.mu.Unlock()
			say("331 password please")
		case "PASS":
			if arg != password {
				say("530 login incorrect")
				continue
			}
			s.mu.Lock()
			s.password = arg
			s.mu.Unlock()
			say("230 logged in")
		case "FEAT":
			say("211 no features")
		case "TYPE":
			say("200 ok")
		case "CWD":
			if !strings.Contains(arg, "diagnostics") {
				say("550 no such directory")
				continue
			}
			s.mu.Lock()
			s.dir = arg
			s.mu.Unlock()
			say("250 ok")
		case "EPSV":
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				say("425 cannot listen")
				continue
			}
			data = ln
			say("229 Entering Extended Passive Mode (|||%d|)", ln.Addr().(*net.TCPAddr).Port)
		case "STOR":
			if data == nil {
				say("425 no data connection")
				continue
			}
			say("150 send it")
			dc, err := data.Accept()
			if err != nil {
				say("426 lost")
				continue
			}
			if encrypted {
				dc = tls.Server(dc, secure)
			}
			content, _ := io.ReadAll(dc)
			dc.Close()
			s.mu.Lock()
			s.files[arg] = string(content)
			if encrypted {
				s.encrypted++
			}
			s.mu.Unlock()
			say("226 stored")
		case "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

func TestSendOverFTP(t *testing.T) {
	srv := startFTP(t, "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := Send(ctx, local, "ftp://plug:s3cret@"+srv.addr+"/diagnostics/", "CP-1.log", []byte("line 1\nline 2\n")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	srv.mu.Lock()
	if srv.user != "plug" || srv.dir != "/diagnostics/" || srv.files["CP-1.log"] != "line 1\nline 2\n" {
		t.Errorf("the server saw user %q, directory %q and files %v", srv.user, srv.dir, srv.files)
	}
	srv.mu.Unlock()

	for location, want := range map[string]string{
		"ftp://plug:hunter2@" + srv.addr + "/diagnostics/": "the FTP server refused the login of plug",
		"ftp://plug:s3cret@" + srv.addr + "/nowhere/":      "the FTP server has no directory /nowhere/",
		"ftp://127.0.0.1:1/":                               "cannot reach the FTP server at 127.0.0.1:1",
	} {
		err := Send(ctx, local, location, "CP-1.log", []byte("x"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Send to %s = %v, want %q", location, err, want)
		}
		if err != nil && strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the error shows the password: %v", err)
		}
	}
}

func TestSendOverHTTP(t *testing.T) {
	var got struct {
		sync.Mutex
		method, path, user, body string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/full/") {
			http.Error(w, "no room", http.StatusInsufficientStorage)
			return
		}
		body, _ := io.ReadAll(r.Body)
		user, _, _ := r.BasicAuth()
		got.Lock()
		got.method, got.path, got.user, got.body = r.Method, r.URL.Path, user, string(body)
		got.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	ctx := context.Background()
	withUser := strings.Replace(srv.URL, "http://", "http://plug:s3cret@", 1)

	// With and without a slash at the end of the directory.
	for _, dir := range []string{"/diagnostics", "/diagnostics/"} {
		if err := Send(ctx, local, withUser+dir, "CP-1.log", []byte("line 1\n")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		got.Lock()
		if got.method != http.MethodPut || got.path != "/diagnostics/CP-1.log" || got.user != "plug" || got.body != "line 1\n" {
			t.Errorf("the server saw %s %s by %q with %q", got.method, got.path, got.user, got.body)
		}
		got.Unlock()
	}

	err := Send(ctx, local, withUser+"/full/", "CP-1.log", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "507 Insufficient Storage") || strings.Contains(err.Error(), "s3cret") {
		t.Errorf("Send to a full server = %v", err)
	}
}

func TestSendRefusesOtherSchemes(t *testing.T) {
	for location, want := range map[string]string{
		"scp://files.example/diagnostics/": "cannot upload with scp: want one of http, https, ftp, ftps, sftp",
		"files.example/diagnostics/":       "has no scheme",
		"http://[::1":                      "not a valid address",
	} {
		err := Send(context.Background(), local, location, "CP-1.log", nil)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Send to %s = %v, want %q", location, err, want)
		}
	}
	if got := Redact("ftp://plug:s3cret@files.example/d/"); strings.Contains(got, "s3cret") || !strings.Contains(got, "plug") {
		t.Errorf("Redact = %q", got)
	}
}
