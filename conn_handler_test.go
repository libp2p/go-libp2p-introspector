package introspector

import (
	"fmt"
	"testing"
	"time"

	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gorilla/websocket"
	"github.com/libp2p/go-eventbus"
	"github.com/stretchr/testify/require"
)

func TestConnHandlerSignalling(t *testing.T) {
	addr := "localhost:9999"

	// create a ws server
	introspector := NewDefaultIntrospector()
	config := &WsServerConfig{
		ListenAddrs: []string{addr},
	}
	server, err := NewWsServer(introspector, eventbus.NewBus(), config)
	require.NoError(t, err)

	// start the server
	require.NoError(t, server.Start())
	defer func() {
		err := server.Close()
		require.NoError(t, err)
	}()

	// connect to it and get a conn
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn.Close()

	// assert handler is running
	var ch *connHandler
	require.Eventually(t, func() bool {
		var chlen int
		es := paused
		done := make(chan struct{}, 1)

		server.evalForTest <- func() {
			for c, s := range server.connHandlerStates {
				ch = c
				es = s
			}
			chlen = len(server.connHandlerStates)
			done <- struct{}{}
		}
		<-done

		return chlen == 1 && es == running
	}, 10*time.Second, 1*time.Second)

	// send a pause message and assert state
	cl := &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_PAUSE_PUSH_EMITTER}
	sendMessage(t, cl, conn)
	require.Eventually(t, func() bool {
		es := running
		done := make(chan struct{}, 1)
		server.evalForTest <- func() {
			es = server.connHandlerStates[ch]
			done <- struct{}{}
		}
		<-done

		return es == paused
	}, 10*time.Second, 1*time.Second)

	// send unpause and assert state
	cl = &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_UNPAUSE_PUSH_EMITTER}
	sendMessage(t, cl, conn)
	require.Eventually(t, func() bool {
		es := paused
		done := make(chan struct{}, 1)
		server.evalForTest <- func() {
			es = server.connHandlerStates[ch]
			done <- struct{}{}
		}
		<-done

		return es == running
	}, 10*time.Second, 1*time.Second)

	// create one more connection and assert handler
	conn2, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn2.Close()

	var ch2 *connHandler
	require.Eventually(t, func() bool {
		var chlen int
		done := make(chan struct{}, 1)
		es := paused

		server.evalForTest <- func() {
			for c, s := range server.connHandlerStates {
				chc := c
				if chc != ch {
					ch2 = chc
					es = s
				}
			}

			chlen = len(server.connHandlerStates)
			done <- struct{}{}
		}
		<-done

		return chlen == 2 && es == running
	}, 10*time.Second, 1*time.Second)

	// changing state of ch2 does not change state for ch1
	cl = &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_PAUSE_PUSH_EMITTER}
	sendMessage(t, cl, conn2)
	require.Eventually(t, func() bool {
		es1 := running
		es2 := running
		done := make(chan struct{}, 1)
		server.evalForTest <- func() {
			es1 = server.connHandlerStates[ch]
			es2 = server.connHandlerStates[ch2]
			done <- struct{}{}
		}
		<-done

		return es1 == running && es2 == paused
	}, 10*time.Second, 1*time.Second)

	// test send runtime
	// first drain the first two messages sent at startup
	p1, err := fetchProtocolWrapper(t, conn2)
	require.NoError(t, err)
	require.NotNil(t, p1.GetRuntime())
	p2, err := fetchProtocolWrapper(t, conn2)
	require.NoError(t, err)
	require.NotNil(t, p2.GetState())

	// now send a send_runtime
	sendMessage(t, &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_SEND_DATA, DataSource: introspection_pb.ClientSignal_RUNTIME}, conn2)
	var fetcherr error
	require.Eventually(t, func() bool {
		p1, err := fetchProtocolWrapper(t, conn2)
		if err != nil {
			fetcherr = err
			return false
		}
		return p1.GetRuntime() != nil && p1.GetState() == nil
	}, 10*time.Second, 1*time.Second)
	require.NoError(t, fetcherr)

	//now send a send data
	sendMessage(t, &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_SEND_DATA, DataSource: introspection_pb.ClientSignal_STATE}, conn2)
	require.Eventually(t, func() bool {
		p1, err := fetchProtocolWrapper(t, conn2)
		if err != nil {
			fetcherr = err
			return false
		}
		return p1.GetState() != nil && p1.GetRuntime() == nil
	}, 10*time.Second, 1*time.Second)
	require.NoError(t, fetcherr)
}
