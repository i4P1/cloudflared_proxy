package supervisor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/netip"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/pkg/errors"
	"github.com/quic-go/quic-go"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	"github.com/cloudflare/cloudflared/client"
	"github.com/cloudflare/cloudflared/connection"
	"github.com/cloudflare/cloudflared/edgediscovery"
	"github.com/cloudflare/cloudflared/edgediscovery/allregions"
	"github.com/cloudflare/cloudflared/features"
	"github.com/cloudflare/cloudflared/fips"
	"github.com/cloudflare/cloudflared/ingress"
	"github.com/cloudflare/cloudflared/ingress/origins"
	"github.com/cloudflare/cloudflared/management"
	"github.com/cloudflare/cloudflared/orchestration"
	quicpogs "github.com/cloudflare/cloudflared/quic"
	v3 "github.com/cloudflare/cloudflared/quic/v3"
	"github.com/cloudflare/cloudflared/retry"
	"github.com/cloudflare/cloudflared/signal"
	"github.com/cloudflare/cloudflared/tunnelrpc/pogs"
	"github.com/cloudflare/cloudflared/tunnelstate"
)

const (
	dialTimeout = 15 * time.Second
)

type TunnelConfig struct {
	ClientConfig       *client.Config
	GracePeriod        time.Duration
	CloseConnOnce      *sync.Once // Used to close connectedSignal no more than once
	EdgeAddrs          []string
	Region             string
	EdgeIPVersion      allregions.ConfigIPVersion
	EdgeBindAddr       net.IP
	HAConnections      int
	IsAutoupdated      bool
	LBPool             string
	Tags               []pogs.Tag
	Log                *zerolog.Logger
	LogTransport       *zerolog.Logger
	Observer           *connection.Observer
	ReportedVersion    string
	Retries            uint
	MaxEdgeAddrRetries uint8
	RunFromTerminal    bool

	NeedPQ bool

	NamedTunnel         *connection.TunnelProperties
	ProtocolSelector    connection.ProtocolSelector
	EdgeTLSConfigs      map[connection.Protocol]*tls.Config
	ICMPRouterServer    ingress.ICMPRouterServer
	OriginDNSService    *origins.DNSResolverService
	OriginDialerService *ingress.OriginDialerService

	RPCTimeout         time.Duration
	WriteStreamTimeout time.Duration

	DisableQUICPathMTUDiscovery         bool
	QUICConnectionLevelFlowControlLimit uint64
	QUICStreamLevelFlowControlLimit     uint64
}

func (c *TunnelConfig) connectionOptions(originLocalAddr string, previousAttempts uint8) *client.ConnectionOptionsSnapshot {
	// attempt to parse out origin IP, but don't fail since it's informational field
	host, _, _ := net.SplitHostPort(originLocalAddr)
	originIP := net.ParseIP(host)
	return c.ClientConfig.ConnectionOptionsSnapshot(originIP, previousAttempts)
}

func StartTunnelDaemon(
	ctx context.Context,
	config *TunnelConfig,
	orchestrator *orchestration.Orchestrator,
	connectedSignal *signal.Signal,
	reconnectCh chan ReconnectSignal,
	graceShutdownC <-chan struct{},
) error {
	s, err := NewSupervisor(config, orchestrator, reconnectCh, graceShutdownC)
	if err != nil {
		return err
	}
	return s.Run(ctx, connectedSignal)
}

type ConnectivityError struct {
	reachedMaxRetries bool
}

func NewConnectivityError(hasReachedMaxRetries bool) *ConnectivityError {
	return &ConnectivityError{
		reachedMaxRetries: hasReachedMaxRetries,
	}
}

func (e *ConnectivityError) Error() string {
	return fmt.Sprintf("connectivity error - reached max retries: %t", e.HasReachedMaxRetries())
}

func (e *ConnectivityError) HasReachedMaxRetries() bool {
	return e.reachedMaxRetries
}

