package lb

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/sony/gobreaker"
)

// TcpProxy manages a TCP listener and its connections
type TcpProxy struct {
	listener     net.Listener
	pool         *BackendPool
	listenerName string
	wg           sync.WaitGroup // Tracks active connections
}

// NewTcpProxy creates a new, un-started TCP proxy
func NewTcpProxy(listenerCfg *ListenerConfig, lb *LoadBalancer) (*TcpProxy, error) {
	pool, ok := lb.l4Pools[listenerCfg.Name]
	if !ok {
		return nil, errors.New("no L4 pool found for TCP listener: " + listenerCfg.Name)
	}

	listener, err := net.Listen("tcp", listenerCfg.ListenAddr)
	if err != nil {
		return nil, err
	}

	slog.Info("TCP listener configured", "name", listenerCfg.Name, "addr", listenerCfg.ListenAddr)
	return &TcpProxy{
		listener:     listener,
		pool:         pool,
		listenerName: listenerCfg.Name,
	}, nil
}

// Run starts the listener's accept loop
func (p *TcpProxy) Run() {
	slog.Info("TCP listener running", "name", p.listenerName, "addr", p.listener.Addr())
	for {
		clientConn, err := p.listener.Accept()
		if err != nil {
			// Check if the error is due to the listener being closed
			if errors.Is(err, net.ErrClosed) {
				slog.Info("TCP listener shutting down", "name", p.listenerName)
				return
			}
			slog.Warn("Failed to accept TCP connection", "name", p.listenerName, "error", err)
			continue
		}

		// Add to WaitGroup before spawning the goroutine
		p.wg.Add(1)
		go p.handleTcpConnection(clientConn)
	}
}

// Shutdown gracefully stops the TCP proxy
func (p *TcpProxy) Shutdown() {
	slog.Info("Closing TCP listener", "name", p.listenerName)
	p.listener.Close() // This will break the Accept() loop in Run()
}

// Wait blocks until all active TCP connections are closed
func (p *TcpProxy) Wait() {
	p.wg.Wait()
	slog.Info("All TCP connections closed", "name", p.listenerName)
}

// handleTcpConnection selects a backend and proxies the TCP connection
func (p *TcpProxy) handleTcpConnection(clientConn net.Conn) {
	defer p.wg.Done() // Decrement WaitGroup when connection is done
	defer clientConn.Close()

	clientIP := clientConn.RemoteAddr().String()
	logger := slog.Default().With(
		"protocol", "tcp",
		"listener", p.listenerName,
		"client_ip", clientIP,
	)

	backend := p.pool.GetNextBackend(nil, clientIP) // Pass nil for *http.Request
	if backend == nil {
		logger.Warn("No healthy backend found for TCP connection")
		return
	}

	logger = logger.With("backend_addr", backend.Addr)

	// Use circuit breaker if available
	if backend.cb != nil {
		_, err := backend.cb.Execute(func() (interface{}, error) {
			// Proxy the connection
			return nil, p.proxyTcp(logger, clientConn, backend)
		})
		if err != nil {
			if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
				logger.Warn("Request blocked by circuit breaker", "state", backend.cb.State().String())
			} else {
				logger.Warn("Circuit breaker failure recorded", "error", err)
			}
			return // Don't proxy if circuit is open or call failed
		}
	} else {
		// No circuit breaker, just proxy
		if err := p.proxyTcp(logger, clientConn, backend); err != nil {
			logger.Warn("TCP proxy error", "error", err)
		}
	}
}

// proxyTcp performs the bi-directional copy
func (p *TcpProxy) proxyTcp(logger *slog.Logger, clientConn net.Conn, backend *Backend) error {
	backendConn, err := net.Dial("tcp", backend.Addr)
	if err != nil {
		logger.Warn("Failed to connect to TCP backend", "error", err)
		return err
	}
	defer backendConn.Close()

	logger.Info("Proxying TCP connection")

	backend.IncrementConnections()
	defer backend.DecrementConnections()

	var wg sync.WaitGroup
	wg.Add(2)

	// Copy client -> backend
	go func() {
		defer wg.Done()
		defer backendConn.Close()
		_, err := io.Copy(backendConn, clientConn)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Warn("TCP copy client->backend error", "error", err)
		}
	}()

	// Copy backend -> client
	go func() {
		defer wg.Done()
		defer clientConn.Close()
		_, err := io.Copy(clientConn, backendConn)
		if err != nil && !errors.Is(err, net.ErrClosed) {
			logger.Warn("TCP copy backend->client error", "error", err)
		}
	}()

	wg.Wait()
	logger.Debug("TCP proxy connection closed")
	return nil
}
