package introspection

import (
	"sync"
	"time"

	"github.com/gogo/protobuf/types"
	"github.com/imdario/mergo"
	"github.com/libp2p/go-libp2p-core/introspect"
	introspectpb "github.com/libp2p/go-libp2p-core/introspect/pb"
	"github.com/pkg/errors"
)

var _ introspect.Introspector = (*DefaultIntrospector)(nil)

// DefaultIntrospector is a registry of subsystem data/metrics providers and also allows
// clients to inspect the system state by calling all the providers registered with it
type DefaultIntrospector struct {
	treeMu     sync.RWMutex
	tree       *introspect.DataProviders
	listenAddr []string
}

func NewDefaultIntrospector(listenAddr []string) *DefaultIntrospector {
	return &DefaultIntrospector{tree: &introspect.DataProviders{}, listenAddr: listenAddr}
}

func (d *DefaultIntrospector) RegisterDataProviders(provs *introspect.DataProviders) error {
	d.treeMu.Lock()
	defer d.treeMu.Unlock()

	if err := mergo.Merge(d.tree, provs); err != nil {
		return err
	}

	return nil
}

func (d *DefaultIntrospector) ListenAddrs() []string {
	return d.listenAddr
}

func (d *DefaultIntrospector) FetchFullState() (*introspectpb.State, error) {
	d.treeMu.RLock()
	defer d.treeMu.RUnlock()

	s := &introspectpb.State{}

	// subsystems
	s.Subsystems = &introspectpb.Subsystems{}

	// version
	s.Version = &introspectpb.Version{Number: introspect.ProtoVersion}

	// runtime
	if d.tree.Runtime != nil {
		r, err := d.tree.Runtime()
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch runtime info")
		}
		s.Runtime = r
	}

	// timestamps
	s.InstantTs = &types.Timestamp{Seconds: time.Now().Unix()}
	// TODO Figure out the other two timestamp fields

	// connections
	if d.tree.Connection != nil {
		conns, err := d.tree.Connection(introspect.ConnectionQueryParams{Output: introspect.QueryOutputFull})
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch connections")
		}
		// resolve streams on connection
		if d.tree.Stream != nil {
			for _, c := range conns {
				var sids []introspect.StreamID
				for _, s := range c.Streams.StreamIds {
					sids = append(sids, introspect.StreamID(s))
				}

				sl, err := d.tree.Stream(introspect.StreamQueryParams{introspect.QueryOutputFull, sids})
				if err != nil {
					return nil, errors.Wrap(err, "failed to fetch streams for connection")
				}
				c.Streams = sl
			}
		}
		s.Subsystems.Connections = conns
	}

	// traffic
	if d.tree.Traffic != nil {
		tr, err := d.tree.Traffic()
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch traffic")
		}
		s.Traffic = tr
	}

	return s, nil
}
