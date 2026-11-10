package ingest

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// A connector's URLs come from a tenant administrator and, for a manifest, from whoever controls the remote site: the CMI must
// not become a way to reach what only the CMI can reach (cloud metadata at 169.254.169.254, the Kubernetes API, the other
// services of the cluster, loopback). The address a connection actually goes to is checked when it is made - after DNS, so a
// name that resolves to a private address (or changes its answer between two lookups) is refused too.

var privateNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, c := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4", "64:ff9b::/96", "2001:db8::/32"} {
		_, n, _ := net.ParseCIDR(c)
		out = append(out, n)
	}
	return out
}()

// publicAddress reports whether ip is an ordinary public unicast address.
func publicAddress(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return false
		}
	}
	return true
}

// NewHTTPClient is the client connectors fetch with: no environment proxy, TLS 1.2 or later, a connect timeout, and - unless
// allowPrivate (a development or in-cluster documentation source the operator chose) - no connection to a non-public address.
func NewHTTPClient(allowPrivate bool) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	if !allowPrivate {
		d.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			if ip := net.ParseIP(host); ip == nil || !publicAddress(ip) {
				return fmt.Errorf("connections to %s are not allowed (not a public address)", host)
			}
			return nil
		}
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{Proxy: nil, DialContext: d.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 30 * time.Second, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}}
}

var errTooManyRedirects = errors.New("too many redirects")
