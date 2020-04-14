package introspector

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/introspection"
	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
)

const writeDeadline = 10 * time.Second
const stateMsgPeriod = 2 * time.Second
const keepStaleData = 120 * time.Second

type emitterState int

const (
	running emitterState = iota
	paused
)

type connHandler struct {
	ctx    context.Context
	cancel context.CancelFunc

	conn         *websocket.Conn
	introspector introspection.Introspector

	lk sync.RWMutex
	es emitterState

	wg sync.WaitGroup
}

func (ch *connHandler) run() {
	ch.ctx, ch.cancel = context.WithCancel(context.Background())
	defer ch.cancel()

	// send initial Runtime message
	if err := ch.fetchAndSendRuntime(); err != nil {
		logger.Errorf("failed to send initial runtime message, err=%s", err)
		return
	}

	// send initial state message
	if err := ch.fetchAndSendState(); err != nil {
		logger.Errorf("failed to send initial state message, err=%s", err)
		return
	}

	// schedule periodic state messages
	ch.wg.Add(3)
	go ch.handleIncoming()
	go ch.periodicStatePush()
	go ch.eventsPush()

	// wait for all goroutines to complete
	ch.wg.Wait()
}

func (ch *connHandler) periodicStatePush() {
	stateMessageTickr := time.NewTicker(stateMsgPeriod)
	defer stateMessageTickr.Stop()
	defer ch.wg.Done()
	defer ch.cancel()

	for {
		select {
		case <-stateMessageTickr.C:
			var shouldSend bool
			ch.lk.RLock()
			shouldSend = (ch.es == running)
			ch.lk.RUnlock()

			if shouldSend {
				if err := ch.fetchAndSendState(); err != nil {
					logger.Errorf("failed to fetch & send state, err=%s", err)
					return
				}
			}

		case <-ch.ctx.Done():
			return
		}
	}
}

func (ch *connHandler) handleIncoming() {
	defer ch.wg.Done()
	defer ch.cancel()

	for {
		select {
		case <-ch.ctx.Done():
			return
		default:
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
				ch.lk.Lock()
				ch.es = paused
				ch.lk.Unlock()
			case introspection_pb.ClientSignal_UNPAUSE_PUSH_EMITTER:
				var wasPaused bool
				ch.lk.Lock()
				wasPaused = (ch.es == paused)
				ch.es = running
				ch.lk.Unlock()

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

}

func (ch *connHandler) eventsPush() {
	defer ch.wg.Done()
	defer ch.cancel()

	select {
	case <-ch.ctx.Done():
		return
	}
}

func (ch *connHandler) fetchAndSendState() error {
	st, err := ch.introspector.FetchFullState()
	if err != nil {
		return fmt.Errorf("failed to fetch state, err=%s", err)
	}

	stMsg := &introspection_pb.ProtocolDataPacket{
		Version: introspection.ProtoVersionPb,
		Message: &introspection_pb.ProtocolDataPacket_State{State: st},
	}

	if err := ch.sendMessage(stMsg); err != nil {
		return fmt.Errorf("failed to send state message to client, err=%s", err)
	}

	return nil
}

func (ch *connHandler) fetchAndSendRuntime() error {
	rt, err := ch.introspector.FetchRuntime()
	if err != nil {
		return fmt.Errorf("failed to fetch runtime mesage, err=%s", err)

	}
	rt.SendStateIntervalMs = uint32(stateMsgPeriod.Milliseconds())
	rt.KeepStaleDataMs = uint32(keepStaleData.Milliseconds())

	rtMsg := &introspection_pb.ProtocolDataPacket{
		Version: introspection.ProtoVersionPb,
		Message: &introspection_pb.ProtocolDataPacket_Runtime{Runtime: rt},
	}

	if err := ch.sendMessage(rtMsg); err != nil {
		return fmt.Errorf("failed to send runtime message, err=%s", err)
	}

	return nil
}

func (ch *connHandler) sendMessage(msg proto.Message) error {
	bz, err := proto.Marshal(msg)
	if err != nil {
		return err
	}

	ch.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
	if err = ch.conn.WriteMessage(websocket.BinaryMessage, bz); err != nil {
		return err
	}

	return nil
}
