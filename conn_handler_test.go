package introspector

import (
	"fmt"
	"testing"
	"time"

	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestConnHandlerSignalling(t *testing.T) {
	addr := "localhost:9999"

	// create a ws server
	introspector := NewDefaultIntrospector()
	config := &WsServerConfig{
		ListenAddrs: []string{addr},
	}
	server, err := NewWsServer(introspector, config)
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
		server.lk.RLock()
		defer server.lk.RUnlock()

		for c, _ := range server.connHandlers {
			ch = c
		}

		return len(server.connHandlers) == 1
	}, 10*time.Second, 1*time.Second)

	require.True(t, ch.isRunning())

	// send a pause message and assert state
	cl := &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_PAUSE_PUSH_EMITTER}
	sendMessage(t, cl, conn)
	require.Eventually(t, func() bool {
		return !ch.isRunning()
	}, 10*time.Second, 1*time.Second)

	// send unpause and assert state
	cl = &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_UNPAUSE_PUSH_EMITTER}
	sendMessage(t, cl, conn)
	require.Eventually(t, func() bool {
		return ch.isRunning()
	}, 10*time.Second, 1*time.Second)

	// create one more connection and assert handler
	conn2, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn2.Close()

	var ch2 *connHandler
	require.Eventually(t, func() bool {
		server.lk.RLock()
		defer server.lk.RUnlock()

		for c, _ := range server.connHandlers {
			chc := c
			if chc != ch {
				ch2 = chc
			}
		}

		return len(server.connHandlers) == 2
	}, 10*time.Second, 1*time.Second)

	require.True(t, ch2.isRunning())

	// changing state of ch2 does not change state for ch1
	cl = &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_PAUSE_PUSH_EMITTER}
	sendMessage(t, cl, conn2)
	require.Eventually(t, func() bool {
		return ch.isRunning() && !ch2.isRunning()
	}, 10*time.Second, 1*time.Second)

	// test send runtime
	// first drain the first two messages sent at startup
	p1 := fetchProtocolWrapper(t, conn2)
	require.NotNil(t, p1.GetRuntime())
	p2 := fetchProtocolWrapper(t, conn2)
	require.NotNil(t, p2.GetState())

	// now send a send_runtime
	sendMessage(t, &introspection_pb.ClientSignal{Signal: introspection_pb.ClientSignal_SEND_DATA, DataSource: introspection_pb.ClientSignal_RUNTIME}, conn2)
	require.Eventually(t, func() bool {
		p1 := fetchProtocolWrapper(t, conn2)
		return p1.GetRuntime() != nil && p1.GetState() == nil
	}, 10*time.Second, 1*time.Second)
}
