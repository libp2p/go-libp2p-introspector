package introspector

import (
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/net/context"
	"net"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/introspection"
	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/protocol"
	"github.com/libp2p/go-libp2p-core/test"

	"github.com/gogo/protobuf/proto"
	"github.com/gorilla/websocket"
	"github.com/libp2p/go-eventbus"
	"github.com/multiformats/go-multiaddr"
	"github.com/stretchr/testify/require"
)

type TestEvent struct {
	a int
	b int
}

type TE2 struct {
	m int
}

type TE3 struct {
	a string
	b int
}

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

			if err := server.Start(eventbus.NewBus()); err != nil {
				t.Fatalf("failed to start ws server: %s", err)
			}

			if err := server.Start(eventbus.NewBus()); err == nil {
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
	require.NoError(t, server.Start(eventbus.NewBus()))
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
		return getNConns(server) == nConns
	}, 10*time.Second, 1*time.Second)

	// should get runtime followed by state message on all 3 conns
	for i := 0; i < nConns; i++ {
		pd1, err := fetchProtocolWrapper(t, conns[i])
		require.NoError(t, err)
		require.NotNil(t, pd1.GetRuntime())
		require.Nil(t, pd1.GetState())
		pd2, err := fetchProtocolWrapper(t, conns[i])
		require.NoError(t, err)
		require.NotNil(t, pd2.GetState())
		require.Nil(t, pd2.GetRuntime())
	}

	// get periodic state messages on all connections -> atleast 3
	doneCh := make(chan struct{}, nConns)

	var lk sync.Mutex
	var fetchError error

	for i := 0; i < nConns; i++ {
		go func(i int) {
			count := 0
			for {
				pd1, err := fetchProtocolWrapper(t, conns[i])
				if err != nil {
					lk.Lock()
					fetchError = err
					lk.Unlock()
					return
				}
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
		lk.Lock()
		require.NoError(t, fetchError)
		lk.Unlock()
		count++
		if count == nConns {
			return
		}
	}
}

func TestEventsBroadcast(t *testing.T) {
	bus := eventbus.NewBus()
	e1, err := bus.Emitter(new(event.EvtPeerProtocolsUpdated))
	require.NoError(t, err)
	e2, err := bus.Emitter(new(event.EvtPeerIdentificationCompleted))
	require.NoError(t, err)

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
	require.NoError(t, server.Start(bus))
	defer func() {
		err := server.Close()
		require.NoError(t, err)
	}()

	// Test events broadcast
	var conns []*websocket.Conn

	for i := 0; i < nConns; i++ {
		conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
		require.NoError(t, err)
		defer conn.Close()
		conns = append(conns, conn)
	}

	// assert conn handlers
	require.Eventually(t, func() bool {
		return getNConns(server) == nConns
	}, 10*time.Second, 1*time.Second)

	// drain initial runtime & state messages from them
	for i := 0; i < nConns; i++ {
		pd1, err := fetchProtocolWrapper(t, conns[i])
		require.NoError(t, err)
		require.NotNil(t, pd1.GetRuntime())
		require.Nil(t, pd1.GetState())
		pd2, err := fetchProtocolWrapper(t, conns[i])
		require.NoError(t, err)
		require.NotNil(t, pd2.GetState())
		require.Nil(t, pd2.GetRuntime())
	}

	// emit two event and see them on all handlers
	pid := test.RandPeerIDFatal(t)
	ev1 := event.EvtPeerProtocolsUpdated{Peer: pid, Added: []protocol.ID{"P1"},
		Removed: []protocol.ID{"P2"}}
	ev2 := event.EvtPeerIdentificationCompleted{pid}
	require.NoError(t, e1.Emit(ev1))
	require.NoError(t, e2.Emit(ev2))

	var lk sync.Mutex
	var fetchError error
	doneCh := make(chan struct{}, nConns)

	for i := 0; i < nConns; i++ {
		go func(i int) {
			gotev1 := false
			gotev2 := false
			for {
				pd1, err := fetchProtocolWrapper(t, conns[i])
				if err != nil {
					lk.Lock()
					fetchError = err
					lk.Unlock()
					return
				}
				if pd1.GetState() == nil && pd1.GetRuntime() == nil && pd1.GetEvent() != nil {
					ev := pd1.GetEvent()
					switch ev.Type.Name {
					case reflect.TypeOf(new(event.EvtPeerProtocolsUpdated)).Elem().Name():
						gotev1 = true

						evt := &event.EvtPeerProtocolsUpdated{}
						require.NoError(t, json.Unmarshal([]byte(ev.Content), evt))
						require.Equal(t, ev1, *evt)

					case reflect.TypeOf(new(event.EvtPeerIdentificationCompleted)).Elem().Name():
						gotev2 = true
						evt := &event.EvtPeerIdentificationCompleted{}
						require.NoError(t, json.Unmarshal([]byte(ev.Content), evt))
						require.Equal(t, ev2, *evt)
					default:
						lk.Lock()
						fetchError = errors.New("invalid event type")
						lk.Unlock()
						return
					}

				}
				if gotev1 && gotev2 {
					doneCh <- struct{}{}
					return
				}
			}
		}(i)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	count := 0
	select {
	case <-ctx.Done():
		t.Fatal("failed to get events")
	case <-doneCh:
		lk.Lock()
		require.NoError(t, fetchError)
		lk.Unlock()
		count++
		if count == nConns {
			return
		}
	}

}

func TestRuntimeAndEvent(t *testing.T) {
	bus := eventbus.NewBus()
	em1, err := bus.Emitter(new(TestEvent))
	require.NoError(t, err)

	addr := "localhost:9999"
	// create a ws server
	introspector := NewDefaultIntrospector()
	config := &WsServerConfig{
		ListenAddrs: []string{addr},
	}
	server, err := NewWsServer(introspector, config)
	require.NoError(t, err)

	// start the server
	require.NoError(t, server.Start(bus))
	defer func() {
		err := server.Close()
		require.NoError(t, err)
	}()

	// make a conn
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn.Close()

	// assert conn handlers are created
	require.Eventually(t, func() bool {
		return getNConns(server) == 1
	}, 10*time.Second, 1*time.Second)

	// drain state and runtime
	pd1, err := fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.NotNil(t, pd1.GetRuntime())
	require.Nil(t, pd1.GetState())
	pd2, err := fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.NotNil(t, pd2.GetState())
	require.Nil(t, pd2.GetRuntime())

	// emit event
	em1.Emit(TestEvent{})

	// assert event state
	require.Eventually(t, func() bool {
		done := make(chan struct{})
		var tk map[reflect.Type]introspection_pb.EventType

		server.evalForTest <- func() {
			tk = server.knownEvtProps
			done <- struct{}{}
		}
		<-done

		_, ok := tk[reflect.TypeOf(new(TestEvent)).Elem()]
		return len(tk) == 1 && ok

	}, 10*time.Second, 1*time.Second)

	// now make a second conn so we get info about the first event in the runtime
	conn2, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn2.Close()

	var rt *introspection_pb.Runtime
	require.Eventually(t, func() bool {
		pd1, err := fetchProtocolWrapper(t, conn2)
		rt = pd1.GetRuntime()
		return err == nil && pd1.GetRuntime() != nil
	}, 10*time.Second, 1*time.Second)

	require.Len(t, rt.EventTypes, 1)
	require.Equal(t, reflect.TypeOf(new(TestEvent)).Elem().Name(), rt.EventTypes[0].Name)

	// and emitting a new event gets us the actual information
	var evt2 *introspection_pb.Event
	em2, err := bus.Emitter(new(TE2))
	require.NoError(t, err)
	require.NoError(t, em2.Emit(TE2{}))
	require.Eventually(t, func() bool {
		pd1, err := fetchProtocolWrapper(t, conn2)
		evt2 = pd1.GetEvent()
		return err == nil && evt2 != nil
	}, 10*time.Second, 1*time.Second)

	require.Len(t, evt2.Type.PropertyTypes, 1)
	require.Equal(t, reflect.TypeOf(new(TE2)).Elem().Name(), evt2.Type.Name)

	// emit another event and wait for it
	em3, err := bus.Emitter(new(TE3))
	require.NoError(t, err)
	require.NoError(t, em3.Emit(TE3{}))
	require.Eventually(t, func() bool {
		pd1, err := fetchProtocolWrapper(t, conn2)
		evt2 = pd1.GetEvent()
		return err == nil && evt2 != nil
	}, 10*time.Second, 1*time.Second)

	// now make another connection so we get runtime message with info about all three events
	conn3, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn3.Close()
	require.Eventually(t, func() bool {
		pd1, err := fetchProtocolWrapper(t, conn3)
		rt = pd1.GetRuntime()
		return err == nil && pd1.GetRuntime() != nil
	}, 10*time.Second, 1*time.Second)

	m := make(map[string]*introspection_pb.EventType)
	require.Len(t, rt.EventTypes, 3)
	for _, e := range rt.EventTypes {
		ec := e
		m[ec.Name] = ec
	}
	require.Len(t, m, 3)
}

func TestEventMessageHasProperties(t *testing.T) {

	bus := eventbus.NewBus()
	addr := "localhost:9999"
	// create a ws server

	introspector := NewDefaultIntrospector()
	config := &WsServerConfig{
		ListenAddrs: []string{addr},
	}
	server, err := NewWsServer(introspector, config)
	require.NoError(t, err)

	// start the server
	require.NoError(t, server.Start(bus))
	defer func() {
		err := server.Close()
		require.NoError(t, err)
	}()

	// make a conn
	conn, _, err := websocket.DefaultDialer.Dial(fmt.Sprintf("ws://%s/introspect", addr), nil)
	require.NoError(t, err)
	defer conn.Close()

	// drain state and runtime
	pd1, err := fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.NotNil(t, pd1.GetRuntime())
	require.Nil(t, pd1.GetState())
	pd2, err := fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.NotNil(t, pd2.GetState())
	require.Nil(t, pd2.GetRuntime())

	// first event has eventype
	e1, err := bus.Emitter(new(TestEvent))
	require.NoError(t, err)
	require.NoError(t, e1.Emit(TestEvent{}))
	pd, err := fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.Nil(t, pd.GetRuntime())
	require.Nil(t, pd.GetState())
	evt := pd.GetEvent()
	require.NotNil(t, evt)
	require.Equal(t, reflect.TypeOf(new(TestEvent)).Elem().Name(),
		evt.Type.Name)
	require.NotEmpty(t, evt.Type.PropertyTypes)
	require.Len(t, evt.Type.PropertyTypes, 2)

	// second ONLY has name
	require.NoError(t, e1.Emit(TestEvent{}))
	pd, err = fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.Nil(t, pd.GetRuntime())
	require.Nil(t, pd.GetState())
	evt = pd.GetEvent()
	require.NotNil(t, evt)
	require.Equal(t, reflect.TypeOf(new(TestEvent)).Elem().Name(),
		evt.Type.Name)
	require.Empty(t, evt.Type.PropertyTypes)

	// first event has eventype
	e2, err := bus.Emitter(new(TE2))
	require.NoError(t, err)
	require.NoError(t, e2.Emit(TE2{}))
	pd, err = fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.Nil(t, pd.GetRuntime())
	require.Nil(t, pd.GetState())
	evt = pd.GetEvent()
	require.NotNil(t, evt)
	require.Equal(t, reflect.TypeOf(new(TE2)).Elem().Name(),
		evt.Type.Name)
	require.NotEmpty(t, evt.Type.PropertyTypes)
	require.Len(t, evt.Type.PropertyTypes, 1)

	// second ONLY has name
	require.NoError(t, e2.Emit(TE2{}))
	pd, err = fetchProtocolWrapper(t, conn)
	require.NoError(t, err)
	require.Nil(t, pd.GetRuntime())
	require.Nil(t, pd.GetState())
	evt = pd.GetEvent()
	require.NotNil(t, evt)
	require.Equal(t, reflect.TypeOf(new(TE2)).Elem().Name(),
		evt.Type.Name)
	require.Empty(t, evt.Type.PropertyTypes)

	// assert internal state
	done := make(chan struct{}, 1)
	var tmap map[reflect.Type]introspection_pb.EventType
	server.evalForTest <- func() {
		tmap = server.knownEvtProps
		done <- struct{}{}
	}
	<-done

	require.Len(t, tmap, 2)
	t1 := tmap[reflect.TypeOf(new(TestEvent)).Elem()]
	require.Len(t, t1.PropertyTypes, 2)
	t2 := tmap[reflect.TypeOf(new(TE2)).Elem()]
	require.Len(t, t2.PropertyTypes, 1)
}

func getNConns(server *WsServer) int {
	var chsLen int
	doneChan := make(chan struct{}, 1)

	server.evalForTest <- func() {
		chsLen = len(server.connHandlerStates)
		doneChan <- struct{}{}
	}

	<-doneChan
	return chsLen
}

func sendMessage(t *testing.T, cl *introspection_pb.ClientSignal, conn *websocket.Conn) {
	bz, err := proto.Marshal(cl)
	require.NoError(t, err)

	err = conn.WriteMessage(websocket.BinaryMessage, bz)
	require.NoError(t, err)
}

func fetchProtocolWrapper(t *testing.T, conn *websocket.Conn) (*introspection_pb.ProtocolDataPacket, error) {
	_, msg, err := conn.ReadMessage()
	if err != nil {
		return nil, err
	}

	pd := &introspection_pb.ProtocolDataPacket{}
	if err := proto.Unmarshal(msg, pd); err != nil {
		return nil, err
	}

	if pd.Message == nil {
		return nil, errors.New("nil message recieved from server")
	}

	if introspection.ProtoVersion != pd.Version.Version {
		return nil, errors.New("incorrect proto version receieved from client")
	}

	return pd, nil
}

func TestGetEventProps(t *testing.T) {
	type EventA struct {
		pid  peer.ID
		pids []peer.ID

		maddr  multiaddr.Multiaddr
		maddrs []multiaddr.Multiaddr

		ti  time.Time
		tis []time.Time

		s  string
		ss []string

		num  int32
		nums []int64

		fallback  connHandler
		fallbacks []connHandler
	}

	prop, err := getEventProperties(EventA{})
	require.NoError(t, err)

	// name
	require.Equal(t, reflect.TypeOf(new(EventA)).Elem().Name(), prop.Name)

	// num props
	nameType := make(map[string]introspection_pb.EventType_EventProperty)

	for _, p := range prop.PropertyTypes {
		p2 := p
		nameType[p.Name] = *p2
	}

	require.Len(t, nameType, 12)

	pidN := nameType["pid"]
	require.Equal(t, introspection_pb.EventType_EventProperty_PEERID, pidN.Type)
	require.False(t, pidN.HasMultiple)

	pidsN := nameType["pids"]
	require.Equal(t, introspection_pb.EventType_EventProperty_PEERID, pidsN.Type)
	require.True(t, pidsN.HasMultiple)

	maddrN := nameType["maddr"]
	require.Equal(t, introspection_pb.EventType_EventProperty_MULTIADDR, maddrN.Type)
	require.False(t, maddrN.HasMultiple)

	maddrsN := nameType["maddrs"]
	require.Equal(t, introspection_pb.EventType_EventProperty_MULTIADDR, maddrsN.Type)
	require.True(t, maddrsN.HasMultiple)

	tiN := nameType["ti"]
	require.Equal(t, introspection_pb.EventType_EventProperty_TIME, tiN.Type)
	require.False(t, tiN.HasMultiple)

	tisN := nameType["tis"]
	require.Equal(t, introspection_pb.EventType_EventProperty_TIME, tisN.Type)
	require.True(t, tisN.HasMultiple)

	sN := nameType["s"]
	require.Equal(t, introspection_pb.EventType_EventProperty_STRING, sN.Type)
	require.False(t, sN.HasMultiple)

	sNs := nameType["ss"]
	require.Equal(t, introspection_pb.EventType_EventProperty_STRING, sNs.Type)
	require.True(t, sNs.HasMultiple)

	cn := nameType["fallback"]
	require.Equal(t, introspection_pb.EventType_EventProperty_JSON, cn.Type)
	require.False(t, cn.HasMultiple)

	cns := nameType["fallbacks"]
	require.Equal(t, introspection_pb.EventType_EventProperty_JSON, cns.Type)
	require.True(t, cns.HasMultiple)
}