// EdgeAddrHandler provides a mechanism switch between behaviors in ServeTunnel
// for handling the errors when attempting to make edge connections.
type EdgeAddrHandler interface {
	// ShouldGetNewAddress will check the edge connection error and determine if
	// the edge address should be replaced with a new one. Also, will return if the
	// error should be recognized as a connectivity error, or otherwise, a general
	// application error.
	ShouldGetNewAddress(connIndex uint8, err error) (needsNewAddress bool, connectivityError error)
}

func NewIPAddrFallback(maxRetries uint8) *ipAddrFallback {
	return &ipAddrFallback{
		retriesByConnIndex: make(map[uint8]uint8),
		maxRetries:         maxRetries,
	}
}

// ipAddrFallback will have more conditions to fall back to a new address for certain
// edge connection errors. This means that this handler will return true for isConnectivityError
// for more cases like duplicate connection register and edge quic dial errors.
type ipAddrFallback struct {
	m                  sync.Mutex
	retriesByConnIndex map[uint8]uint8
	maxRetries         uint8
}

func (f *ipAddrFallback) ShouldGetNewAddress(connIndex uint8, err error) (needsNewAddress bool, connectivityError error) {
	f.m.Lock()
	defer f.m.Unlock()
	switch err.(type) {
	case nil: // maintain current IP address
	// Try the next address if it was a quic.IdleTimeoutError
	// DupConnRegisterTunnelError needs to also receive a new ip address
	case connection.DupConnRegisterTunnelError,
		*quic.IdleTimeoutError:
		return true, nil
	// Network problems should be retried with new address immediately and report
	// as connectivity error
	case edgediscovery.DialError, *connection.EdgeQuicDialError:
		if f.retriesByConnIndex[connIndex] >= f.maxRetries {
			f.retriesByConnIndex[connIndex] = 0
			return true, NewConnectivityError(true)
		}
		f.retriesByConnIndex[connIndex]++
		return true, NewConnectivityError(false)
	default: // maintain current IP address
	}
	return false, nil
}

type EdgeTunnelServer struct {
	config            *TunnelConfig
	orchestrator      *orchestration.Orchestrator
	sessionManager    v3.SessionManager
	datagramMetrics   v3.Metrics
	edgeAddrHandler   EdgeAddrHandler
	edgeAddrs         *edgediscovery.Edge
	edgeBindAddr      net.IP
	reconnectCh       chan ReconnectSignal
	gracefulShutdownC <-chan struct{}
	tracker           *tunnelstate.ConnTracker

	connAwareLogger *ConnAwareLogger
}

type TunnelServer interface {
	Serve(ctx context.Context, connIndex uint8, protocolFallback *protocolFallback, connectedSignal *signal.Signal) error
}

