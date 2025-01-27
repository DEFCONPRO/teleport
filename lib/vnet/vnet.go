// Teleport
// Copyright (C) 2024 Gravitational, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package vnet

import (
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"

	"github.com/gravitational/trace"
	"golang.org/x/sync/errgroup"
	"golang.zx2c4.com/wireguard/device"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/gravitational/teleport"
)

const (
	nicID                            = 1
	mtu                              = 1500
	tcpReceiveBufferSize             = 0 // 0 means a default will be used.
	maxInFlightTCPConnectionAttempts = 1024
)

// Config holds configuration parameters for the VNet.
type Config struct {
	// TUNDevice is the OS TUN virtual network interface.
	TUNDevice TUNDevice
	// IPv6Prefix is the IPv6 ULA prefix to use for all assigned VNet IP addresses.
	IPv6Prefix tcpip.Address
}

// CheckAndSetDefaults checks the config and sets defaults.
func (c *Config) CheckAndSetDefaults() error {
	if c.TUNDevice == nil {
		return trace.BadParameter("TUNdevice is required")
	}
	if c.IPv6Prefix.Len() != 16 || c.IPv6Prefix.AsSlice()[0] != 0xfd {
		return trace.BadParameter("IPv6Prefix must be an IPv6 ULA address")
	}
	return nil
}

// IPv6Prefix returns a Unique Local IPv6 Unicast Address which will be used as a 64-bit prefix for all v6 IP
// addresses in the VNet.
func IPv6Prefix() (tcpip.Address, error) {
	// |   8 bits   |  40 bits   |  16 bits  |          64 bits           |
	// +------------+------------+-----------+----------------------------+
	// | ULA Prefix | Global ID  | Subnet ID |        Interface ID        |
	// +------------+------------+-----------+----------------------------+
	// ULA Prefix is always 0xfd
	// Global ID is random bytes for the specific VNet instance
	// Subnet ID is always 0
	// Interface ID will be the IPv4 address prefixed with zeros.
	var bytes [16]byte
	bytes[0] = 0xfd
	if _, err := rand.Read(bytes[1:6]); err != nil {
		return tcpip.Address{}, trace.Wrap(err)
	}
	return tcpip.AddrFrom16(bytes), nil
}

// TUNDevice abstracts a virtual network TUN device.
type TUNDevice interface {
	// Write one or more packets to the device (without any additional headers).
	// On a successful write it returns the number of packets written. A nonzero
	// offset can be used to instruct the Device on where to begin writing from
	// each packet contained within the bufs slice.
	Write(bufs [][]byte, offset int) (int, error)

	// Read one or more packets from the Device (without any additional headers).
	// On a successful read it returns the number of packets read, and sets
	// packet lengths within the sizes slice. len(sizes) must be >= len(bufs).
	// A nonzero offset can be used to instruct the Device on where to begin
	// reading into each element of the bufs slice.
	Read(bufs [][]byte, sizes []int, offset int) (n int, err error)

	// BatchSize returns the preferred/max number of packets that can be read or
	// written in a single read/write call. BatchSize must not change over the
	// lifetime of a Device.
	BatchSize() int

	// Close stops the Device and closes the Event channel.
	Close() error
}

// Manager holds configuration and state for the VNet.
type Manager struct {
	// stack is the gVisor networking stack.
	stack *stack.Stack

	// tun is the OS TUN device. Incoming IP/L3 packets will be copied from here to [linkEndpoint], and
	// outgoing packets from [linkEndpoint] will be written here.
	tun TUNDevice

	// linkEndpoint is the gVisor-side endpoint that emulates the OS TUN device. All incoming IP/L3 packets
	// from the OS TUN device will be injected as inbound packets to this endpoint to be processed by the
	// gVisor netstack which ultimately calls the TCP or UDP protocol handler. When the protocol handler
	// writes packets to the gVisor stack to an address assigned to this endpoint, they will be written to
	// this endpoint, and then copied from this endpoint to the OS TUN device.
	linkEndpoint *channel.Endpoint

	// ipv6Prefix holds the 96-bit prefix that will be used for all IPv6 addresses assigned in the VNet.
	ipv6Prefix tcpip.Address

	// destroyed is a channel that will be closed when the VNet is in the process of being destroyed.
	// All goroutines should terminate quickly after either this is closed or the context passed to
	// [Manager.Run] is canceled.
	destroyed chan struct{}
	// wg is a [sync.WaitGroup] that keeps track of all running goroutines started by the [Manager].
	wg sync.WaitGroup

	// state holds all mutable state for the Manager, it is currently protect by a single RWMutex, this could
	// be optimized as necessary.
	state state

	slog *slog.Logger
}

