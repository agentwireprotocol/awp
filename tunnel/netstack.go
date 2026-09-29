package tunnel

// A userspace TCP/IP stack as a WireGuard TUN device, adapted from
// wireguard-go's tun/netstack (MIT licensed, Copyright (C) 2017-2023
// WireGuard LLC) for the gVisor version this module pins, and trimmed to
// what the tunnel needs: one IPv6 address, TCP.

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"syscall"
	"time"

	"github.com/tailscale/wireguard-go/tun"
	"gvisor.dev/gvisor/pkg/buffer"
	glog "gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// gVisor logs warnings (segments for connections already gone, and the
// like) straight to stderr. They are noise for a peer's user.
func init() { glog.SetTarget(discard{}) }

type discard struct{}

func (discard) Emit(int, glog.Level, time.Time, string, ...any) {}

type netTun struct {
	ep       *channel.Endpoint
	stack    *stack.Stack
	events   chan tun.Event
	incoming chan *buffer.View
	closed   chan struct{}
	mtu      int
}

func newNetTun(addr netip.Addr, mtu int) (*netTun, error) {
	t := &netTun{
		ep: channel.New(1024, uint32(mtu), ""),
		stack: stack.New(stack.Options{
			NetworkProtocols:   []stack.NetworkProtocolFactory{ipv6.NewProtocol},
			TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, icmp.NewProtocol6},
			HandleLocal:        true,
		}),
		events:   make(chan tun.Event, 10),
		incoming: make(chan *buffer.View),
		closed:   make(chan struct{}),
		mtu:      mtu,
	}
	sack := tcpip.TCPSACKEnabled(true)
	if err := t.stack.SetTransportProtocolOption(tcp.ProtocolNumber, &sack); err != nil {
		return nil, fmt.Errorf("enable TCP SACK: %v", err)
	}
	t.ep.AddNotify(t)
	if err := t.stack.CreateNIC(1, t.ep); err != nil {
		return nil, fmt.Errorf("CreateNIC: %v", err)
	}
	pa := tcpip.ProtocolAddress{
		Protocol:          ipv6.ProtocolNumber,
		AddressWithPrefix: tcpip.AddrFromSlice(addr.AsSlice()).WithPrefix(),
	}
	if err := t.stack.AddProtocolAddress(1, pa, stack.AddressProperties{}); err != nil {
		return nil, fmt.Errorf("AddProtocolAddress(%v): %v", addr, err)
	}
	t.stack.AddRoute(tcpip.Route{Destination: header.IPv6EmptySubnet, NIC: 1})
	t.events <- tun.EventUp
	return t, nil
}

func (t *netTun) Name() (string, error)    { return "awp", nil }
func (t *netTun) File() *os.File           { return nil }
func (t *netTun) Events() <-chan tun.Event { return t.events }
func (t *netTun) MTU() (int, error)        { return t.mtu, nil }
func (t *netTun) BatchSize() int           { return 1 }

func (t *netTun) Read(slab []byte, packets []tun.ReadPacket) (int, error) {
	var view *buffer.View
	select {
	case view = <-t.incoming:
	case <-t.closed:
		return 0, os.ErrClosed
	}
	n, err := view.Read(slab[tun.ReadPacketSpacing : len(slab)-tun.ReadPacketSpacing])
	if err != nil {
		return 0, err
	}
	packets[0].Size = n
	packets[0].Offset = tun.ReadPacketSpacing
	return 1, nil
}

func (t *netTun) Write(bufs [][]byte, offset int) (int, error) {
	for _, b := range bufs {
		p := b[offset:]
		if len(p) == 0 {
			continue
		}
		if p[0]>>4 != 6 {
			return 0, syscall.EAFNOSUPPORT
		}
		pkb := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(p)})
		t.ep.InjectInbound(header.IPv6ProtocolNumber, pkb)
	}
	return len(bufs), nil
}

// WriteNotify is called by the channel endpoint when the stack has a packet
// to send.
func (t *netTun) WriteNotify() {
	pkt := t.ep.Read()
	if pkt == nil {
		return
	}
	view := pkt.ToView()
	pkt.DecRef()
	select {
	case t.incoming <- view:
	case <-t.closed:
	}
}

func (t *netTun) Close() error {
	select {
	case <-t.closed:
		return nil
	default:
	}
	close(t.closed)
	t.stack.RemoveNIC(1)
	t.stack.Close()
	close(t.events)
	t.ep.Close()
	return nil
}

func fullAddr(ap netip.AddrPort) tcpip.FullAddress {
	return tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(ap.Addr().AsSlice()), Port: ap.Port()}
}

func (t *netTun) dialTCP(ctx context.Context, ap netip.AddrPort) (*gonet.TCPConn, error) {
	return gonet.DialContextTCP(ctx, t.stack, fullAddr(ap), ipv6.ProtocolNumber)
}

func (t *netTun) listenTCP(ap netip.AddrPort) (net.Listener, error) {
	return gonet.ListenTCP(t.stack, fullAddr(ap), ipv6.ProtocolNumber)
}
