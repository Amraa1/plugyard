// Package upload sends a file to the place a CSMS names in GetDiagnostics.
package upload

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// Schemes lists the kinds of location Send can upload to.
var Schemes = []string{"http", "https", "ftp", "ftps", "sftp"}

// implicitFTPSPort is the port of FTPS that is encrypted from the first
// byte. On any other port FTPS starts as FTP and then turns encryption on.
const implicitFTPSPort = "990"

// Redact returns a location without the password it may carry, for logs.
func Redact(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return location
	}
	return u.Redacted()
}

// Send uploads data as a file called name to the directory at location:
// with PUT for http and https, with STOR for ftp and ftps, and over SSH for
// sftp. A user name and a password in the location are used to log in. The
// policy says which machines may be connected to; an upload it stops fails
// with a *RefusedError.
func Send(ctx context.Context, policy Policy, location, name string, data []byte) error {
	g := newGuard(policy)
	u, err := url.Parse(location)
	if err != nil {
		return fmt.Errorf("the location is not a valid address: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return put(ctx, g, u, name, data)
	case "ftp":
		return stor(ctx, g, u, name, data, false)
	case "ftps":
		return stor(ctx, g, u, name, data, true)
	case "sftp":
		return overSSH(ctx, g, u, name, data)
	case "":
		return fmt.Errorf("the location %q has no scheme: want one of %s", Redact(location), strings.Join(Schemes, ", "))
	default:
		return fmt.Errorf("cannot upload with %s: want one of %s", u.Scheme, strings.Join(Schemes, ", "))
	}
}

// asRefused returns the refusal inside err, if that is what it is, so that
// the reason is not buried in what a library wrapped around it.
func asRefused(err error) error {
	var refused *RefusedError
	if errors.As(err, &refused) {
		return refused
	}
	return nil
}

// certificateHint adds what to do about a certificate that was not
// accepted.
func certificateHint(err error) error {
	var rejected *tls.CertificateVerificationError
	if errors.As(err, &rejected) {
		return fmt.Errorf("%w. If the server's certificate is self-signed, set \"uploads: { insecureSkipVerify: true }\" in the site file", err)
	}
	return err
}

func put(ctx context.Context, g *guard, dir *url.URL, name string, data []byte) error {
	target := dir.JoinPath(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	// Every connection goes through the guard, also the one a redirect
	// leads to. No proxy is used: the guard has to see where the upload
	// really goes.
	transport := &http.Transport{
		DialContext:       g.dial,
		DisableKeepAlives: true,
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: g.policy.InsecureSkipVerify},
	}
	defer transport.CloseIdleConnections()
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		if refused := asRefused(err); refused != nil {
			return refused
		}
		return certificateHint(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the server answered PUT %s with %s", target.Redacted(), resp.Status)
	}
	return nil
}

// login returns the user name and password of a location, or what FTP
// calls an anonymous login.
func login(dir *url.URL) (user, password string) {
	if dir.User == nil {
		return "anonymous", "anonymous"
	}
	password, _ = dir.User.Password()
	return dir.User.Username(), password
}

func stor(ctx context.Context, g *guard, dir *url.URL, name string, data []byte, secure bool) error {
	kind, port := "FTP", "21"
	if secure {
		kind = "FTPS"
	}
	if dir.Port() != "" {
		port = dir.Port()
	}
	host := net.JoinHostPort(dir.Hostname(), port)
	implicit := secure && port == implicitFTPSPort

	config := &tls.Config{
		ServerName:         dir.Hostname(),
		InsecureSkipVerify: g.policy.InsecureSkipVerify,
		// Some servers only take the connection for the file when it
		// continues the session of the first one.
		ClientSessionCache: tls.NewLRUClientSessionCache(0),
	}
	// The guard makes the connection for the commands and the one for
	// the file, which the server may ask to have made to another address.
	// The library leaves encrypting them to whoever makes them: the first
	// one only when FTPS is encrypted from the start, the others always.
	connections := 0
	options := []ftp.DialOption{ftp.DialWithContext(ctx), ftp.DialWithDialFunc(func(network, address string) (net.Conn, error) {
		conn, err := g.dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		connections++
		if secure && (implicit || connections > 1) {
			return tls.Client(conn, config), nil
		}
		return conn, nil
	})}
	if secure && !implicit {
		options = append(options, ftp.DialWithExplicitTLS(config))
	}
	conn, err := ftp.Dial(host, options...)
	if err != nil {
		if refused := asRefused(err); refused != nil {
			return refused
		}
		return fmt.Errorf("cannot reach the %s server at %s: %w", kind, host, certificateHint(err))
	}
	defer conn.Quit()

	user, password := login(dir)
	if err := conn.Login(user, password); err != nil {
		return fmt.Errorf("the %s server refused the login of %s: %w", kind, user, certificateHint(err))
	}
	if folder := strings.Trim(dir.Path, "/"); folder != "" {
		if err := conn.ChangeDir(dir.Path); err != nil {
			return fmt.Errorf("the %s server has no directory %s: %w", kind, dir.Path, err)
		}
	}
	if err := conn.Stor(name, bytes.NewReader(data)); err != nil {
		if refused := asRefused(err); refused != nil {
			return refused
		}
		return fmt.Errorf("the %s server refused the file: %w", kind, certificateHint(err))
	}
	return nil
}

// overSSH uploads with SFTP. SSH has no certificate authorities: a client
// is meant to know the key of the server beforehand, and a GetDiagnostics
// request has no place for one. Like most chargers, plugyard takes the key
// the server shows, and says which one it was.
func overSSH(ctx context.Context, g *guard, dir *url.URL, name string, data []byte) error {
	port := "22"
	if dir.Port() != "" {
		port = dir.Port()
	}
	host := net.JoinHostPort(dir.Hostname(), port)

	conn, err := g.dial(ctx, "tcp", host)
	if err != nil {
		if refused := asRefused(err); refused != nil {
			return refused
		}
		return fmt.Errorf("cannot reach the SFTP server at %s: %w", host, err)
	}
	defer conn.Close()
	deadline, bounded := ctx.Deadline()
	if bounded {
		_ = conn.SetDeadline(deadline)
	}

	user, password := login(dir)
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
			// Some servers ask for the password as a question.
			ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = password
				}
				return answers, nil
			}),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			g.policy.notice("the SFTP server at %s showed the key %s, which was taken as it is", host, ssh.FingerprintSHA256(key))
			return nil
		},
	}
	if bounded {
		config.Timeout = time.Until(deadline)
	}
	session, channels, requests, err := ssh.NewClientConn(conn, host, config)
	if err != nil {
		if strings.Contains(err.Error(), "unable to authenticate") {
			return fmt.Errorf("the SFTP server refused the login of %s: %w", user, err)
		}
		return fmt.Errorf("cannot reach the SFTP server at %s: %w", host, err)
	}
	client := ssh.NewClient(session, channels, requests)
	defer client.Close()

	files, err := sftp.NewClient(client)
	if err != nil {
		return fmt.Errorf("the server at %s does not offer SFTP: %w", host, err)
	}
	defer files.Close()

	// The path of the location is taken from the root of the server, as
	// it is written.
	target := path.Join(dir.Path, name)
	if folder := strings.Trim(dir.Path, "/"); folder != "" {
		if info, err := files.Stat(dir.Path); err != nil || !info.IsDir() {
			return fmt.Errorf("the SFTP server has no directory %s", dir.Path)
		}
	}
	file, err := files.Create(target)
	if err != nil {
		return fmt.Errorf("the SFTP server refused the file: %w", err)
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return fmt.Errorf("the SFTP server refused the file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("the SFTP server refused the file: %w", err)
	}
	return nil
}
