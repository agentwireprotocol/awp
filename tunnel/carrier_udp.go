package tunnel

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// udpCarrier is WireGuard's native carrier (section 7.1). It has one socket
// per listen address, plus an ephemeral one for dialing when it listens on
// none.
type udpCarrier struct {
	b *bind

	mu      sync.Mutex
	socks   []*net.UDPConn
	dialer  *net.UDPConn
	resolve map[string]netip.AddrPort
	done    bool
}

func newUDP(b *bind) *udpCarrier {
	return &udpCarrier{b: b, resolve: map[string]netip.AddrPort{}}
}

// listen binds hostport and returns the endpoints to advertise. An
// unspecified host advertises every global and private unicast address of
// this machine.
func (u *udpCarrier) listen(hostport string) ([]string, error) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return nil, err
	}
	c, err := net.ListenUDP("udp", mustUDPAddr(hostport))
	if err != nil {
		return nil, err
	}
	u.mu.Lock()
	if u.done {
		u.mu.Unlock()
		c.Close()
		return nil, net.ErrClosed
	}
	u.socks = append(u.socks, c)
	u.mu.Unlock()
	go u.read(c)
	if port == "0" {
		port = strconv.Itoa(c.LocalAddr().(*net.UDPAddr).Port)
	}
	if ip, err := netip.ParseAddr(host); err == nil && !ip.IsUnspecified() {
		return []string{net.JoinHostPort(host, port)}, nil
	}
	if host != "" && net.ParseIP(host) == nil {
		return []string{net.JoinHostPort(host, port)}, nil // a name
	}
	return localEndpoints(port), nil
}

func mustUDPAddr(hostport string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", hostport)
	if err != nil {
		return &net.UDPAddr{}
	}
	return a
}

func localEndpoints(port string) []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		pfx, err := netip.ParsePrefix(a.String())
		if err != nil {
			continue
		}
		ip := pfx.Addr()
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
			continue
		}
		out = append(out, net.JoinHostPort(ip.String(), port))
	}
	if len(out) == 0 {
		out = []string{net.JoinHostPort("127.0.0.1", port)}
	}
	return out
}

func (u *udpCarrier) read(c *net.UDPConn) {
	buf := make([]byte, 65536)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		u.b.deliver(KindUDP, from.String(), append([]byte(nil), buf[:n]...))
	}
}

func (u *udpCarrier) sock() (*net.UDPConn, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.done {
		return nil, net.ErrClosed
	}
	if len(u.socks) > 0 {
		return u.socks[0], nil
	}
	if u.dialer == nil {
		c, err := net.ListenUDP("udp", nil)
		if err != nil {
			return nil, err
		}
		u.dialer = c
		go u.read(c)
	}
	return u.dialer, nil
}

func (u *udpCarrier) addr(to string) (netip.AddrPort, error) {
	if ap, err := netip.ParseAddrPort(to); err == nil {
		return ap, nil
	}
	u.mu.Lock()
	ap, ok := u.resolve[to]
	u.mu.Unlock()
	if ok {
		return ap, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	host, port, err := net.SplitHostPort(to)
	if err != nil {
		return netip.AddrPort{}, err
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return netip.AddrPort{}, errors.New("udp: cannot resolve " + host)
	}
	p, _ := strconv.Atoi(port)
	ap = netip.AddrPortFrom(ips[0].Unmap(), uint16(p))
	u.mu.Lock()
	u.resolve[to] = ap
	u.mu.Unlock()
	return ap, nil
}

func (u *udpCarrier) send(to string, pkt []byte) error {
	ap, err := u.addr(to)
	if err != nil {
		return err
	}
	c, err := u.sock()
	if err != nil {
		return err
	}
	_, err = c.WriteToUDPAddrPort(pkt, ap)
	return err
}

func (u *udpCarrier) close() error {
	u.mu.Lock()
	u.done = true
	socks := append([]*net.UDPConn{}, u.socks...)
	if u.dialer != nil {
		socks = append(socks, u.dialer)
	}
	u.mu.Unlock()
	for _, c := range socks {
		c.Close()
	}
	return nil
}