func (e *EdgeTunnelServer) Serve(ctx context.Context, connIndex uint8, protocolFallback *protocolFallback, connectedSignal *signal.Signal) error {
	haConnections.Inc()
	defer haConnections.Dec()

	connectedFuse := newBooleanFuse()
	go func() {
		if connectedFuse.Await() {
			connectedSignal.Notify()
		}
	}()
	// Ensure the above goroutine will terminate if we return without connecting
	defer connectedFuse.Fuse(false)

	// Fetch IP address to associated connection index
	addr, err := e.edgeAddrs.GetAddr(int(connIndex))
	switch err.(type) {
	case nil: // no error
	case edgediscovery.ErrNoAddressesLeft:
		return err
	default:
		return err
	}

	logger := e.config.Log.With().
		Int(management.EventTypeKey, int(management.Cloudflared)).
		IPAddr(connection.LogFieldIPAddress, addr.UDP.IP).
		Uint8(connection.LogFieldConnIndex, connIndex).
		Logger()
	connLog := e.connAwareLogger.ReplaceLogger(&logger)

	// Each connection to keep its own copy of protocol, because individual connections might fallback
	// to another protocol when a particular metal doesn't support new protocol
	// Each connection can also have it's own IP version because individual connections might fallback
	// to another IP version.
	err, shouldFallbackProtocol := e.serveTunnel(
		ctx,
		connLog,
		addr,
		connIndex,
		connectedFuse,
		protocolFallback,
		protocolFallback.protocol,
	)

	// Check if the connection error was from an IP issue with the host or
	// establishing a connection to the edge and if so, rotate the IP address.
	shouldRotateEdgeIP, cErr := e.edgeAddrHandler.ShouldGetNewAddress(connIndex, err)
	if shouldRotateEdgeIP {
		// rotate IP, but forcing internal state to assign a new IP to connection index.
		if _, err := e.edgeAddrs.GetDifferentAddr(int(connIndex), true); err != nil {
			return err
		}

		// In addition, if it is a connectivity error, and we have exhausted the configurable maximum edge IPs to rotate,
		// then just fallback protocol on next iteration run.
		connectivityErr, ok := cErr.(*ConnectivityError)
		if ok {
			shouldFallbackProtocol = connectivityErr.HasReachedMaxRetries()
		}
	}

	// set connection has re-connecting and log the next retrying backoff
	duration, ok := protocolFallback.GetMaxBackoffDuration(ctx)
	if !ok {
		return err
	}
	e.config.Observer.SendReconnect(connIndex)
	connLog.Logger().Info().Msgf("Retrying connection in up to %s", duration)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.gracefulShutdownC:
		return nil
	case <-protocolFallback.BackoffTimer():
		// should we fallback protocol? If not, just return. Otherwise, set new protocol for next method call.
		if !shouldFallbackProtocol {
			return err
		}

		// If a single connection has connected with the current protocol, we know we know we don't have to fallback
		// to a different protocol.
		if e.tracker.HasConnectedWith(e.config.ProtocolSelector.Current()) {
			return err
		}

		if !selectNextProtocol(
			connLog.Logger(),
			protocolFallback,
			e.config.ProtocolSelector,
			err,
		) {
			return err
		}
	}

	return err
}

// protocolFallback is a wrapper around backoffHandler that will try fallback option when backoff reaches
// max retries
type protocolFallback struct {
	retry.BackoffHandler
	protocol   connection.Protocol
	inFallback bool
}

func (pf *protocolFallback) reset() {
	pf.ResetNow()
	pf.inFallback = false
}

func (pf *protocolFallback) fallback(fallback connection.Protocol) {
	pf.ResetNow()
	pf.protocol = fallback
	pf.inFallback = true
}

// selectNextProtocol picks connection protocol for the next retry iteration,
// returns true if it was able to pick the protocol, false if we are out of options and should stop retrying
func selectNextProtocol(
	connLog *zerolog.Logger,
	protocolBackoff *protocolFallback,
	selector connection.ProtocolSelector,
	cause error,
) bool {
	isQuicBroken := isQuicBroken(cause)
	_, hasFallback := selector.Fallback()

	if protocolBackoff.ReachedMaxRetries() || (hasFallback && isQuicBroken) {
		if isQuicBroken {
			connLog.Warn().Msg("If this log occurs persistently, and cloudflared is unable to connect to " +
				"Cloudflare Network with `quic` protocol, then most likely your machine/network is getting its egress " +
				"UDP to port 7844 (or others) blocked or dropped. Make sure to allow egress connectivity as per " +
				"https://developers.cloudflare.com/cloudflare-one/connections/connect-apps/configuration/ports-and-ips/\n" +
				"If you are using private routing to this Tunnel, then ICMP, UDP (and Private DNS Resolution) will not work " +
				"unless your cloudflared can connect with Cloudflare Network with `quic`.")
		}

		fallback, hasFallback := selector.Fallback()
		if !hasFallback {
			return false
		}
		// Already using fallback protocol, no point to retry
		if protocolBackoff.protocol == fallback {
			return false
		}
		connLog.Info().Msgf("Switching to fallback protocol %s", fallback)
		protocolBackoff.fallback(fallback)
	} else if !protocolBackoff.inFallback {
		current := selector.Current()
		if protocolBackoff.protocol != current {
			protocolBackoff.protocol = current
			connLog.Info().Msgf("Changing protocol to %s", current)
		}
	}
	return true
}

