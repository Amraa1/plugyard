package upload

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// Policy says where a charger may upload to. The CSMS chooses the location
// of an upload, so without a limit a hostile or compromised CSMS could make
// the machine plugyard runs on connect to any address that machine can
// reach, such as the internal services of a network.
//
// A charger may always upload to the machine its CSMS is on, which gives
// the CSMS no reach it did not have. Any other host has to be allowed.
type Policy struct {
	// CSMS is the address the charger reaches its CSMS at. The zero value
	// means it is not known, and then only allowed hosts can be reached.
	CSMS netip.Addr
	// Allow lists further hosts, by name or by address, as they are
	// written in a location.
	Allow []string
	// InsecureSkipVerify turns off the check of the certificate an https
	// or ftps server shows, for servers with a self-signed one.
	InsecureSkipVerify bool
	// Notice, if set, is told what is worth a line in the charger's log
	// about an upload that goes ahead.
	Notice func(text string)
}

func (p Policy) notice(format string, args ...any) {
	if p.Notice != nil {
		p.Notice(fmt.Sprintf(format, args...))
	}
}

// awsMetadataV6 is where AWS serves instance metadata over IPv6. It is not
// a link-local address, unlike its IPv4 counterpart.
var awsMetadataV6 = netip.MustParsePrefix("fd00:ec2::/32")

// RefusedError says an upload was not made because of where it would go.
type RefusedError struct {
	// Host is the host as the location names it, and Addr the address it
	// stands for.
	Host string
	Addr netip.Addr
	// Never is set for an address no setting can allow.
	Never bool
}

func (e *RefusedError) Error() string {
	where := e.Host
	if addr := e.Addr.String(); e.Addr.IsValid() && addr != e.Host {
		where = fmt.Sprintf("%s (%s)", e.Host, addr)
	}
	if e.Never {
		return fmt.Sprintf("uploads to %s are never allowed: it is a link-local address, where the metadata of cloud machines is served", where)
	}
	return fmt.Sprintf("uploads to %s are not allowed: it is not the machine the CSMS is on. "+
		"To allow it, add \"uploads: { allow: [%s] }\" to the site file, or start the charger with \"plugyard run --allow-upload %s\"",
		where, e.Host, e.Host)
}

// never reports whether an address is one no upload may go to.
func never(addr netip.Addr) bool {
	return addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsMulticast() ||
		addr.IsUnspecified() || awsMetadataV6.Contains(addr)
}

// guard applies a policy to the connections of one upload.
type guard struct {
	policy Policy
	// approved holds the addresses this upload was let through to. An FTP
	// server asks for a second connection for the file itself, and that
	// one arrives as an address with no name.
	approved map[netip.Addr]bool
}

func newGuard(policy Policy) *guard {
	return &guard{policy: policy, approved: map[netip.Addr]bool{}}
}

// permits reports whether a connection to addr, which the location calls
// host, may be made.
func (g *guard) permits(host string, addr netip.Addr) bool {
	csms := g.policy.CSMS.Unmap()
	switch {
	case never(addr):
		return false
	case g.approved[addr]:
		return true
	case csms.IsValid() && (addr == csms || (csms.IsLoopback() && addr.IsLoopback())):
		// Every loopback address is the same machine.
		return true
	}
	for _, allowed := range g.policy.Allow {
		if strings.EqualFold(allowed, host) {
			return true
		}
		if a, err := netip.ParseAddr(allowed); err == nil && a.Unmap() == addr {
			return true
		}
	}
	return false
}

// dial connects to address if the policy permits it. The check is made on
// the address that is connected to, after the name has been looked up, so
// a name that points somewhere else by the time of the upload does not get
// around it.
func (g *guard) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	var addrs []netip.Addr
	if addr, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{addr}
	} else if addrs, err = net.DefaultResolver.LookupNetIP(ctx, "ip", host); err != nil {
		return nil, err
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no address", host)
	}

	refused := &RefusedError{Host: host, Addr: addrs[0].Unmap(), Never: true}
	var dialErr error
	for _, addr := range addrs {
		addr = addr.Unmap()
		if !g.permits(host, addr) {
			if !never(addr) {
				refused = &RefusedError{Host: host, Addr: addr}
			}
			continue
		}
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(addr.String(), port))
		if err == nil {
			g.approved[addr] = true
			return conn, nil
		}
		dialErr = err
	}
	if dialErr != nil {
		return nil, dialErr
	}
	return nil, refused
}
