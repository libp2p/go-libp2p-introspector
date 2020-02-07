package introspection

import (
	"fmt"
	"net"
	"strconv"
	"testing"

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

				err = conn.WriteMessage(websocket.BinaryMessage, []byte("foo"))
				require.NoError(err)

				_, _, err = conn.ReadMessage()
				require.NoError(err)
			}
		}
	}

	t.Run("single address", test([]string{"localhost:0"}))
	t.Run("multiple address", test([]string{"localhost:0", "localhost:9999"}))
}
