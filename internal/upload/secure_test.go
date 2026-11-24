package upload

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// testCertificate is a self-signed certificate for 127.0.0.1, borrowed
// from the test server of net/http.
func testCertificate(t *testing.T) *tls.Config {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &tls.Config{Certificates: srv.TLS.Certificates}
}

func TestSendOverFTPS(t *testing.T) {
	secure := testCertificate(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	trusting := local
	trusting.InsecureSkipVerify = true

	// FTPS that starts as FTP and then turns encryption on.
	srv := startFTPOn(t, "127.0.0.1:0", "s3cret", secure, false)
	location := "ftps://plug:s3cret@" + srv.addr + "/diagnostics/"
	if err := Send(ctx, trusting, location, "CP-1.log", []byte("line 1\n")); err != nil {
		t.Fatalf("Send over FTPS: %v", err)
	}
	// A file of no bytes still has to make the encrypted connection.
	if err := Send(ctx, trusting, location, "empty.log", nil); err != nil {
		t.Fatalf("Send of an empty file over FTPS: %v", err)
	}
	srv.mu.Lock()
	if srv.files["CP-1.log"] != "line 1\n" || srv.encrypted != 2 || srv.user != "plug" {
		t.Errorf("the server holds %v, %d of them sent encrypted, from user %q", srv.files, srv.encrypted, srv.user)
	}
	srv.mu.Unlock()

	// A certificate nobody vouches for is refused unless the site says
	// otherwise, and the message says how.
	err := Send(ctx, local, location, "CP-1.log", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), `set "uploads: { insecureSkipVerify: true }" in the site file`) {
		t.Errorf("Send to a self-signed FTPS server = %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error shows the password: %v", err)
	}
	// So is a wrong password, over the encrypted connection.
	err = Send(ctx, trusting, "ftps://plug:hunter2@"+srv.addr+"/diagnostics/", "CP-1.log", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "the FTPS server refused the login of plug") {
		t.Errorf("Send with a wrong password = %v", err)
	}
	// A server that knows no encryption is not sent a password.
	plain := startFTP(t, "s3cret")
	err = Send(ctx, trusting, "ftps://plug:s3cret@"+plain.addr+"/diagnostics/", "CP-1.log", []byte("x"))
	if err == nil || !strings.Contains(err.Error(), "cannot reach the FTPS server") {
		t.Errorf("Send over FTPS to a server without it = %v", err)
	}
	plain.mu.Lock()
	if plain.password != "" || len(plain.files) != 0 {
		t.Errorf("a server without encryption got the password %q and files %v", plain.password, plain.files)
	}
	plain.mu.Unlock()

	// FTPS that is encrypted from the first byte, on its own port. The
	// test is skipped where that port cannot be used.
	implicit := startFTPOn(t, "127.0.0.1:"+implicitFTPSPort, "s3cret", secure, true)
	if err := Send(ctx, trusting, "ftps://plug:s3cret@"+implicit.addr+"/diagnostics/", "CP-1.log", []byte("line 1\n")); err != nil {
		t.Fatalf("Send over FTPS on port %s: %v", implicitFTPSPort, err)
	}
	implicit.mu.Lock()
	if implicit.files["CP-1.log"] != "line 1\n" || implicit.encrypted != 1 {
		t.Errorf("the server on port %s holds %v, %d sent encrypted", implicitFTPSPort, implicit.files, implicit.encrypted)
	}
	implicit.mu.Unlock()
}

// sftpServer is an SFTP server that keeps its files in memory.
type sftpServer struct {
	addr        string
	fingerprint string
	files       sftp.Handlers
}

func startSFTP(t *testing.T, user, password string) *sftpServer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{
		PasswordCallback: func(conn ssh.ConnMetadata, given []byte) (*ssh.Permissions, error) {
			if conn.User() == user && string(given) == password {
				return nil, nil
			}
			return nil, io.ErrUnexpectedEOF
		},
	}
	config.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	s := &sftpServer{
		addr:        ln.Addr().String(),
		fingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
		files:       sftp.InMemHandler(),
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, config)
		}
	}()
	return s
}

func (s *sftpServer) serve(conn net.Conn, config *ssh.ServerConfig) {
	defer conn.Close()
	_, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(requests)
	for offered := range channels {
		if offered.ChannelType() != "session" {
			offered.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		channel, asked, err := offered.Accept()
		if err != nil {
			return
		}
		go func() {
			for req := range asked {
				req.Reply(req.Type == "subsystem" && string(req.Payload[4:]) == "sftp", nil)
			}
		}()
		server := sftp.NewRequestServer(channel, s.files)
		server.Serve()
		server.Close()
	}
}

// read returns what the server holds under a path, fetched as a client
// would fetch it.
func (s *sftpServer) read(t *testing.T, path string) string {
	t.Helper()
	client, err := ssh.Dial("tcp", s.addr, &ssh.ClientConfig{
		User:            "plug",
		Auth:            []ssh.AuthMethod{ssh.Password("s3cret")},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	files, err := sftp.NewClient(client)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	file, err := files.Open(path)
	if err != nil {
		t.Fatalf("the SFTP server has no %s: %v", path, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSendOverSFTP(t *testing.T) {
	srv := startSFTP(t, "plug", "s3cret")
	// The directory the CSMS names has to be there.
	if err := srv.files.FileCmd.Filecmd(sftp.NewRequest("Mkdir", "/diagnostics")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var mu sync.Mutex
	var notices []string
	policy := local
	policy.Notice = func(text string) {
		mu.Lock()
		notices = append(notices, text)
		mu.Unlock()
	}

	location := "sftp://plug:s3cret@" + srv.addr + "/diagnostics/"
	if err := Send(ctx, policy, location, "CP-1.log", []byte("line 1\nline 2\n")); err != nil {
		t.Fatalf("Send over SFTP: %v", err)
	}
	if got := srv.read(t, "/diagnostics/CP-1.log"); got != "line 1\nline 2\n" {
		t.Errorf("the server holds %q", got)
	}
	// The key of the server is taken as it is, and that is said.
	mu.Lock()
	want := "the SFTP server at " + srv.addr + " showed the key " + srv.fingerprint + ", which was taken as it is"
	if len(notices) != 1 || notices[0] != want {
		t.Errorf("notices = %q, want %q", notices, want)
	}
	mu.Unlock()

	for location, want := range map[string]string{
		"sftp://plug:hunter2@" + srv.addr + "/diagnostics/": "the SFTP server refused the login of plug",
		"sftp://" + srv.addr + "/diagnostics/":              "the SFTP server refused the login of anonymous",
		"sftp://plug:s3cret@" + srv.addr + "/nowhere/":      "the SFTP server has no directory /nowhere/",
		"sftp://plug:s3cret@127.0.0.1:1/diagnostics/":       "cannot reach the SFTP server at 127.0.0.1:1",
	} {
		err := Send(ctx, local, location, "CP-1.log", []byte("x"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Send to %s = %v, want %q", location, err, want)
		}
		if err != nil && (strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "s3cret")) {
			t.Errorf("the error shows the password: %v", err)
		}
	}

	// The limit on where uploads go holds for SFTP too.
	err := Send(ctx, elsewhere, location, "CP-1.log", []byte("x"))
	if _, refused := err.(*RefusedError); !refused {
		t.Errorf("SFTP upload to another machine than the CSMS's = %v", err)
	}
}