func isQuicBroken(cause error) bool {
	var idleTimeoutError *quic.IdleTimeoutError
	if errors.As(cause, &idleTimeoutError) {
		return true
	}

	var transportError *quic.TransportError
	if errors.As(cause, &transportError) && strings.Contains(cause.Error(), "operation not permitted") {
		return true
	}

	return false
}

// ServeTunnel runs a single tunnel connection, returns nil on graceful shutdown,
// on error returns a flag indicating if error can be retried
func (e *EdgeTunnelServer) serveTunnel(
	ctx context.Context,
	connLog *ConnAwareLogger,
	addr *allregions.EdgeAddr,
	connIndex uint8,
	fuse *booleanFuse,
	backoff *protocolFallback,
	protocol connection.Protocol,
) (err error, recoverable bool) {
	// Treat panics as recoverable errors
	defer func() {
		if r := recover(); r != nil {
			var ok bool
			err, ok = r.(error)
			if !ok {
				err = fmt.Errorf("ServeTunnel: %v", r)
			}
			err = errors.Wrapf(err, "stack trace: %s", string(debug.Stack()))
			recoverable = true
		}
	}()

	defer e.config.Observer.SendDisconnect(connIndex)
	err, recoverable = e.serveConnection(
		ctx,
		connLog,
		addr,
		connIndex,
		fuse,
		backoff,
		protocol,
	)

	if err != nil {
		switch err := err.(type) {
		case connection.DupConnRegisterTunnelError:
			connLog.ConnAwareLogger().Err(err).Msg("Unable to establish connection.")
			// don't retry this connection anymore, let supervisor pick a new address
			return err, false
		case connection.ServerRegisterTunnelError:
			connLog.ConnAwareLogger().Err(err).Msg("Register tunnel error from server side")
			// Don't send registration error return from server to Sentry. They are
			// logged on server side
			return err.Cause, !err.Permanent
		case *connection.EdgeQuicDialError:
			return err, false
		case ReconnectSignal:
			connLog.Logger().Info().
				IPAddr(connection.LogFieldIPAddress, addr.UDP.IP).
				Uint8(connection.LogFieldConnIndex, connIndex).
				Msgf("Restarting connection due to reconnect signal in %s", err.Delay)
			err.DelayBeforeReconnect()
			return err, true
		default:
			if err == context.Canceled {
				connLog.Logger().Debug().Err(err).Msgf("Serve tunnel error")
				return err, false
			}
			connLog.ConnAwareLogger().Err(err).Msgf("Serve tunnel error")
			_, permanent := err.(unrecoverableError)
			return err, !permanent
		}
	}
	return nil, false
}

func (e *EdgeTunnelServer) serveConnection(
	ctx context.Context,
	connLog *ConnAwareLogger,
	addr *allregions.EdgeAddr,
	connIndex uint8,
	fuse *booleanFuse,
	backoff *protocolFallback,
	protocol connection.Protocol,
) (err error, recoverable bool) {
	connectedFuse := &connectedFuse{
		fuse:    fuse,
		backoff: backoff,
	}
	controlStream := connection.NewControlStream(
		e.config.Observer,
		connectedFuse,
		e.config.NamedTunnel,
		connIndex,
		addr.UDP.IP,
		nil,
		e.config.RPCTimeout,
		e.gracefulShutdownC,
		e.config.GracePeriod,
		protocol,
	)

	switch protocol {
	case connection.QUIC:
		// nolint: gosec
		connOptions := e.config.connectionOptions(addr.UDP.String(), uint8(backoff.Retries()))
		// nolint: zerologlint
		connOptions.LogFields(connLog.Logger().Debug().Uint8(connection.LogFieldConnIndex, connIndex)).Msgf("Tunnel connection options")
		return e.serveQUIC(ctx,
			addr.UDP.AddrPort(),
			connLog,
			connOptions,
			controlStream,
			connIndex)

	case connection.HTTP2:
		edgeConn, err := edgediscovery.DialEdge(ctx, dialTimeout, e.config.EdgeTLSConfigs[protocol], addr.TCP, e.edgeBindAddr)
		if err != nil {
			connLog.ConnAwareLogger().Err(err).Msg("Unable to establish connection with Cloudflare edge")
			return err, true
		}

		// nolint: gosec
		connOptions := e.config.connectionOptions(edgeConn.LocalAddr().String(), uint8(backoff.Retries()))
		// nolint: zerologlint
		connOptions.LogFields(connLog.Logger().Debug().Uint8(connection.LogFieldConnIndex, connIndex)).Msgf("Tunnel connection options")
		if err := e.serveHTTP2(
			ctx,
			connLog,
			edgeConn,
			connOptions,
			controlStream,
			connIndex,
		); err != nil {
			return err, false
		}

	default:
		return fmt.Errorf("invalid protocol selected: %s", protocol), false
	}
	return
}

