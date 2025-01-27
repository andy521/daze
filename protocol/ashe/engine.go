package ashe

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"time"

	"github.com/godump/doa"
	"github.com/mohanson/daze"
)

// This document describes a TCP-based cryptographic proxy protocol. The main purpose of this protocol is to bypass
// firewalls while providing a good user experience, so it only provides minimal security, which is one of the reasons
// for choosing the RC4 algorithm(RC4 is cryptographically broken and should not be used for secure applications).
//
// The client connects to the server, and sends a request details:
//
// +-----+-----------+------+-----+---------+---------+
// | OTA | Handshake | Time | Net | DST.Len | DST     |
// +-----+-----------+------+-----+---------+---------+
// | 128 | 2         | 8    |  1  | 1       | 0 - 255 |
// +-----+-----------+------+-----+---------+---------+
//
// - OTA       : Random 128 bytes for rc4 key, all data will be transmitted encrypted after there
// - Handshake : Must be 0xff, 0xff, used to quickly verify the client's key
// - Time      : Timestamp of request. The server will reject requests with past or future timestamps to prevent replay
//               attacks.
// - Net       : 0x01 : TCP
//               0x03 : UDP
// - DST.Len   : Len of DST. If DST is https://google.com, DST.Len is 0x12
// - DST       : Desired destination address
//
// The server returns:
//
// +------+
// | Code |
// +------+
// |  1   |
// +------+
//
// - Code: 0x00: succeed
//         0x01: general server failure

// Conf is acting as package level configuration.
var Conf = struct {
	// The time error allowed by the server in seconds.
	LifeExpired int
}{
	LifeExpired: 120,
}

// TCPConn is an implementation of the Conn interface for TCP network connections.
type TCPConn struct {
	io.ReadWriteCloser
}

// NewTCPConn returns a new TCPConn.
func NewTCPConn(c io.ReadWriteCloser) *TCPConn {
	return &TCPConn{c}
}

// UDPConn is an implementation of the Conn interface for UDP network connections.
type UDPConn struct {
	io.ReadWriteCloser
	b []byte
}

// NewUDPConn returns a new UDPConn.
func NewUDPConn(c io.ReadWriteCloser) *UDPConn {
	return &UDPConn{ReadWriteCloser: c, b: make([]byte, 2)}
}

// Read implements the Conn Read method.
func (c *UDPConn) Read(p []byte) (int, error) {
	_, err := io.ReadFull(c.ReadWriteCloser, p[:2])
	if err != nil {
		return 0, err
	}
	n := binary.BigEndian.Uint16(p[:2])
	return io.ReadFull(c.ReadWriteCloser, p[:n])
}

// Write implements the Conn Write method.
func (c *UDPConn) Write(p []byte) (int, error) {
	// Maximum UDP packet size is 2^16 bytes in theoretically.
	// But every packet lives in an Ethernet frame. Ethernet frames can only contain 1500 bytes of data. This is called
	// the "maximum transmission unit" or "MTU".
	doa.Doa(len(p) <= math.MaxUint16)
	binary.BigEndian.PutUint16(c.b, uint16(len(p)))
	_, err := c.ReadWriteCloser.Write(c.b[:2])
	if err != nil {
		return 0, err
	}
	return c.ReadWriteCloser.Write(p)
}

// Server implemented the ashe protocol. The ASHE server will typically evaluate the request based on source and
// destination addresses, and return one or more reply messages, as appropriate for the request type.
type Server struct {
	Listen string
	Cipher [16]byte
	Closer io.Closer
}

// ServeCipher creates an encrypted channel.
func (s *Server) ServeCipher(ctx *daze.Context, raw io.ReadWriteCloser) (io.ReadWriteCloser, error) {
	var (
		buf     = make([]byte, 144)
		cli     io.ReadWriteCloser
		err     error
		gap     int64
		gapSign int64
	)
	_, err = io.ReadFull(raw, buf[:128])
	if err != nil {
		return nil, err
	}
	copy(buf[128:144], s.Cipher[:])
	cli = daze.Gravity(raw, buf[:144])
	_, err = io.ReadFull(cli, buf[:10])
	if err != nil {
		return nil, err
	}
	if buf[0] != 0xff || buf[1] != 0xff {
		return nil, errors.New("daze: request malformed")
	}
	gap = time.Now().Unix() - int64(binary.BigEndian.Uint64(buf[2:10]))
	gapSign = gap >> 63
	if gap^gapSign-gapSign > int64(Conf.LifeExpired) {
		return nil, errors.New("daze: request expired")
	}
	return cli, nil
}

