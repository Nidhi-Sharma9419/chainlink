package plugins

import (
	"errors"
	"fmt"
	"net"
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/net/context"

	"github.com/smartcontractkit/chainlink/v2/core/logger"
)

type PromServer struct {
	port        int
	srvr        *http.Server
	tcpListener *net.TCPListener
	lggr        logger.Logger

	handler http.Handler
}

type PromServerOpt func(*PromServer)

func WithRegistry(r *prometheus.Registry) PromServerOpt {
	return func(s *PromServer) {
		s.handler = promhttp.HandlerFor(r, promhttp.HandlerOpts{})
	}
}

func NewPromServer(port int, lggr logger.Logger, opts ...PromServerOpt) *PromServer {

	s := &PromServer{
		port: port,
		lggr: lggr,
		srvr: &http.Server{},

		handler: promhttp.Handler(),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Start start HTTP server on specified port to handle metrics requests
func (p *PromServer) Start() error {
	err := p.setupListener()
	if err != nil {
		return err
	}

	http.Handle("/metrics", p.handler)

	go func() {
		err := p.srvr.Serve(p.tcpListener)
		if errors.Is(err, net.ErrClosed) {
			// ErrClose is expected on gracefully shutdown
			p.lggr.Warnf("%s closed", p.Name())
		} else {
			p.lggr.Errorf("%s: %s", p.Name(), err)
		}
	}()
	return nil
}

// Close shutdowns down the underlying HTTP server. See [http.Server.Close] for details
func (p *PromServer) Close() error {
	return p.srvr.Shutdown(context.Background())
}

// Name of the server
func (p *PromServer) Name() string {
	return fmt.Sprintf("%s-prom-server", p.lggr.Name())
}

func (p *PromServer) Port() int {
	// always safe to cast because we explicitly have a tcp listener
	// doesn't seem to be direct access to Port without the addr casting
	return p.tcpListener.Addr().(*net.TCPAddr).Port

}

// setupListener creates explicit listener so that we can resolve `:0` port, which is needed for testing
// if we didn't need the resolved addr, or could pick a static port we could use p.srvr.ListenAndServer
func (p *PromServer) setupListener() error {

	l, err := net.ListenTCP("tcp", &net.TCPAddr{
		Port: p.port,
	})
	if err != nil {
		return err
	}

	p.tcpListener = l
	return nil
}
