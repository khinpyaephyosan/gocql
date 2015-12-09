package gocql

import (
	"log"
	"net"
	"sync"
	"time"
)

type eventDeouncer struct {
	mu     sync.Mutex
	events []frame // TODO: possibly use a chan here

	callback func([]frame)
	quit     chan struct{}
}

func (e *eventDeouncer) flusher() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.mu.Lock()
			e.flush()
			e.mu.Unlock()
		case <-e.quit:
			return
		}
	}
}

// flush must be called with mu locked
func (e *eventDeouncer) flush() {
	if len(e.events) == 0 {
		return
	}

	// TODO: can this be done in a nicer way?
	events := make([]frame, len(e.events))
	copy(events, e.events)
	e.events = e.events[:0]

	go e.callback(events)
}

func (e *eventDeouncer) handleEvent(frame frame) {
	e.mu.Lock()

	const maxEvents = 100
	e.events = append(e.events, frame)
	// TODO: probably need a warning to track if this threshold is too low
	if len(e.events) > maxEvents {
		e.flush()
	}
	e.mu.Unlock()
}

func (s *Session) handleEvent(framer *framer) {
	// TODO(zariel): need to debounce events frames, and possible also events
	defer framerPool.Put(framer)

	frame, err := framer.parseFrame()
	if err != nil {
		// TODO: logger
		log.Printf("gocql: unable to parse event frame: %v\n", err)
		return
	}

	// TODO: handle medatadata events
	switch f := frame.(type) {
	case *schemaChangeKeyspace:
	case *schemaChangeFunction:
	case *schemaChangeTable:
	case *topologyChangeEventFrame:
		switch f.change {
		case "NEW_NODE":
			s.handleNewNode(f.host, f.port)
		case "REMOVED_NODE":
			s.handleRemovedNode(f.host, f.port)
		case "MOVED_NODE":
			// java-driver handles this, not mentioned in the spec
			// TODO(zariel): refresh token map
		}
	case *statusChangeEventFrame:
		// TODO(zariel): is it worth having 2 methods for these?
		switch f.change {
		case "UP":
			s.handleNodeUp(f.host, f.port)
		case "DOWN":
			s.handleNodeDown(f.host, f.port)
		}
	default:
		log.Printf("gocql: invalid event frame (%T): %v\n", f, f)
	}
}

func (s *Session) handleNewNode(host net.IP, port int) {
	// TODO(zariel): need to be able to filter discovered nodes
	if s.control == nil {
		return
	}

	hostInfo, err := s.control.fetchHostInfo(host, port)
	if err != nil {
		log.Printf("gocql: unable to fetch host info for %v: %v\n", host, err)
		return
	}

	// should this handle token moving?
	if !s.ring.addHostIfMissing(hostInfo) {
		s.handleNodeUp(host, port)
		return
	}

	s.pool.addHost(hostInfo)
}

func (s *Session) handleRemovedNode(ip net.IP, port int) {
	// we remove all nodes but only add ones which pass the filter
	addr := ip.String()
	s.pool.removeHost(addr)
	s.ring.removeHost(addr)
}

func (s *Session) handleNodeUp(ip net.IP, port int) {
	addr := ip.String()
	host := s.ring.getHost(addr)
	if host != nil {
		host.setState(NodeUp)
		s.pool.hostUp(host)
		return
	}

	// TODO: this could infinite loop
	s.handleNewNode(ip, port)
}

func (s *Session) handleNodeDown(ip net.IP, port int) {
	addr := ip.String()
	host := s.ring.getHost(addr)
	if host != nil {
		host.setState(NodeDown)
	}

	s.pool.hostDown(addr)
}
