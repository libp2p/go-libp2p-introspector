package introspector

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"

	"github.com/libp2p/go-libp2p-core/introspection"

	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log"
)

var logger = logging.Logger("introspection-server")
var upgrader = websocket.Upgrader{}

type WsServer struct {
	sync.RWMutex

	config    *WsServerConfig
	listeners []net.Listener
	server    *http.Server
	closeWg   sync.WaitGroup
}

var _ introspection.Endpoint = (*WsServer)(nil)

type WsServerConfig struct {
	ListenAddrs []string
}

// WsServerWithConfig returns a function compatible with the
// libp2p.Introspection constructor option, which when called, creates a
// WsServer with the supplied configuration.
func WsServerWithConfig(config *WsServerConfig) func(i introspection.Introspector) (introspection.Endpoint, error) {
	return func(i introspection.Introspector) (introspection.Endpoint, error) {
		return NewWsServer(i, config)
	}
}

// NewWsServer creates a WebSockets server to serve introspection data.
func NewWsServer(introspector introspection.Introspector, config *WsServerConfig) (*WsServer, error) {
	mux := http.NewServeMux()
	// introspection handler
	mux.HandleFunc("/introspect", wsUpgrader(introspector))

	srv := &WsServer{
		server: &http.Server{Handler: mux},
		config: config,
	}
	return srv, nil
}

// Start starts this WS server.
func (s *WsServer) Start() error {
	s.Lock()
	defer s.Unlock()

	if len(s.listeners) > 0 {
		return errors.New("failed to start WS server: already started")
	}

	if len(s.config.ListenAddrs) == 0 {
		return errors.New("failed to start WS server: no listen addresses supplied")
	}

	logger.Infof("WS introspection server starting, listening on %s", s.config.ListenAddrs)

	for _, addr := range s.config.ListenAddrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("failed to start WS server: %w", err)
		}

		s.closeWg.Add(1)
		go func() {
			defer s.closeWg.Done()
			if err := s.server.Serve(l); err != http.ErrServerClosed {
				logger.Errorf("failed to start WS server, err: %s", err)
			}
		}()

		s.listeners = append(s.listeners, l)
	}

	return nil
}

// Close closes a WS introspection server.
func (s *WsServer) Close() error {
	s.Lock()
	defer s.Unlock()

	if len(s.listeners) == 0 {
		// nothing to do.
		return nil
	}

	// Close the server, which in turn closes all listeners.
	if err := s.server.Close(); err != nil {
		return err
	}
	s.closeWg.Wait()

	s.listeners = nil
	return nil
}

// ListenAddrs returns the actual listen addresses of this server.
func (s *WsServer) ListenAddrs() []string {
	s.RLock()
	defer s.RUnlock()

	res := make([]string, 0, len(s.listeners))
	for _, l := range s.listeners {
		res = append(res, l.Addr().String())
	}
	return res
}

func wsUpgrader(introspector introspection.Introspector) http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		upgrader.CheckOrigin = func(rq *http.Request) bool { return true }
		wsConn, err := upgrader.Upgrade(w, rq, nil)
		if err != nil {
			logger.Errorf("upgrade to websocket failed, err: %s", err)
			return
		}
		defer wsConn.Close()

		ch := &connHandler{conn: wsConn, introspector: introspector}
		// will block till run returns
		ch.run()
	}
}
