package dogstatsd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/0intro/dd-micro-agent/internal/metrics"
)

// maxPacketSize bounds a single UDP/UDS datagram. 64 KiB is the practical
// ceiling for a UDP payload and is plenty for batched statsd lines.
const maxPacketSize = 65536

// Server listens for DogStatsD traffic on UDP and/or a Unix datagram socket and
// feeds parsed samples to a sink.
type Server struct {
	port         int
	socket       string
	nonLocal     bool
	sink         func(metrics.Sample)
	serviceCheck func(metrics.ServiceCheck)
	event        func(metrics.Event)
	log          *slog.Logger
}

// Options configures a Server. Sink is required and is typically Aggregator.Add.
// ServiceCheck and Event are optional. When nil, _sc/_e datagrams are dropped.
type Options struct {
	Port         int
	Socket       string
	NonLocal     bool
	Sink         func(metrics.Sample)
	ServiceCheck func(metrics.ServiceCheck)
	Event        func(metrics.Event)
	Logger       *slog.Logger
}

// New returns a Server.
func New(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Server{
		port: o.Port, socket: o.Socket, nonLocal: o.NonLocal,
		sink: o.Sink, serviceCheck: o.ServiceCheck, event: o.Event, log: o.Logger,
	}
}

// Run binds the configured listeners and serves until ctx is cancelled. It
// returns an error only if no listener could be started.
func (s *Server) Run(ctx context.Context) error {
	var conns []net.PacketConn

	if s.port > 0 {
		ip := net.IPv4(127, 0, 0, 1)
		if s.nonLocal {
			ip = net.IPv4zero
		}
		addr := &net.UDPAddr{IP: ip, Port: s.port}
		conn, err := net.ListenUDP("udp", addr)
		if err != nil {
			return fmt.Errorf("dogstatsd udp: %w", err)
		}
		conns = append(conns, conn)
		s.log.Info("dogstatsd listening", "udp", addr.String())
	}

	if s.socket != "" {
		_ = os.Remove(s.socket) // a stale socket from a previous run blocks bind
		conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: s.socket, Net: "unixgram"})
		if err != nil {
			s.closeAll(conns)
			return fmt.Errorf("dogstatsd uds: %w", err)
		}
		conns = append(conns, conn)
		s.log.Info("dogstatsd listening", "uds", s.socket)
	}

	if len(conns) == 0 {
		return fmt.Errorf("dogstatsd: no listener configured (set dogstatsd_port or dogstatsd_socket)")
	}

	var wg sync.WaitGroup
	for _, conn := range conns {
		wg.Add(1)
		go func(c net.PacketConn) {
			defer wg.Done()
			s.readLoop(c)
		}(conn)
	}

	<-ctx.Done()
	s.closeAll(conns) // unblocks the read loops
	wg.Wait()
	if s.socket != "" {
		_ = os.Remove(s.socket)
	}
	return nil
}

func (s *Server) closeAll(conns []net.PacketConn) {
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) readLoop(conn net.PacketConn) {
	buf := make([]byte, maxPacketSize)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return // shutdown
			}
			// Keep listening through transient socket errors. A dead read
			// loop leaves the agent deaf on this socket until restart.
			s.log.Warn("dogstatsd read failed", "err", err)
			continue
		}
		s.handlePacket(buf[:n])
	}
}

// handlePacket splits a datagram into newline-separated lines and routes each: the
// _sc/_e prefixes are service checks and events, everything else is a metric sample.
func (s *Server) handlePacket(packet []byte) {
	for _, line := range bytes.Split(packet, []byte{'\n'}) {
		line = bytes.TrimRight(line, "\r")
		if len(line) == 0 {
			continue
		}
		str := string(line)
		switch {
		case strings.HasPrefix(str, "_sc|"):
			if sc, ok := parseServiceCheck(str); ok && s.serviceCheck != nil {
				s.serviceCheck(sc)
			} else if !ok {
				s.log.Debug("dropping malformed dogstatsd service check", "line", str)
			}
		case strings.HasPrefix(str, "_e{"):
			if ev, ok := parseEvent(str); ok && s.event != nil {
				s.event(ev)
			} else if !ok {
				s.log.Debug("dropping malformed dogstatsd event", "line", str)
			}
		default:
			if samples, ok := parse(str); ok {
				for _, sample := range samples {
					s.sink(sample)
				}
			} else {
				s.log.Debug("dropping malformed dogstatsd line", "line", str)
			}
		}
	}
}
