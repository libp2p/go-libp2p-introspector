package introspection

import (
	"github.com/golang/protobuf/ptypes/timestamp"
	"github.com/imdario/mergo"
	coreit "github.com/libp2p/go-libp2p-core/introspection"
	"github.com/pkg/errors"
	"sync"
	"time"
)

var _ coreit.Introspector = (*ProviderRegistry)(nil)

// ProviderRegistry is a registry of subsystem data/metrics providers and also allows
// clients to inspect the system state by calling all the providers registered with it
type ProviderRegistry struct {
	treeMu sync.RWMutex
	tree   *coreit.ProvidersTree
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{tree: &coreit.ProvidersTree{}}
}

func (p *ProviderRegistry) RegisterProviders(provs *coreit.ProvidersTree) error {
	p.treeMu.Lock()
	defer p.treeMu.Unlock()

	if err := mergo.Merge(p.tree, provs); err != nil {
		return err
	}

	return nil
}

func (p *ProviderRegistry) FetchCurrentState() (*coreit.State, error) {
	p.treeMu.RLock()
	defer p.treeMu.RUnlock()

	s := &coreit.State{}

	// subsystems
	s.Subsystems = &coreit.Subsystems{}

	// version
	s.Version = &coreit.Version{Number: coreit.ProtoVersion}

	// runtime
	// TODO Figure out how & where a runtime provider would be injected
	if p.tree.Runtime != nil {
		r, err := p.tree.Runtime.Get()
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch runtime info")
		}
		s.Runtime = r
	}

	// timestamps
	s.InstantTs = &timestamp.Timestamp{Seconds: time.Now().Unix()}
	// TODO Figure out the other two timestamp fields

	// connections
	if p.tree.Conn.List != nil {
		conns, err := p.tree.Conn.List()
		if err != nil {
			return nil, errors.Wrap(err, "failed to fetch connection list")
		}

		s.Subsystems.Connections = conns

		// streams
		if p.tree.Stream.List != nil {
			for _, c := range conns {
				s, err := p.tree.Stream.List(coreit.StreamListQuery{
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
