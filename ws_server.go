package introspector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/introspection"
	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log"
)

var logger = logging.Logger("introspection-server")
var upgrader = websocket.Upgrader{}

const writeDeadline = 10 * time.Second
const stateMsgPeriod = 2 * time.Second
const keepStaleData = 120 * time.Second

type WsServer struct {
	ctx          context.Context
	cancel       context.CancelFunc
	closeWg      sync.WaitGroup
	introspector introspection.Introspector
	config       *WsServerConfig
	server       *http.Server

	lk           sync.RWMutex
	listeners    []net.Listener
	connHandlers map[*connHandler]struct{}
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

	srv := &WsServer{
		server:       &http.Server{Handler: mux},
		config:       config,
		connHandlers: make(map[*connHandler]struct{}),
		introspector: introspector,
	}
	srv.ctx, srv.cancel = context.WithCancel(context.Background())
	mux.HandleFunc("/introspect", srv.wsUpgrader(introspector))
	return srv, nil
}

// Start starts this WS server.
func (s *WsServer) Start() error {
	s.lk.Lock()
	defer s.lk.Unlock()

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
			return fmt.Errorf("failed to start WS server: %wsvc", err)
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

	// start the periodic state push routine
	s.closeWg.Add(1)
	go s.periodicStateBroadcast()

	return nil
}

// Close closes a WS introspection server.
func (s *WsServer) Close() error {
	s.lk.Lock()
	defer s.lk.Unlock()

	if len(s.listeners) == 0 {
		// nothing to do.
		return nil
	}

	// Close the server, which in turn closes all listeners.
	if err := s.server.Close(); err != nil {
		return err
	}

	// cancel the context and wait for all go-routines to shut down
	s.cancel()
	s.closeWg.Wait()

	s.listeners = nil
	s.connHandlers = nil
	return nil
}

// ListenAddrs returns the actual listen addresses of this server.
func (s *WsServer) ListenAddrs() []string {
	s.lk.RLock()
	defer s.lk.RUnlock()

	res := make([]string, 0, len(s.listeners))
	for _, l := range s.listeners {
		res = append(res, l.Addr().String())
	}
	return res
}

func (sv *WsServer) wsUpgrader(introspector introspection.Introspector) http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		upgrader.CheckOrigin = func(rq *http.Request) bool { return true }
		wsConn, err := upgrader.Upgrade(w, rq, nil)
		if err != nil {
			logger.Errorf("upgrade to websocket failed, err: %s", err)
			return
		}
		ch := &connHandler{wsvc: sv, conn: wsConn}
		defer func() {
			wsConn.Close()
			sv.lk.Lock()
			delete(sv.connHandlers, ch)
			sv.lk.Unlock()
		}()

		sv.lk.Lock()
		sv.connHandlers[ch] = struct{}{}
		sv.lk.Unlock()

		// will block till run returns
		ch.run()
	}
}

func (sv *WsServer) periodicStateBroadcast() {
	stateMessageTickr := time.NewTicker(stateMsgPeriod)
	defer stateMessageTickr.Stop()
	defer sv.closeWg.Done()

	for {
		select {
		case <-stateMessageTickr.C:
			stateBz, err := sv.fetchStateBinary()
			if err != nil {
				logger.Errorf("failed to fetch state, err=%s", err)
				continue
			}
			sv.broadcast(stateBz)

		case <-sv.ctx.Done():
			return
		}
	}
}

func (sv *WsServer) broadcast(msg []byte) {
	var cs []*connHandler
	sv.lk.RLock()
	for c, _ := range sv.connHandlers {
		cs = append(cs, c)
	}
	sv.lk.RUnlock()

	if len(cs) == 0 {
		return
	}

	for _, c := range cs {
		ch := c
		if ch.isRunning() {
			go func(ch *connHandler) {
				if err := ch.sendBinaryMessage(msg); err != nil {
					// TODO Close this connection ?
					logger.Errorf("failed to send message to connection with %s, err=%s", ch.conn.RemoteAddr(), err)
				}
			}(ch)
		}
	}
}

func (sv *WsServer) fetchStateBinary() ([]byte, error) {
	st, err := sv.introspector.FetchFullState()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch state, err=%s", err)
	}

	stMsg := &introspection_pb.ProtocolDataPacket{
		Version: introspection.ProtoVersionPb,
		Message: &introspection_pb.ProtocolDataPacket_State{State: st},
	}

	bz, err := proto.Marshal(stMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal state proto to bianry, err=%s", err)
	}
	return bz, nil
}

func (sv *WsServer) fetchRuntimeBinary() ([]byte, error) {
	rt, err := sv.introspector.FetchRuntime()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch runtime mesage, err=%s", err)

	}
	rt.SendStateIntervalMs = uint32(stateMsgPeriod.Milliseconds())
	rt.KeepStaleDataMs = uint32(keepStaleData.Milliseconds())

	rtMsg := &introspection_pb.ProtocolDataPacket{
		Version: introspection.ProtoVersionPb,
		Message: &introspection_pb.ProtocolDataPacket_Runtime{Runtime: rt},
	}

	bz, err := proto.Marshal(rtMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal runtime proto to binary, err=%s", err)
	}
	return bz, nil
}