type unrecoverableError struct {
	err error
}

func (r unrecoverableError) Error() string {
	return r.err.Error()
}

func (e *EdgeTunnelServer) serveHTTP2(
	ctx context.Context,
	connLog *ConnAwareLogger,
	tlsServerConn net.Conn,
	connOptions *client.ConnectionOptionsSnapshot,
	controlStreamHandler connection.ControlStreamHandler,
	connIndex uint8,
) error {
	pqMode := connOptions.FeatureSnapshot.PostQuantum
	if pqMode == features.PostQuantumStrict {
		return unrecoverableError{errors.New("HTTP/2 transport does not support post-quantum")}
	}

	connLog.Logger().Debug().Msgf("Connecting via http2")
	h2conn := connection.NewHTTP2Connection(
		tlsServerConn,
		e.orchestrator,
		connOptions,
		e.config.Observer,
		connIndex,
		controlStreamHandler,
		e.config.Log,
	)

	errGroup, serveCtx := errgroup.WithContext(ctx)
	errGroup.Go(func() error {
		return h2conn.Serve(serveCtx)
	})

	errGroup.Go(func() error {
		err := listenReconnect(serveCtx, e.reconnectCh, e.gracefulShutdownC)
		if err != nil {
			// forcefully break the connection (this is only used for testing)
			// errgroup will return context canceled for the h2conn.Serve
			connLog.Logger().Debug().Msg("Forcefully breaking http2 connection")
		}
		return err
	})

	return errGroup.Wait()
}

