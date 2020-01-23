package introspection

import (
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/imdario/mergo"
	coreit "github.com/libp2p/go-libp2p-core/introspection"
	"github.com/pkg/errors"
	"sync"
	"time"
)

var _ coreit.Introspector = (*DefaultIntrospector)(nil)

// DefaultIntrospector is a registry of subsystem data/metrics providers and also allows
// clients to inspect the system state by calling all the providers registered with it
type DefaultIntrospector struct {
	treeMu sync.RWMutex
	tree   *coreit.ProvidersTree
}

func NewDefaultIntrospector() *DefaultIntrospector {
	return &DefaultIntrospector{tree: &coreit.ProvidersTree{}}
}

func (d *DefaultIntrospector) RegisterProviders(provs *coreit.ProvidersTree) error {
	d.treeMu.Lock()
	defer d.treeMu.Unlock()

	if err := mergo.Merge(d.tree, provs); err != nil {
		return err
	}

	return nil
}

func (d *DefaultIntrospector) FetchCurrentState() (*coreit.State, error) {
	d.treeMu.RLock()
	defer d.treeMu.RUnlock()

	s := &coreit.State{}

	// subsystems
	s.Subsystems = &coreit.Subsystems{}

	// version
	s.Version = &coreit.Version{Number: coreit.ProtoVersion}

	// runtime
	// TODO Figure out how & where a runtime provider would be injected
	if d.tree.Runtime != nil {
		r, err := d.tree.Runtime.Get()
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch runtime info")
		}
		s.Runtime = r
	}

	// timestamps
	s.InstantTs = &timestamp.Timestamp{Seconds: time.Now().Unix()}
	// TODO Figure out the other two timestamp fields

	// connections
	if d.tree.Conn.List != nil {
		conns, err := d.tree.Conn.List()
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch connection list")
		}

		s.Subsystems.Connections = conns

		// streams
		if d.tree.Stream.List != nil {
			for _, c := range conns {
				s, err := d.tree.Stream.List(coreit.StreamListQuery{
					Type:   coreit.StreamListQueryTypeConn,
					ConnID: coreit.ConnID(c.Id),
				})
				if err != nil {
					return nil, errors.Wrap(err, "failed to fetch stream list")
				}

				c.Streams = &coreit.StreamList{}
				c.Streams.Streams = append(c.Streams.Streams, s...)
			}
		}
	}

	return nil, nil
}
