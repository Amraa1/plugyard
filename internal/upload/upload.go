// Package upload sends a file to the place a CSMS names in GetDiagnostics.
package upload

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

// Schemes lists the kinds of location Send can upload to.
var Schemes = []string{"http", "https", "ftp"}

// Redact returns a location without the password it may carry, for logs.
func Redact(location string) string {
	u, err := url.Parse(location)
	if err != nil {
		return location
	}
	return u.Redacted()
}

// Send uploads data as a file called name to the directory at location:
// with PUT for http and https, and with STOR for ftp. A user name and a
// password in the location are used to log in.
func Send(ctx context.Context, location, name string, data []byte) error {
	u, err := url.Parse(location)
	if err != nil {
		return fmt.Errorf("the location is not a valid address: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return put(ctx, u, name, data)
	case "ftp":
		return stor(ctx, u, name, data)
	case "":
		return fmt.Errorf("the location %q has no scheme: want one of %s", Redact(location), strings.Join(Schemes, ", "))
	default:
		return fmt.Errorf("cannot upload with %s: want one of %s", u.Scheme, strings.Join(Schemes, ", "))
	}
}

func put(ctx context.Context, dir *url.URL, name string, data []byte) error {
	target := dir.JoinPath(name)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, target.String(), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the server answered PUT %s with %s", target.Redacted(), resp.Status)
	}
	return nil
}

func stor(ctx context.Context, dir *url.URL, name string, data []byte) error {
	host := dir.Host
	if dir.Port() == "" {
		host = net.JoinHostPort(dir.Hostname(), "21")
	}
	options := []ftp.DialOption{ftp.DialWithContext(ctx)}
	if deadline, ok := ctx.Deadline(); ok {
		options = append(options, ftp.DialWithTimeout(time.Until(deadline)))
	}
	conn, err := ftp.Dial(host, options...)
	if err != nil {
		return fmt.Errorf("cannot reach the FTP server at %s: %w", host, err)
	}
	defer conn.Quit()

	user, password := "anonymous", "anonymous"
	if dir.User != nil {
		user = dir.User.Username()
		password, _ = dir.User.Password()
	}
	if err := conn.Login(user, password); err != nil {
		return fmt.Errorf("the FTP server refused the login of %s: %w", user, err)
	}
	if folder := strings.Trim(dir.Path, "/"); folder != "" {
		if err := conn.ChangeDir(dir.Path); err != nil {
			return fmt.Errorf("the FTP server has no directory %s: %w", dir.Path, err)
		}
	}
	if err := conn.Stor(name, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("the FTP server refused the file: %w", err)
	}
	return nil
}
