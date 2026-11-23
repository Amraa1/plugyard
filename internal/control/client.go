package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// probeTimeout bounds how long finding out who is behind a socket may take.
const probeTimeout = 2 * time.Second

// Client talks to one running site.
type Client struct {
	// Info describes the site behind the socket.
	Info Info
	http *http.Client
}

func newHTTPClient(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
	}
}

// probe asks the process behind a socket who it is.
func probe(ctx context.Context, path string) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	c := &Client{http: newHTTPClient(path)}
	if err := c.do(ctx, http.MethodGet, "/v1/info", nil, &c.Info); err != nil {
		c.http.CloseIdleConnections()
		return nil, err
	}
	return c, nil
}

// Discover returns a client for every site running under dataDir, sorted by
// site name. Sockets left behind by processes that died are removed.
func Discover(ctx context.Context, dataDir string) ([]*Client, error) {
	paths, err := filepath.Glob(filepath.Join(dataDir, socketPrefix+"*"+socketSuffix))
	if err != nil {
		return nil, err
	}

	var clients []*Client
	for _, path := range paths {
		c, err := probe(ctx, path)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_ = os.Remove(path)
			continue
		}
		if c.Info.Protocol != ProtocolVersion {
			return nil, fmt.Errorf("site %q is run by a different version of plugyard; use that version to control it", c.Info.Site)
		}
		clients = append(clients, c)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].Info.Site < clients[j].Info.Site })
	return clients, nil
}

// Chargers returns the state of every charger in the site.
func (c *Client) Chargers(ctx context.Context) ([]ChargerState, error) {
	var wire []wireState
	if err := c.do(ctx, http.MethodGet, "/v1/chargers", nil, &wire); err != nil {
		return nil, err
	}
	states := make([]ChargerState, len(wire))
	for i, w := range wire {
		states[i] = w.ChargerState
		states[i].Powered = w.Powered
	}
	return states, nil
}

// Power powers chargers of the site on or off, or removes them, and returns
// one result per charge point ID, in the order given.
func (c *Client) Power(ctx context.Context, req PowerRequest) ([]ExecResult, error) {
	var results []ExecResult
	err := c.do(ctx, http.MethodPost, "/v1/power", req, &results)
	return results, err
}

// Exec runs an action on chargers of the site and returns one result per
// charge point ID, in the order given.
func (c *Client) Exec(ctx context.Context, req ExecRequest) ([]ExecResult, error) {
	var results []ExecResult
	err := c.do(ctx, http.MethodPost, "/v1/exec", req, &results)
	return results, err
}

// Shutdown asks the site to shut down in an orderly way and waits until its
// process has stopped answering.
func (c *Client) Shutdown(ctx context.Context) error {
	var ignored struct{}
	if err := c.do(ctx, http.MethodPost, "/v1/shutdown", struct{}{}, &ignored); err != nil {
		return err
	}
	for {
		c.http.CloseIdleConnections()
		probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
		err := c.do(probeCtx, http.MethodGet, "/v1/info", nil, &Info{})
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Probe returns a client for the site with the given name if it is running
// under dataDir.
func Probe(ctx context.Context, dataDir, siteName string) (*Client, error) {
	return probe(ctx, SocketPath(dataDir, siteName))
}

// Close releases the client's connection.
func (c *Client) Close() { c.http.CloseIdleConnections() }

func (c *Client) do(ctx context.Context, method, path string, body, result any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}

	// The host is a placeholder: the transport always dials the socket.
	req, err := http.NewRequestWithContext(ctx, method, "http://plugyard"+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("the site answered %s: %s", resp.Status, strings.TrimSpace(string(text)))
	}
	return json.NewDecoder(resp.Body).Decode(result)
}
