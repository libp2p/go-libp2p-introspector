package introspector

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/libp2p/go-eventbus"
	"hash/fnv"

	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/event"
	"github.com/libp2p/go-libp2p-core/introspection"
	introspection_pb "github.com/libp2p/go-libp2p-core/introspection/pb"
	"github.com/libp2p/go-libp2p-core/peer"

	"github.com/gogo/protobuf/proto"
	"github.com/gogo/protobuf/types"
	"github.com/gorilla/websocket"
	logging "github.com/ipfs/go-log"
	"github.com/multiformats/go-multiaddr"
)

var (
	jsType     = reflect.TypeOf(new(event.JSString)).Elem()
	peerIdType = reflect.TypeOf(new(peer.ID)).Elem()
	timeType   = reflect.TypeOf(new(time.Time)).Elem()
	maddrType  = reflect.TypeOf(new(multiaddr.Multiaddr)).Elem()
)

var logger = logging.Logger("introspection-server")
var upgrader = websocket.Upgrader{}

const (
	writeDeadline  = 10 * time.Second
	stateMsgPeriod = 2 * time.Second
	keepStaleData  = 120 * time.Second

	maxClientOutgoingBufferSize = 64
)

type newConnReq struct {
	conn *websocket.Conn
	done chan struct{}
}

type closeConnReq struct {
	ch   *connHandler
	done chan struct{}
}

type getEventTypesReq struct {
	resp chan []*introspection_pb.EventType
}

type connStateChangeReq struct {
	ch       *connHandler
	newState connectionState
}

type sendDataReq struct {
	ch       *connHandler
	dataType introspection_pb.ClientSignal_DataSource
}

type WsServer struct {
	// state initialized by constructor
	ctx          context.Context
	cancel       context.CancelFunc
	closeWg      sync.WaitGroup
	introspector introspection.Introspector
	config       *WsServerConfig
	server       *http.Server
	eventSub     event.Subscription

	// state managed in the eventloop
	newConnCh            chan *newConnReq
	closeConnCh          chan *closeConnReq
	connHandlerStates    map[*connHandler]connectionState
	connStateChangeReqCh chan *connStateChangeReq
	sendDataCh           chan *sendDataReq

	knownEvtProps map[reflect.Type]introspection_pb.EventType

	evalForTest chan func()

	// state managed by locking
	lk        sync.RWMutex
	listeners []net.Listener
	isClosed  bool
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
	if introspector == nil || config == nil {
		return nil, errors.New("none of introspector, event-bus OR config can be nil")
	}
	mux := http.NewServeMux()

	srv := &WsServer{
		server: &http.Server{Handler: mux},
		config: config,

		introspector: introspector,

		connHandlerStates:    make(map[*connHandler]connectionState),
		newConnCh:            make(chan *newConnReq),
		closeConnCh:          make(chan *closeConnReq),
		connStateChangeReqCh: make(chan *connStateChangeReq),
		sendDataCh:           make(chan *sendDataReq),
		knownEvtProps:        make(map[reflect.Type]introspection_pb.EventType),
		evalForTest:          make(chan func()),
	}
	srv.ctx, srv.cancel = context.WithCancel(context.Background())

	// register introspection handler
	mux.HandleFunc("/introspect", srv.wsUpgrader())
	return srv, nil
}

// Start starts this WS server.
func (s *WsServer) Start(bus event.Bus) error {
	s.lk.Lock()
	defer s.lk.Unlock()

	if len(s.listeners) > 0 {
		return errors.New("failed to start WS server: already started")
	}

	if len(s.config.ListenAddrs) == 0 {
		return errors.New("failed to start WS server: no listen addresses supplied")
	}

	sub, err := bus.Subscribe(event.WildcardSubscriptionType, eventbus.BufSize(256))
	if err != nil {
		return fmt.Errorf("failed to susbcribe for events with WildcardSubscriptionType")
	}
	s.eventSub = sub

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

	// start the periodic event outgoingLoop
	s.closeWg.Add(1)
	go s.eventLoop()

	return nil
}