func (e *EdgeTunnelServer) serveQUIC(
	ctx context.Context,
	edgeAddr netip.AddrPort,
	connLogger *ConnAwareLogger,
	connOptions *client.ConnectionOptionsSnapshot,
	controlStreamHandler connection.ControlStreamHandler,
	connIndex uint8,
) (err error, recoverable bool) {
	tlsConfig := e.config.EdgeTLSConfigs[connection.QUIC]

	pqMode := connOptions.FeatureSnapshot.PostQuantum
	curvePref, err := curvePreference(pqMode, fips.IsFipsEnabled(), tlsConfig.CurvePreferences)
	if err != nil {
		return err, true
	}

	connLogger.Logger().Info().Msgf("Tunnel connection curve preferences: %v", curvePref)

	tlsConfig.CurvePreferences = curvePref

	// quic-go 0.44 increases the initial packet size to 1280 by default. That breaks anyone running tunnel through WARP
	// because WARP MTU is 1280.
	var initialPacketSize uint16 = 1252
	if edgeAddr.Addr().Is4() {
		initialPacketSize = 1232
	}

	quicConfig := &quic.Config{
		HandshakeIdleTimeout:       quicpogs.HandshakeIdleTimeout,
		MaxIdleTimeout:             quicpogs.MaxIdleTimeout,
		KeepAlivePeriod:            quicpogs.MaxIdlePingPeriod,
		MaxIncomingStreams:         quicpogs.MaxIncomingStreams,
		MaxIncomingUniStreams:      quicpogs.MaxIncomingStreams,
		EnableDatagrams:            true,
		Tracer:                     quicpogs.NewClientTracer(connLogger.Logger(), connIndex),
		DisablePathMTUDiscovery:    e.config.DisableQUICPathMTUDiscovery,
		MaxConnectionReceiveWindow: e.config.QUICConnectionLevelFlowControlLimit,
		MaxStreamReceiveWindow:     e.config.QUICStreamLevelFlowControlLimit,
		InitialPacketSize:          initialPacketSize,
	}

	// Dial the QUIC connection to the edge
	conn, err := connection.DialQuic(
		ctx,
		quicConfig,
		tlsConfig,
		edgeAddr,
		e.edgeBindAddr,
		connIndex,
		connLogger.Logger(),
	)
	if err != nil {
		connLogger.ConnAwareLogger().Err(err).Msgf("Failed to dial a quic connection")

		e.reportErrorToSentry(err, connOptions.FeatureSnapshot.PostQuantum)
		return err, true
	}

	var datagramSessionManager connection.DatagramSessionHandler
	if connOptions.FeatureSnapshot.DatagramVersion == features.DatagramV3 {
		datagramSessionManager = connection.NewDatagramV3Connection(
			ctx,
			conn,
			e.sessionManager,
			e.config.ICMPRouterServer,
			connIndex,
			e.datagramMetrics,
			connLogger.Logger(),
		)
	} else {
		datagramSessionManager = connection.NewDatagramV2Connection(
			ctx,
			conn,
			e.config.OriginDialerService,
			e.config.ICMPRouterServer,
			connIndex,
			e.config.RPCTimeout,
			e.config.WriteStreamTimeout,
			e.orchestrator.GetFlowLimiter(),
			connLogger.Logger(),
		)
	}

	// Wrap the [quic.Connection] as a TunnelConnection
	tunnelConn, err := connection.NewTunnelConnection(
		ctx,
		conn,
		connIndex,
		e.orchestrator,
		datagramSessionManager,
		controlStreamHandler,
		connOptions,
		e.config.RPCTimeout,
		e.config.WriteStreamTimeout,
		e.config.GracePeriod,
		connLogger.Logger(),
	)
	if err != nil {
		connLogger.ConnAwareLogger().Err(err).Msgf("Failed to create new tunnel connection")
		return err, true
	}

	// Serve the TunnelConnection
	errGroup, serveCtx := errgroup.WithContext(ctx)
	errGroup.Go(func() error {
		err := tunnelConn.Serve(serveCtx)
		if err != nil {
			connLogger.ConnAwareLogger().Err(err).Msg("Failed to serve tunnel connection")
		}
		return err
	})

	errGroup.Go(func() error {
		err := listenReconnect(serveCtx, e.reconnectCh, e.gracefulShutdownC)
		if err != nil {
			// forcefully break the connection (this is only used for testing)
			// errgroup will return context canceled for the tunnelConn.Serve
			connLogger.Logger().Debug().Msg("Forcefully breaking tunnel connection")
		}
		return err
	})

	return errGroup.Wait(), false
}

// The reportErrorToSentry is an helper function that handles
// verifies if an error should be reported to Sentry.
func (e *EdgeTunnelServer) reportErrorToSentry(err error, pqMode features.PostQuantumMode) {
	dialErr, ok := err.(*connection.EdgeQuicDialError)
	if ok {
		// The TransportError provides an Unwrap function however
		// the err MAY not always be set
		transportErr, ok := dialErr.Cause.(*quic.TransportError)
		if ok &&
			transportErr.ErrorCode.IsCryptoError() &&
			fips.IsFipsEnabled() &&
			pqMode == features.PostQuantumStrict {
			// Only report to Sentry when using FIPS, PQ,
			// and the error is a Crypto error reported by
			// an EdgeQuicDialError
			sentry.CaptureException(err)
		}
	}
}

func listenReconnect(ctx context.Context, reconnectCh <-chan ReconnectSignal, gracefulShutdownCh <-chan struct{}) error {
	select {
	case reconnect := <-reconnectCh:
		return reconnect
	case <-gracefulShutdownCh:
		return nil
	case <-ctx.Done():
		return nil
	}
}

type connectedFuse struct {
	fuse    *booleanFuse
	backoff *protocolFallback
}

func (cf *connectedFuse) Connected() {
	cf.fuse.Fuse(true)
	cf.backoff.reset()
}

func (cf *connectedFuse) IsConnected() bool {
	return cf.fuse.Value()
}
