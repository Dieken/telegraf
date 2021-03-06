package syslog

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/influxdata/go-syslog/rfc5424"
	"github.com/influxdata/go-syslog/rfc5425"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	tlsConfig "github.com/influxdata/telegraf/internal/tls"
	"github.com/influxdata/telegraf/plugins/inputs"
)

const defaultReadTimeout = time.Millisecond * 500
const ipMaxPacketSize = 64 * 1024

// Syslog is a syslog plugin
type Syslog struct {
	tlsConfig.ServerConfig
	Address         string `toml:"server"`
	KeepAlivePeriod *internal.Duration
	ReadTimeout     *internal.Duration
	MaxConnections  int
	BestEffort      bool
	Separator       string `toml:"sdparam_separator"`

	now      func() time.Time
	lastTime time.Time

	mu sync.Mutex
	wg sync.WaitGroup
	io.Closer

	isStream      bool
	tcpListener   net.Listener
	tlsConfig     *tls.Config
	connections   map[string]net.Conn
	connectionsMu sync.Mutex

	udpListener net.PacketConn
}

var sampleConfig = `
  ## Specify an ip or hostname with port - eg., tcp://localhost:6514, tcp://10.0.0.1:6514
  ## Protocol, address and port to host the syslog receiver.
  ## If no host is specified, then localhost is used.
  ## If no port is specified, 6514 is used (RFC5425#section-4.1).
  server = "tcp://:6514"

  ## TLS Config
  # tls_allowed_cacerts = ["/etc/telegraf/ca.pem"]
  # tls_cert = "/etc/telegraf/cert.pem"
  # tls_key = "/etc/telegraf/key.pem"

  ## Period between keep alive probes.
  ## 0 disables keep alive probes.
  ## Defaults to the OS configuration.
  ## Only applies to stream sockets (e.g. TCP).
  # keep_alive_period = "5m"

  ## Maximum number of concurrent connections (default = 0).
  ## 0 means unlimited.
  ## Only applies to stream sockets (e.g. TCP).
  # max_connections = 1024

  ## Read timeout (default = 500ms).
  ## 0 means unlimited.
  # read_timeout = 500ms

  ## Whether to parse in best effort mode or not (default = false).
  ## By default best effort parsing is off.
  # best_effort = false

  ## Character to prepend to SD-PARAMs (default = "_").
  ## A syslog message can contain multiple parameters and multiple identifiers within structured data section.
  ## Eg., [id1 name1="val1" name2="val2"][id2 name1="val1" nameA="valA"]
  ## For each combination a field is created.
  ## Its name is created concatenating identifier, sdparam_separator, and parameter name.
  # sdparam_separator = "_"
`

// SampleConfig returns sample configuration message
func (s *Syslog) SampleConfig() string {
	return sampleConfig
}

// Description returns the plugin description
func (s *Syslog) Description() string {
	return "Accepts syslog messages per RFC5425"
}

// Gather ...
func (s *Syslog) Gather(_ telegraf.Accumulator) error {
	return nil
}

// Start starts the service.
func (s *Syslog) Start(acc telegraf.Accumulator) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	scheme, host, err := getAddressParts(s.Address)
	if err != nil {
		return err
	}
	s.Address = host

	switch scheme {
	case "tcp", "tcp4", "tcp6", "unix", "unixpacket":
		s.isStream = true
	case "udp", "udp4", "udp6", "ip", "ip4", "ip6", "unixgram":
		s.isStream = false
	default:
		return fmt.Errorf("unknown protocol '%s' in '%s'", scheme, s.Address)
	}

	if scheme == "unix" || scheme == "unixpacket" || scheme == "unixgram" {
		os.Remove(s.Address)
	}

	if s.isStream {
		l, err := net.Listen(scheme, s.Address)
		if err != nil {
			return err
		}
		s.Closer = l
		s.tcpListener = l
		s.tlsConfig, err = s.TLSConfig()
		if err != nil {
			return err
		}

		s.wg.Add(1)
		go s.listenStream(acc)
	} else {
		l, err := net.ListenPacket(scheme, s.Address)
		if err != nil {
			return err
		}
		s.Closer = l
		s.udpListener = l

		s.wg.Add(1)
		go s.listenPacket(acc)
	}

	if scheme == "unix" || scheme == "unixpacket" || scheme == "unixgram" {
		s.Closer = unixCloser{path: s.Address, closer: s.Closer}
	}

	return nil
}

// Stop cleans up all resources
func (s *Syslog) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.Closer != nil {
		s.Close()
	}
	s.wg.Wait()
}

// getAddressParts returns the address scheme and host
// it also sets defaults for them when missing
// when the input address does not specify the protocol it returns an error
func getAddressParts(a string) (string, string, error) {
	parts := strings.SplitN(a, "://", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("missing protocol within address '%s'", a)
	}

	u, _ := url.Parse(a)
	switch u.Scheme {
	case "unix", "unixpacket", "unixgram":
		return parts[0], parts[1], nil
	}

	var host string
	if u.Hostname() != "" {
		host = u.Hostname()
	}
	host += ":"
	if u.Port() == "" {
		host += "6514"
	} else {
		host += u.Port()
	}

	return u.Scheme, host, nil
}

func (s *Syslog) listenPacket(acc telegraf.Accumulator) {
	defer s.wg.Done()
	b := make([]byte, ipMaxPacketSize)
	p := rfc5424.NewParser()
	for {
		n, _, err := s.udpListener.ReadFrom(b)
		if err != nil {
			if !strings.HasSuffix(err.Error(), ": use of closed network connection") {
				acc.AddError(err)
			}
			break
		}

		if s.ReadTimeout != nil && s.ReadTimeout.Duration > 0 {
			s.udpListener.SetReadDeadline(time.Now().Add(s.ReadTimeout.Duration))
		}

		message, err := p.Parse(b[:n], &s.BestEffort)
		if message != nil {
			acc.AddFields("syslog", fields(*message, s), tags(*message), s.time())
		}
		if err != nil {
			acc.AddError(err)
		}
	}
}

