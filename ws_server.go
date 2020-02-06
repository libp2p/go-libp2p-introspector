package introspection

import (
	"context"
	"net/http"
	"time"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/introspect"
)

var logger = logging.Logger("introspection-server")
var upgrader = websocket.Upgrader{}

// StartServer starts the ws introspection server with the given introspector
func StartServer(introspector introspect.Introspector) func() error {
	// introspect handler
	http.HandleFunc("/introspect", wsUpgrader(introspector))

	// start server
	srv := http.Server{
		// TODO Need a better strategy to select an address
		Addr: introspector.ListenAddrs()[0],
	}

	// start server
	go func() {
		if err := srv.ListenAndServe(); err != http.ErrServerClosed {
			logger.Errorf("failed to start server, err=%s", err)
		}
	}()

	logger.Infof("server starting, listening on %s", introspector.ListenAddrs()[0])

	return func() error {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		return srv.Shutdown(shutdownCtx)
	}
}

func wsUpgrader(introspector introspect.Introspector) http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		upgrader.CheckOrigin = func(rq *http.Request) bool { return true }
		wsConn, err := upgrader.Upgrade(w, rq, nil)
		if err != nil {
			logger.Errorf("upgrade to websocket failed, err: %s", err)
			return
		}
		defer wsConn.Close()

		for {
			// wait for client to ask for the state
			mt, message, err := wsConn.ReadMessage()
			if err != nil {
				logger.Errorf("failed to read message from ws connection, err: %s", err)
				return
			}
			logger.Debugf("received message from ws connection, type: %d. recv: %s", mt, message)

			// fetch the current state & marshal to bytes
			state, err := introspector.FetchFullState()
			if err != nil {
				logger.Errorf("failed to fetch current state in introspector, err: %s", err)
				return
			}

			bz, err := proto.Marshal(state)
			if err != nil {
				logger.Errorf("failed to marshal introspector state, err: %s", err)
				return
			}

			// send the response
			wsConn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err = wsConn.WriteMessage(websocket.BinaryMessage, bz); err != nil {
				logger.Errorf("failed to write response to ws connection, err: %s", err)
				return
			}
		}
	}
}