type state struct {
	mu                   sync.RWMutex
	tcpHandlers          map[tcpip.Address]tcpHandler
	lastAssignedIPSuffix uint32
}

func newState() state {
	return state{
		tcpHandlers: make(map[tcpip.Address]tcpHandler),
		// Suffix 0 is reserved, suffix 1 is assigned to the NIC.
		lastAssignedIPSuffix: 1,
	}
}

// tcpConnector is a type of function that can be called to consume a TCP connection.
type tcpConnector func() (io.ReadWriteCloser, error)
type tcpHandler interface {
	handleTCP(context.Context, tcpConnector) error
}

// NewManager creates a new VNet manager with the given configuration and root context. It takes ownership of
// [cfg.TUNDevice] and will handle closing it before Run() returns. Call Run() on the returned manager to
// start the VNet.
func NewManager(cfg *Config) (*Manager, error) {
	if err := cfg.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	slog := slog.With(teleport.ComponentKey, "VNet")

	stack, linkEndpoint, err := createStack()
	if err != nil {
		return nil, trace.Wrap(err)
	}

	if err := installVnetRoutes(stack); err != nil {
		return nil, trace.Wrap(err)
	}

	m := &Manager{
		tun:          cfg.TUNDevice,
		stack:        stack,
		linkEndpoint: linkEndpoint,
		ipv6Prefix:   cfg.IPv6Prefix,
		destroyed:    make(chan struct{}),
		state:        newState(),
		slog:         slog,
	}

	tcpForwarder := tcp.NewForwarder(m.stack, tcpReceiveBufferSize, maxInFlightTCPConnectionAttempts, m.handleTCP)
	m.stack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	return m, nil
}

func createStack() (*stack.Stack, *channel.Endpoint, error) {
	netStack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})

	const (
		size     = 512
		linkAddr = ""
	)
	linkEndpoint := channel.New(size, mtu, linkAddr)
	if err := netStack.CreateNIC(nicID, linkEndpoint); err != nil {
		return nil, nil, trace.Errorf("creating VNet NIC: %s", err)
	}

	return netStack, linkEndpoint, nil
}

func installVnetRoutes(stack *stack.Stack) error {
	// Make the network stack pass all outbound IP packets to the NIC, regardless of destination IP address.
	ipv6Subnet, err := tcpip.NewSubnet(tcpip.AddrFrom16([16]byte{}), tcpip.MaskFromBytes(make([]byte, 16)))
	if err != nil {
		return trace.Wrap(err, "creating VNet IPv6 subnet")
	}
	stack.SetRouteTable([]tcpip.Route{{
		Destination: ipv6Subnet,
		NIC:         nicID,
	}})
	return nil
}

// Run starts the VNet. It blocks until [ctx] is canceled, at which point it closes the link endpoint, waits
// for all goroutines to terminate, and destroys the networking stack.
func (m *Manager) Run(ctx context.Context) error {
	m.slog.InfoContext(ctx, "Running Teleport VNet.", "ipv6_prefix", m.ipv6Prefix)

	ctx, cancel := context.WithCancel(ctx)

	allErrors := make(chan error, 2)
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		// Make sure to cancel the context in case this exits prematurely with a nil error.
		defer cancel()
		err := forwardBetweenTunAndNetstack(ctx, m.tun, m.linkEndpoint)
		allErrors <- err
		return err
	})
	g.Go(func() error {
		// When the context is canceled for any reason (the caller or one of the other concurrent tasks may
		// have canceled it) destroy everything and quit.
		<-ctx.Done()

		// In-flight connections should start terminating after closing [m.destroyed].
		close(m.destroyed)

		// Close the link endpoint and the TUN, this should cause [forwardBetweenTunAndNetstack] to terminate
		// if it hasn't already.
		m.linkEndpoint.Close()
		err := trace.Wrap(m.tun.Close(), "closing TUN device")

		allErrors <- err
		return err
	})

	// Deliberately ignoring the error from g.Wait() to return an aggregate of all errors.
	_ = g.Wait()

	// Wait for all connections and goroutines to clean themselves up.
	m.wg.Wait()

	// Now we can destroy the gVisor networking stack and wait for all its goroutines to terminate.
	m.stack.Destroy()

	close(allErrors)
	return trace.NewAggregateFromChannel(allErrors, context.Background())
}

