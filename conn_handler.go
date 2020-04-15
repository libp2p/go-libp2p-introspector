package introspector

import (
	"fmt"
	"sync"
	"time"

	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
)

type emitterState int

const (
	running emitterState = iota
	paused
)

type connHandler struct {
	wsvc *WsServer
	conn *websocket.Conn

	emitterStateLk sync.RWMutex
	es             emitterState

	// https://godoc.org/github.com/gorilla/websocket#hdr-Concurrency
	connWriteLk sync.Mutex
}

func (ch *connHandler) run() {
	// send initial Runtime message
	if err := ch.fetchAndSendRuntime(); err != nil {
		logger.Errorf("failed to fetch and send runtime message, err=%s", err)
		return
	}

	// send initial state message
	if err := ch.fetchAndSendState(); err != nil {
		logger.Errorf("failed to send initial state message, err=%s", err)
		return
	}

	// block and handle incoming messages
	ch.handleIncoming()
}

func (ch *connHandler) isRunning() bool {
	ch.emitterStateLk.RLock()
	defer ch.emitterStateLk.RUnlock()

	return ch.es == running
}

func (ch *connHandler) handleIncoming() {
	for {
		mt, message, err := ch.conn.ReadMessage()
		switch err.(type) {
		case nil:
		case *websocket.CloseError:
			logger.Warnf("connection closed: %s", err)
			return
		default:
			logger.Errorf("failed to read message from ws connection, err: %s", err)
			return
		}
		logger.Debugf("received message from ws connection, type: %d. recv: %s", mt, message)

		// unmarshal
		var clMsg introspection_pb.ClientSignal
		if err := proto.Unmarshal(message, &clMsg); err != nil {
			logger.Errorf("failed to read client message, err=%s", err)
			return
		}

		switch clMsg.Signal {
		case introspection_pb.ClientSignal_SEND_DATA:
			switch clMsg.DataSource {
			case introspection_pb.ClientSignal_STATE:
				if err := ch.fetchAndSendState(); err != nil {
					logger.Errorf("failed to fetch and send state; err=%s", err)
					return
				}
			case introspection_pb.ClientSignal_RUNTIME:
				if err := ch.fetchAndSendRuntime(); err != nil {
					logger.Errorf("failed to fetch and send runtime; err=%s", err)
					return
				}
			}
		case introspection_pb.ClientSignal_PAUSE_PUSH_EMITTER:
			ch.emitterStateLk.Lock()
			ch.es = paused
			ch.emitterStateLk.Unlock()
		case introspection_pb.ClientSignal_UNPAUSE_PUSH_EMITTER:
			var wasPaused bool
			ch.emitterStateLk.Lock()
			wasPaused = (ch.es == paused)
			ch.es = running
			ch.emitterStateLk.Unlock()

			// send a state message as emitter was paused earlier
			if wasPaused {
				if err := ch.fetchAndSendState(); err != nil {
					logger.Errorf("failed to fetch and send state; err=%s", err)
					return
				}
			}
		}
	}
}

func (ch *connHandler) fetchAndSendRuntime() error {
	bz, err := ch.wsvc.fetchRuntimeBinary()
	if err != nil {
		return fmt.Errorf("failed to fetch runtime, err=%s", err)
	}
	if err := ch.sendBinaryMessage(bz); err != nil {
		return fmt.Errorf("failed to send runtime message, err=%s", err)
	}
	return nil
}

func (ch *connHandler) fetchAndSendState() error {
	bz, err := ch.wsvc.fetchStateBinary()
	if err != nil {
		return fmt.Errorf("failed to fetch state, err=%s", err)
	}
	if err := ch.sendBinaryMessage(bz); err != nil {
		return fmt.Errorf("failed to send state message, err=%s", err)
	}
	return nil
}

func (ch *connHandler) sendBinaryMessage(bz []byte) error {
	ch.connWriteLk.Lock()
	defer ch.connWriteLk.Unlock()

	ch.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err := ch.conn.WriteMessage(websocket.BinaryMessage, bz); err != nil {
		return err
	}

	return nil
}