func (s *Syslog) listenStream(acc telegraf.Accumulator) {
	defer s.wg.Done()

	s.connections = map[string]net.Conn{}

	for {
		conn, err := s.tcpListener.Accept()
		if err != nil {
			if !strings.HasSuffix(err.Error(), ": use of closed network connection") {
				acc.AddError(err)
			}
			break
		}
		var tcpConn, _ = conn.(*net.TCPConn)
		if s.tlsConfig != nil {
			conn = tls.Server(conn, s.tlsConfig)
		}

		s.connectionsMu.Lock()
		if s.MaxConnections > 0 && len(s.connections) >= s.MaxConnections {
			s.connectionsMu.Unlock()
			conn.Close()
			continue
		}
		s.connections[conn.RemoteAddr().String()] = conn
		s.connectionsMu.Unlock()

		if err := s.setKeepAlive(tcpConn); err != nil {
			acc.AddError(fmt.Errorf("unable to configure keep alive (%s): %s", s.Address, err))
		}

		go s.handle(conn, acc)
	}

	s.connectionsMu.Lock()
	for _, c := range s.connections {
		c.Close()
	}
	s.connectionsMu.Unlock()
}

func (s *Syslog) removeConnection(c net.Conn) {
	s.connectionsMu.Lock()
	delete(s.connections, c.RemoteAddr().String())
	s.connectionsMu.Unlock()
}

func (s *Syslog) handle(conn net.Conn, acc telegraf.Accumulator) {
	defer func() {
		s.removeConnection(conn)
		conn.Close()
	}()

	if s.ReadTimeout != nil && s.ReadTimeout.Duration > 0 {
		conn.SetReadDeadline(time.Now().Add(s.ReadTimeout.Duration))
	}

	var p *rfc5425.Parser
	if s.BestEffort {
		p = rfc5425.NewParser(conn, rfc5425.WithBestEffort())
	} else {
		p = rfc5425.NewParser(conn)
	}

	p.ParseExecuting(func(r *rfc5425.Result) {
		s.store(*r, acc)
	})
}

func (s *Syslog) setKeepAlive(c *net.TCPConn) error {
	if s.KeepAlivePeriod == nil {
		return nil
	}

	if s.KeepAlivePeriod.Duration == 0 {
		return c.SetKeepAlive(false)
	}
	if err := c.SetKeepAlive(true); err != nil {
		return err
	}
	return c.SetKeepAlivePeriod(s.KeepAlivePeriod.Duration)
}

func (s *Syslog) store(res rfc5425.Result, acc telegraf.Accumulator) {
	if res.Error != nil {
		acc.AddError(res.Error)
	}
	if res.MessageError != nil {
		acc.AddError(res.MessageError)
	}
	if res.Message != nil {
		msg := *res.Message
		acc.AddFields("syslog", fields(msg, s), tags(msg), s.time())
	}
}

func tags(msg rfc5424.SyslogMessage) map[string]string {
	ts := map[string]string{}

	// Not checking assuming a minimally valid message
	ts["severity"] = *msg.SeverityShortLevel()
	ts["facility"] = *msg.FacilityLevel()

	if msg.Hostname() != nil {
		ts["hostname"] = *msg.Hostname()
	}

	if msg.Appname() != nil {
		ts["appname"] = *msg.Appname()
	}

	return ts
}

func fields(msg rfc5424.SyslogMessage, s *Syslog) map[string]interface{} {
	// Not checking assuming a minimally valid message
	flds := map[string]interface{}{
		"version": msg.Version(),
	}
	flds["severity_code"] = int(*msg.Severity())
	flds["facility_code"] = int(*msg.Facility())

	if msg.Timestamp() != nil {
		flds["timestamp"] = (*msg.Timestamp()).UnixNano()
	}

	if msg.ProcID() != nil {
		flds["procid"] = *msg.ProcID()
	}

	if msg.MsgID() != nil {
		flds["msgid"] = *msg.MsgID()
	}

	if msg.Message() != nil {
		flds["message"] = *msg.Message()
	}

	if msg.StructuredData() != nil {
		for sdid, sdparams := range *msg.StructuredData() {
			if len(sdparams) == 0 {
				// When SD-ID does not have params we indicate its presence with a bool
				flds[sdid] = true
				continue
			}
			for name, value := range sdparams {
				// Using whitespace as separator since it is not allowed by the grammar within SDID
				flds[sdid+s.Separator+name] = value
			}
		}
	}

	return flds
}

type unixCloser struct {
	path   string
	closer io.Closer
}

func (uc unixCloser) Close() error {
	err := uc.closer.Close()
	os.Remove(uc.path) // ignore error
	return err
}

func (s *Syslog) time() time.Time {
	t := s.now()
	if t == s.lastTime {
		t = t.Add(time.Nanosecond)
	}
	s.lastTime = t
	return t
}

func getNanoNow() time.Time {
	return time.Unix(0, time.Now().UnixNano())
}

func init() {
	receiver := &Syslog{
		Address: ":6514",
		now:     getNanoNow,
		ReadTimeout: &internal.Duration{
			Duration: defaultReadTimeout,
		},
		Separator: "_",
	}

	inputs.Add("syslog", func() telegraf.Input { return receiver })
}
