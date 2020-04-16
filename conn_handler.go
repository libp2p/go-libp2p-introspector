package introspector

import (
	"context"
	"sync"
	"time"

	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
)

type connectionState int

const (
	running connectionState = iota
	paused
)

type connHandler struct {
	ctx    context.Context
	cancel context.CancelFunc

	ws   *WsServer
	conn *websocket.Conn

	outgoingChan chan []byte

	wg sync.WaitGroup
}

func newConnHandler(ctx context.Context, sv *WsServer, conn *websocket.Conn) *connHandler {
	ch := &connHandler{ws: sv, conn: conn, outgoingChan: make(chan []byte, maxClientOutgoingBufferSize)}
	ch.ctx, ch.cancel = context.WithCancel(ctx)
	return ch
}

func (ch *connHandler) run() {
	ch.wg.Add(2)
	go ch.outgoingLoop()
	go ch.handleIncoming()
	ch.wg.Wait()
}

func (ch *connHandler) outgoingLoop() {
	defer ch.wg.Done()
	for {
		select {
		case bz := <-ch.outgoingChan:
			ch.conn.SetWriteDeadline(time.Now().Add(writeDeadline))
			if err := ch.conn.WriteMessage(websocket.BinaryMessage, bz); err != nil {
				logger.Warnf("failed to send binary message to client with addr %s, err=%s", err)
				ch.cancel()
				return
			}

		case <-ch.ctx.Done():
			return
		}
	}
}

func (ch *connHandler) handleIncoming() {
	defer ch.wg.Done()
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
				ch.cancel()
				return
			default:
				logger.Warnf("failed to read message from ws connection, err: %s", err)
				ch.cancel()
				return
			}
			logger.Debugf("received message from ws connection, type: %d. recv: %s", mt, message)

			// unmarshal
			var clMsg introspection_pb.ClientSignal
			if err := proto.Unmarshal(message, &clMsg); err != nil {
				logger.Errorf("failed to read client message, err=%s", err)
				ch.cancel()
				return
			}

			switch clMsg.Signal {
			case introspection_pb.ClientSignal_SEND_DATA:
				switch clMsg.DataSource {
				case introspection_pb.ClientSignal_STATE:
					select {
					case ch.ws.sendDataCh <- &sendDataReq{ch, introspection_pb.ClientSignal_STATE}:
					case <-ch.ctx.Done():
						logger.Errorf("context cancelled while waiting to submit state send request")
						return
					}

				case introspection_pb.ClientSignal_RUNTIME:
					select {
					case ch.ws.sendDataCh <- &sendDataReq{ch, introspection_pb.ClientSignal_RUNTIME}:
					case <-ch.ctx.Done():
						logger.Errorf("context cancelled while waiting to submit runtime send request")
						return
					}
				}
			case introspection_pb.ClientSignal_PAUSE_PUSH_EMITTER:
				select {
				case ch.ws.connStateChangeReqCh <- &connStateChangeReq{ch, paused}:
				case <-ch.ctx.Done():
					logger.Errorf("context cancelled while waiting to submit conn pause request")
					return
				}
			case introspection_pb.ClientSignal_UNPAUSE_PUSH_EMITTER:
				select {
				case ch.ws.connStateChangeReqCh <- &connStateChangeReq{ch, running}:
				case <-ch.ctx.Done():
					logger.Errorf("context cancelled while waiting to submit conn unpause request")
					return
				}
			}
		}
	}

}
