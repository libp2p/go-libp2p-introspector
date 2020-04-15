package introspector

import (
	"fmt"
	"golang.org/x/net/context"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p-core/introspection"
	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestIntrospectionServer(t *testing.T) {
	require := require.New(t)
	introspector := NewDefaultIntrospector()

	test := func(addrs []string) func(t *testing.T) {
		return func(t *testing.T) {
			config := &WsServerConfig{
				ListenAddrs: addrs,
			}
			server, err := NewWsServer(introspector, config)
			if err != nil {
				t.Fatalf("failed to construct ws server: %s", err)
			}

			if err := server.Start(); err != nil {
				t.Fatalf("failed to start ws server: %s", err)
			}

			if err := server.Start(); err == nil {
				t.Fatalf("expected to fail when starting server twice")
			}

			defer func() {
				err := server.Close()
				require.NoError(err)
			}()

			actualAddrs := server.ListenAddrs()
			require.Len(actualAddrs, len(addrs))

			for _, addr := range actualAddrs {
				_, p, err := net.SplitHostPort(addr)
				require.NoError(err)

				port, err := strconv.Atoi(p)
				require.NoError(err)

				require.Greater(port, 0)

				conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
				require.NoError(err)
				defer conn.Close()

				require.Equal(conn.RemoteAddr().String(), addr)

				sendMessage(t, &introspection_pb.ClientSignal{}, conn)

				_, _, err = conn.ReadMessage()
				require.NoError(err)
			}
		}
	}

	t.Run("single address", test([]string{"localhost:0"}))
	t.Run("multiple address", test([]string{"localhost:0", "localhost:9999"}))
}

func TestBroadcast(t *testing.T) {
	nConns := 3
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

	var conns []*websocket.Conn

	for i := 0; i < nConns; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
		require.NoError(t, err)
		defer conn.Close()
		conns = append(conns, conn)
	}

	// assert conn handlers
	require.Eventually(t, func() bool {
		server.lk.RLock()
		defer server.lk.RUnlock()
		return len(server.connHandlers) == nConns
	}, 10*time.Second, 1*time.Second)

	// should get runtime followed by state message on all 3 conns
	for i := 0; i < nConns; i++ {
		pd1 := fetchProtocolWrapper(t, conns[i])
		require.NotNil(t, pd1.GetRuntime())
		require.Nil(t, pd1.GetState())
		pd2 := fetchProtocolWrapper(t, conns[i])
		require.NotNil(t, pd2.GetState())
		require.Nil(t, pd2.GetRuntime())
	}

	// get periodic state messages on all connections -> atleast 3
	doneCh := make(chan struct{}, 3)

	for i := 0; i < nConns; i++ {
		go func(i int) {
			count := 0
			for {
				pd1 := fetchProtocolWrapper(t, conns[i])
				if pd1.GetState() != nil && pd1.GetRuntime() == nil {
					count++
				}
				if count == 3 {
					doneCh <- struct{}{}
					return
				}
			}
		}(i)
	}

	// should get ATLEAST 3 state broadcasts on each connection in 12 seconds
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()

	count := 0
	select {
	case <-ctx.Done():
		t.Fatal("failed to get atleast 3 state broadcasts on all three connections")
	case <-doneCh:
		count++
		if count == nConns {
			return
		}
	}
}

func sendMessage(t *testing.T, cl *introspection_pb.ClientSignal, conn *websocket.Conn) {
	bz, err := proto.Marshal(cl)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.BinaryMessage, bz)
	require.NoError(t, err)
}

func fetchProtocolWrapper(t *testing.T, conn *websocket.Conn) *introspection_pb.ProtocolDataPacket {
	_, msg, err := conn.ReadMessage()
	require.NoError(t, err)
	require.NotEmpty(t, msg)
	pd := &introspection_pb.ProtocolDataPacket{}
	require.NoError(t, proto.Unmarshal(msg, pd))
	require.NotNil(t, pd.Message)
	require.Equal(t, introspection.ProtoVersion, pd.Version.Version)
	return pd
}