func (m *Manager) handleTCP(req *tcp.ForwarderRequest) {
	// Add 1 to the waitgroup because the networking stack runs this in its own goroutine.
	m.wg.Add(1)
	defer m.wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Clients of *tcp.ForwarderRequest must eventually call Complete on it exactly once.
	// [req] consumes 1 of [maxInFlightTCPConnectionAttempts] until [req.Complete] is called.
	var completed bool
	defer func() {
		if !completed {
			req.Complete(true /*send TCP reset*/)
		}
	}()

	id := req.ID()
	slog := m.slog.With("request", id)
	slog.DebugContext(ctx, "Handling TCP connection.")
	defer slog.DebugContext(ctx, "Finished handling TCP connection.")

	handler, ok := m.getTCPHandler(id.LocalAddress)
	if !ok {
		slog.DebugContext(ctx, "No handler for address.", "addr", id.LocalAddress)
		return
	}

	connector := func() (io.ReadWriteCloser, error) {
		var wq waiter.Queue
		waitEntry, notifyCh := waiter.NewChannelEntry(waiter.EventErr | waiter.EventHUp)
		wq.EventRegister(&waitEntry)
		defer wq.EventUnregister(&waitEntry)

		endpoint, err := req.CreateEndpoint(&wq)
		if err != nil {
			// This err doesn't actually implement [error]
			return nil, trace.Errorf("creating TCP endpoint: %s", err)
		}

		completed = true
		req.Complete(false /*don't send TCP reset*/)

		endpoint.SocketOptions().SetKeepAlive(true)

		conn, connClosed := newConnWithCloseNotifier(gonet.NewTCPConn(&wq, endpoint))

		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			select {
			case <-connClosed:
				// Conn is already being closed, nothing to do.
				return
			case <-notifyCh:
				slog.DebugContext(ctx, "Got HUP or ERR, closing TCP conn.")
			case <-m.destroyed:
				slog.DebugContext(ctx, "VNet is being destroyed, closing TCP conn.")
			}
			cancel()
			conn.Close()
		}()

		return conn, nil
	}

	if err := handler.handleTCP(ctx, connector); err != nil {
		if errors.Is(err, context.Canceled) {
			slog.DebugContext(ctx, "TCP connection handler returned early due to canceled context.")
		} else {
			slog.DebugContext(ctx, "Error handling TCP connection.", "err", err)
		}
	}
}

func (m *Manager) getTCPHandler(addr tcpip.Address) (tcpHandler, bool) {
	m.state.mu.RLock()
	defer m.state.mu.RUnlock()
	handler, ok := m.state.tcpHandlers[addr]
	return handler, ok
}

func (m *Manager) assignTCPHandler(handler tcpHandler) (tcpip.Address, error) {
	m.state.mu.Lock()
	defer m.state.mu.Unlock()

	m.state.lastAssignedIPSuffix++
	ipSuffix := m.state.lastAssignedIPSuffix

	addr := ipv6WithSuffix(m.ipv6Prefix, u32ToBytes(ipSuffix))

	m.state.tcpHandlers[addr] = handler
	if err := m.addProtocolAddress(addr); err != nil {
		return addr, trace.Wrap(err)
	}

	return addr, nil
}

func forwardBetweenTunAndNetstack(ctx context.Context, tun TUNDevice, linkEndpoint *channel.Endpoint) error {
	slog.DebugContext(ctx, "Forwarding IP packets between OS and VNet.")
	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error { return forwardNetstackToTUN(ctx, linkEndpoint, tun) })
	g.Go(func() error { return forwardTUNtoNetstack(tun, linkEndpoint) })
	return trace.Wrap(g.Wait())
}