// Close closes a WS introspection server.
func (s *WsServer) Close() error {
	s.lk.Lock()
	defer s.lk.Unlock()

	if s.isClosed {
		return nil
	}

	// Close the server, which in turn closes all listeners.
	if err := s.server.Close(); err != nil {
		return err
	}

	// cancel the context and wait for all go-routines to shut down
	s.cancel()
	s.closeWg.Wait()

	// close our subscription to the events
	if err := s.eventSub.Close(); err != nil {
		logger.Errorf("error while trying to close eventbus subscription, err=%s", err)
	}

	s.listeners = nil
	s.connHandlerStates = nil
	s.isClosed = true
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

func (sv *WsServer) wsUpgrader() http.HandlerFunc {
	return func(w http.ResponseWriter, rq *http.Request) {
		upgrader.CheckOrigin = func(rq *http.Request) bool { return true }
		wsConn, err := upgrader.Upgrade(w, rq, nil)
		if err != nil {
			logger.Errorf("upgrade to websocket failed, err: %s", err)
			return
		}

		done := make(chan struct{}, 1)
		select {
		case sv.newConnCh <- &newConnReq{wsConn, done}:
		case <-sv.ctx.Done():
			wsConn.Close()
			return
		}

		select {
		case <-done:
		case <-sv.ctx.Done():
			wsConn.Close()
			return
		}
	}
}

func (sv *WsServer) eventLoop() {
	var stateMessageTimer *time.Timer
	var stateTimerCh <-chan time.Time
	stMsgTmrRunning := false

	eventSubCh := sv.eventSub.Out()

	defer func() {
		if stMsgTmrRunning && !stateMessageTimer.Stop() {
			<-stateMessageTimer.C
		}
		sv.closeWg.Done()
	}()

	for {
		select {
		case <-stateTimerCh:
			stateBz, err := sv.fetchStateBinary()
			if err != nil {
				logger.Errorf("failed to fetch state bytes for periodic push, err=%s", err)
				continue
			}
			sv.broadcast(stateBz)
			stateMessageTimer.Reset(stateMsgPeriod)

		case req := <-sv.connStateChangeReqCh:
			currState := sv.connHandlerStates[req.ch]
			sv.connHandlerStates[req.ch] = req.newState

			if currState == paused && req.newState == running {
				sv.sendRuntimeMessage(req.ch)
				sv.sendStateMessage(req.ch)
			}

		case sendDataReq := <-sv.sendDataCh:
			switch sendDataReq.dataType {
			case introspection_pb.ClientSignal_STATE:
				sv.sendStateMessage(sendDataReq.ch)
			case introspection_pb.ClientSignal_RUNTIME:
				sv.sendRuntimeMessage(sendDataReq.ch)
			}

		case newConnReq := <-sv.newConnCh:
			handler := newConnHandler(sv.ctx, sv, newConnReq.conn)
			sv.connHandlerStates[handler] = running
			// if this was the first connection, start the periodic state push
			if len(sv.connHandlerStates) == 1 {
				stateMessageTimer = time.NewTimer(stateMsgPeriod)
				stateTimerCh = stateMessageTimer.C
				stMsgTmrRunning = true
			}

			// schedule runtime followed by state message on the connection
			sv.sendRuntimeMessage(handler)
			sv.sendStateMessage(handler)

			sv.closeWg.Add(1)
			go func() {
				defer sv.closeWg.Done()
				handler.run()
				select {
				case sv.closeConnCh <- &closeConnReq{handler, newConnReq.done}:
				case <-sv.ctx.Done():
					return
				}
			}()

		case closeReq := <-sv.closeConnCh:
			closeReq.ch.conn.Close()
			delete(sv.connHandlerStates, closeReq.ch)

			// shut down the periodic state push
			if len(sv.connHandlerStates) == 0 {
				// stop periodic state push
				if stMsgTmrRunning && !stateMessageTimer.Stop() {
					<-stateMessageTimer.C
				}
				stMsgTmrRunning = false
				stateTimerCh = nil
				stateMessageTimer = nil
			}

			closeReq.done <- struct{}{}

		case evt, more := <-eventSubCh:
			if !more {
				eventSubCh = nil
				continue
			}
			if len(sv.connHandlerStates) == 0 {
				continue
			}

			// will only send the name property if we've seen the event before, otherwise
			// will send props for all fields
			if err := sv.broadcastEvent(evt); err != nil {
				logger.Errorf("error while broadcasting event, err=%s", err)
			}

		case fnc := <-sv.evalForTest:
			fnc()

		case <-sv.ctx.Done():
			return
		}
	}

}

func (sv *WsServer) sendMessageToConn(ch *connHandler, msg []byte) {
	select {
	case ch.outgoingChan <- msg:
	default:
		logger.Warnf("dropping outgoing message to %s as buffer is full", ch.conn.RemoteAddr())
	}
}

func (sv *WsServer) sendRuntimeMessage(ch *connHandler) {
	rt, err := sv.fetchRuntimeBinary()
	if err != nil {
		logger.Errorf("failed to fetch runtime message, err=%s", err)
	} else {
		sv.sendMessageToConn(ch, rt)
	}
}

func (sv *WsServer) sendStateMessage(ch *connHandler) {
	st, err := sv.fetchStateBinary()
	if err != nil {
		logger.Errorf("failed to fetch full state, err=%s", err)
	} else {
		sv.sendMessageToConn(ch, st)
	}
}

func (sv *WsServer) broadcastEvent(evt interface{}) error {
	js, err := json.Marshal(evt)
	if err != nil {
		return fmt.Errorf("failed to marshal event to json, err=%s", err)
	}

	key := reflect.TypeOf(evt)
	var evtType *introspection_pb.EventType
	_, ok := sv.knownEvtProps[key]
	if ok {
		// just send the name if we've already seen the event before
		evtType = &introspection_pb.EventType{Name: sv.knownEvtProps[key].Name}
	} else {
		// compute props, cache and send
		evtType, err = getEventProperties(evt)
		if err != nil {
			return fmt.Errorf("failed to get properties of event, err=%s", err)
		}
		sv.knownEvtProps[key] = *evtType
	}

	ts, _ := types.TimestampProto(time.Now())

	evtMessage := &introspection_pb.ProtocolDataPacket{
		Version: introspection.ProtoVersionPb,
		Message: &introspection_pb.ProtocolDataPacket_Event{
			Event: &introspection_pb.Event{
				Type:    evtType,
				Ts:      ts,
				Content: string(js),
			},
		},
	}

	bz, err := proto.Marshal(evtMessage)
	if err != nil {
		return fmt.Errorf("failed to marshal event proto, err=%s", err)
	}

	cbz, err := checkSumedMessageForClient(bz)
	if err != nil {
		return fmt.Errorf("failed to generate checksumed event message, err=%s", err)
	}

	sv.broadcast(cbz)
	return nil
}

func (sv *WsServer) broadcast(msg []byte) {
	for c, _ := range sv.connHandlerStates {
		ch := c
		if sv.connHandlerStates[ch] != running {
			continue
		}

		sv.sendMessageToConn(ch, msg)
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
	return checkSumedMessageForClient(bz)
}

func (sv *WsServer) fetchRuntimeBinary() ([]byte, error) {
	rt, err := sv.introspector.FetchRuntime()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch runtime mesage, err=%s", err)
	}
	rt.SendStateIntervalMs = uint32(stateMsgPeriod.Milliseconds())
	rt.KeepStaleDataMs = uint32(keepStaleData.Milliseconds())

	evtProps := make([]*introspection_pb.EventType, 0, len(sv.knownEvtProps))
	for k := range sv.knownEvtProps {
		v := sv.knownEvtProps[k]
		evtProps = append(evtProps, &v)
	}
	rt.EventTypes = evtProps

	rtMsg := &introspection_pb.ProtocolDataPacket{
		Version: introspection.ProtoVersionPb,
		Message: &introspection_pb.ProtocolDataPacket_Runtime{Runtime: rt},
	}

	bz, err := proto.Marshal(rtMsg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal runtime proto to binary, err=%s", err)
	}
	return checkSumedMessageForClient(bz)
}

func checkSumedMessageForClient(protoMsg []byte) ([]byte, error) {
	bz := make([]byte, 12+len(protoMsg))

	f := fnv.New32a()
	n, err := f.Write(protoMsg)
	if err != nil {
		return nil, fmt.Errorf("failed creating fnc hash digest, err=%s", err)
	}
	if n != len(protoMsg) {
		return nil, fmt.Errorf("failed to write all bytes of the proto message to the hash digest")
	}

	binary.LittleEndian.PutUint32(bz[0:4], introspection.ProtoVersion)
	binary.LittleEndian.PutUint32(bz[4:8], f.Sum32())
	binary.LittleEndian.PutUint32(bz[8:12], uint32(len(protoMsg)))
	copy(bz[12:], protoMsg)

	return bz, nil
}

func getEventProperties(evt interface{}) (*introspection_pb.EventType, error) {
	eventType := reflect.TypeOf(evt)

	if eventType.Kind() != reflect.Struct {
		return nil, errors.New("event type must be a struct")
	}

	re := &introspection_pb.EventType{}
	re.Name = eventType.Name()
	re.PropertyTypes = make([]*introspection_pb.EventType_EventProperty, 0, eventType.NumField())

	for i := 0; i < eventType.NumField(); i++ {
		prop := &introspection_pb.EventType_EventProperty{}
		fld := eventType.Field(i)
		prop.Name = fld.Name

		fldType := fld.Type

		if fldType.Kind() == reflect.Array || fldType.Kind() == reflect.Slice {
			prop.HasMultiple = true
			fldType = fld.Type.Elem()
		}

		switch fldType {
		case jsType:
			prop.Type = introspection_pb.EventType_EventProperty_JSON
		case peerIdType:
			prop.Type = introspection_pb.EventType_EventProperty_PEERID
		case maddrType:
			prop.Type = introspection_pb.EventType_EventProperty_MULTIADDR
		case timeType:
			prop.Type = introspection_pb.EventType_EventProperty_TIME
		default:
			switch fldType.Kind() {
			case reflect.String:
				prop.Type = introspection_pb.EventType_EventProperty_STRING
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
				reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32,
				reflect.Uint64, reflect.Float32, reflect.Float64:
				prop.Type = introspection_pb.EventType_EventProperty_NUMBER
			default:
				prop.Type = introspection_pb.EventType_EventProperty_JSON
			}
		}

		re.PropertyTypes = append(re.PropertyTypes, prop)
	}

	return re, nil
}