// Serve. Parameter raw will be closed automatically when the function exits.
func (s *Server) Serve(ctx *daze.Context, cli io.ReadWriteCloser) error {
	var (
		buf    = make([]byte, 256)
		dst    string
		dstLen uint8
		dstNet uint8
		srv    io.ReadWriteCloser
		err    error
	)
	cli, err = s.ServeCipher(ctx, cli)
	if err != nil {
		return err
	}
	_, err = io.ReadFull(cli, buf[:2])
	if err != nil {
		return err
	}
	dstNet = buf[0]
	dstLen = buf[1]
	_, err = io.ReadFull(cli, buf[:dstLen])
	if err != nil {
		return err
	}
	dst = string(buf[:dstLen])
	switch dstNet {
	case 0x01:
		log.Printf("%s   dial network=tcp address=%s", ctx.Cid, dst)
		srv, err = daze.Conf.Dialer.Dial("tcp", dst)
	case 0x03:
		log.Printf("%s   dial network=udp address=%s", ctx.Cid, dst)
		srv, err = daze.Conf.Dialer.Dial("udp", dst)
	}
	if err != nil {
		cli.Write([]byte{1})
		return err
	}
	cli.Write([]byte{0})
	switch dstNet {
	case 0x01:
		cli = NewTCPConn(cli)
	case 0x03:
		cli = NewUDPConn(cli)
	}
	daze.Link(cli, srv)
	return nil
}

// Close listener. Established connections will not be closed.
func (s *Server) Close() error {
	if s.Closer != nil {
		return s.Closer.Close()
	}
	return nil
}

// Run.
func (s *Server) Run() error {
	ln, err := net.Listen("tcp", s.Listen)
	if err != nil {
		return err
	}
	s.Closer = ln
	log.Println("listen and serve on", s.Listen)

	go func() {
		idx := uint32(math.MaxUint32)
		for {
			cli, err := ln.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					log.Println(err)
				}
				break
			}
			idx += 1
			ctx := &daze.Context{Cid: daze.Hu32(idx)}
			log.Printf("%s accept remote=%s", ctx.Cid, cli.RemoteAddr())
			go func(cli net.Conn) {
				defer cli.Close()
				if err := s.Serve(ctx, cli); err != nil {
					log.Println(ctx.Cid, " error", err)
				}
				log.Println(ctx.Cid, "closed")
			}(cli)
		}
	}()

	return nil
}

// NewServer returns a new Server.
func NewServer(listen string, cipher string) *Server {
	return &Server{
		Listen: listen,
		Cipher: md5.Sum([]byte(cipher)),
	}
}

// Client implemented the ashe protocol.
type Client struct {
	Server string
	Cipher [16]byte
}

// WithCipher creates an encrypted channel.
func (c *Client) WithCipher(ctx *daze.Context, raw io.ReadWriteCloser) (io.ReadWriteCloser, error) {
	var (
		buf = make([]byte, 144)
		err error
		srv io.ReadWriteCloser
	)
	daze.Conf.Random.Read(buf[:128])
	_, err = raw.Write(buf[:128])
	if err != nil {
		return nil, err
	}
	copy(buf[128:144], c.Cipher[:])
	srv = daze.Gravity(raw, buf[:144])
	buf[0x00] = 0xff
	buf[0x01] = 0xff
	binary.BigEndian.PutUint64(buf[2:10], uint64(time.Now().Unix()))
	_, err = srv.Write(buf[:10])
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// With an existing connection. It is the caller's responsibility to close the srv.
func (c *Client) With(ctx *daze.Context, srv io.ReadWriteCloser, network string, address string) (io.ReadWriteCloser, error) {
	var (
		n   = len(address)
		buf = make([]byte, 2)
		err error
	)
	if n > 255 {
		return nil, fmt.Errorf("daze: destination address too long %s", address)
	}
	if network != "tcp" && network != "udp" {
		return nil, fmt.Errorf("daze: network must be tcp or udp")
	}
	srv, err = c.WithCipher(ctx, srv)
	if err != nil {
		return nil, err
	}
	switch network {
	case "tcp":
		buf[0x00] = 0x01
	case "udp":
		buf[0x00] = 0x03
	}
	buf[0x01] = uint8(n)
	srv.Write(buf[:2])
	_, err = srv.Write([]byte(address))
	if err != nil {
		return nil, err
	}
	_, err = io.ReadFull(srv, buf[:1])
	if err != nil {
		return nil, err
	}
	if buf[0] != 0 {
		return nil, errors.New("daze: general server failure")
	}
	switch network {
	case "tcp":
		return NewTCPConn(srv), nil
	case "udp":
		return NewUDPConn(srv), nil
	}
	panic("unreachable")
}

// Dial connects to the address on the named network.
func (c *Client) Dial(ctx *daze.Context, network string, address string) (io.ReadWriteCloser, error) {
	srv, err := daze.Conf.Dialer.Dial("tcp", c.Server)
	if err != nil {
		return nil, err
	}
	ret, err := c.With(ctx, srv, network, address)
	if err != nil {
		srv.Close()
	}
	return ret, err
}

// NewClient returns a new Client. A secret data needs to be passed in Cipher, as a sign to interface with the Server.
func NewClient(server, cipher string) *Client {
	return &Client{
		Server: server,
		Cipher: md5.Sum([]byte(cipher)),
	}
}