func forwardNetstackToTUN(ctx context.Context, linkEndpoint *channel.Endpoint, tun TUNDevice) error {
	bufs := [][]byte{make([]byte, device.MessageTransportHeaderSize+mtu)}
	for {
		packet := linkEndpoint.ReadContext(ctx)
		if packet.IsNil() {
			// Nil packet is returned when context is canceled.
			return trace.Wrap(ctx.Err())
		}
		offset := device.MessageTransportHeaderSize
		for _, s := range packet.AsSlices() {
			offset += copy(bufs[0][offset:], s)
		}
		packet.DecRef()
		bufs[0] = bufs[0][:offset]
		if _, err := tun.Write(bufs, device.MessageTransportHeaderSize); err != nil {
			return trace.Wrap(err, "writing packets to TUN")
		}
		bufs[0] = bufs[0][:cap(bufs[0])]
	}
}

func forwardTUNtoNetstack(tun TUNDevice, linkEndpoint *channel.Endpoint) error {
	const readOffset = device.MessageTransportHeaderSize
	bufs := make([][]byte, tun.BatchSize())
	for i := range bufs {
		bufs[i] = make([]byte, device.MessageTransportHeaderSize+mtu)
	}
	sizes := make([]int, len(bufs))
	for {
		n, err := tun.Read(bufs, sizes, readOffset)
		if err != nil {
			return trace.Wrap(err, "reading packets from TUN")
		}
		for i := range sizes[:n] {
			protocol, ok := protocolVersion(bufs[i][readOffset])
			if !ok {
				continue
			}
			packet := stack.NewPacketBuffer(stack.PacketBufferOptions{
				Payload: buffer.MakeWithData(bufs[i][readOffset : readOffset+sizes[i]]),
			})
			linkEndpoint.InjectInbound(protocol, packet)
			packet.DecRef()
		}
	}
}

func (m *Manager) addProtocolAddress(localAddress tcpip.Address) error {
	protocolAddress, err := protocolAddress(localAddress)
	if err != nil {
		return trace.Wrap(err)
	}
	if err := m.stack.AddProtocolAddress(nicID, protocolAddress, stack.AddressProperties{}); err != nil {
		return trace.Errorf("%s", err)
	}
	return nil
}

func protocolAddress(addr tcpip.Address) (tcpip.ProtocolAddress, error) {
	addrWithPrefix := addr.WithPrefix()
	var protocol tcpip.NetworkProtocolNumber
	switch addrWithPrefix.PrefixLen {
	case 32:
		protocol = ipv4.ProtocolNumber
	case 128:
		protocol = ipv6.ProtocolNumber
	default:
		return tcpip.ProtocolAddress{}, trace.BadParameter("unhandled prefix len %d", addrWithPrefix.PrefixLen)
	}
	return tcpip.ProtocolAddress{
		AddressWithPrefix: addrWithPrefix,
		Protocol:          protocol,
	}, nil
}

func protocolVersion(b byte) (tcpip.NetworkProtocolNumber, bool) {
	switch b >> 4 {
	case 4:
		return header.IPv4ProtocolNumber, true
	case 6:
		return header.IPv6ProtocolNumber, true
	}
	return 0, false
}

func ipv6WithSuffix(prefix tcpip.Address, suffix []byte) tcpip.Address {
	addrBytes := prefix.As16()
	offset := len(addrBytes) - len(suffix)
	for i, b := range suffix {
		addrBytes[offset+i] = b
	}
	return tcpip.AddrFrom16(addrBytes)
}

func u32ToBytes(i uint32) []byte {
	bytes := make([]byte, 4)
	bytes[0] = byte(i >> 24)
	bytes[1] = byte(i >> 16)
	bytes[2] = byte(i >> 8)
	bytes[3] = byte(i >> 0)
	return bytes
}

// newConnWithCloseNotifier returns a net.Conn and a channel that will be closed when the conn is closed.
func newConnWithCloseNotifier(conn *gonet.TCPConn) (net.Conn, <-chan struct{}) {
	ch := make(chan struct{})
	return &connWithCloseNotifier{
		TCPConn:   conn,
		closeOnce: sync.OnceFunc(func() { close(ch) }),
	}, ch
}

type connWithCloseNotifier struct {
	*gonet.TCPConn
	closeOnce func()
}

func (c *connWithCloseNotifier) Close() error {
	c.closeOnce()
	return c.TCPConn.Close()
}
