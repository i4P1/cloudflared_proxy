package edgediscovery

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/pkg/errors"
	"golang.org/x/net/proxy"
)

// DialEdge makes a TLS connection to a Cloudflare edge node
func DialEdge(
	ctx context.Context,
	timeout time.Duration,
	tlsConfig *tls.Config,
	edgeTCPAddr *net.TCPAddr,
	localIP net.IP,
) (net.Conn, error) {
	dialer := net.Dialer{}
	if localIP != nil {
		dialer.LocalAddr = &net.TCPAddr{IP: localIP, Port: 0}
	}
	proxyDialer := proxy.FromEnvironmentUsing(&dialer)

	var edgeConn net.Conn
	var err error

	ctxDialer, ok := proxyDialer.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	})
	if ok {
		// Inherit from parent context so we can cancel (Ctrl-C) while dialing
		dialCtx, dialCancel := context.WithTimeout(ctx, timeout)
		defer dialCancel()
		edgeConn, err = ctxDialer.DialContext(dialCtx, "tcp", edgeTCPAddr.String())
	} else {
		edgeConn, err = proxyDialer.Dial("tcp", edgeTCPAddr.String())
	}
	if err != nil {
		return nil, newDialError(err, "DialContext error")
	}

	tlsEdgeConn := tls.Client(edgeConn, tlsConfig)
	tlsEdgeConn.SetDeadline(time.Now().Add(timeout))

	if err = tlsEdgeConn.Handshake(); err != nil {
		return nil, newDialError(err, "TLS handshake with edge error")
	}
	// clear the deadline on the conn; http2 has its own timeouts
	tlsEdgeConn.SetDeadline(time.Time{})
	return tlsEdgeConn, nil
}

// DialError is an error returned from DialEdge
type DialError struct {
	cause error
}

func newDialError(err error, message string) error {
	return DialError{cause: errors.Wrap(err, message)}
}

func (e DialError) Error() string {
	return e.cause.Error()
}

func (e DialError) Cause() error {
	return e.cause
}
