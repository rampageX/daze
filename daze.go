package daze

import (
	"bufio"
	"bytes"
	"context"
	"crypto/cipher"
	"crypto/rc4"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mohanson/daze/router"
)

var Conf = struct {
	DialTimeout        time.Duration
	PathDelegatedApnic string
	PathRule           string
}{
	DialTimeout:        time.Second * 8,
	PathDelegatedApnic: "/delegated-apnic-latest",
	PathRule:           "/rule.ls",
}

// Link copies from src to dst and dst to src until either EOF is reached.
func Link(a, b io.ReadWriteCloser) {
	go func() {
		io.Copy(b, a)
		a.Close()
		b.Close()
	}()
	io.Copy(a, b)
	b.Close()
	a.Close()
}

// ReadWriteCloser is the interface that groups the basic Read, Write and
// Close methods.
type ReadWriteCloser struct {
	io.Reader
	io.Writer
	io.Closer
}

// GravityReader wraps an io.Reader with RC4 crypto.
func GravityReader(r io.Reader, k []byte) io.Reader {
	cr, _ := rc4.NewCipher(k)
	return cipher.StreamReader{S: cr, R: r}
}

// GravityWriter wraps an io.Writer with RC4 crypto.
func GravityWriter(w io.Writer, k []byte) io.Writer {
	cw, _ := rc4.NewCipher(k)
	return cipher.StreamWriter{S: cw, W: w}
}

// Double gravity, double happiness.
func Gravity(conn io.ReadWriteCloser, k []byte) io.ReadWriteCloser {
	cr, _ := rc4.NewCipher(k)
	cw, _ := rc4.NewCipher(k)
	return &ReadWriteCloser{
		Reader: cipher.StreamReader{S: cr, R: conn},
		Writer: cipher.StreamWriter{S: cw, W: conn},
		Closer: conn,
	}
}

// Resolve modifies the net.DefaultResolver(which is the resolver used by the package-level Lookup functions and by
// Dialers without a specified Resolver).
//
// Examples:
//   Resolve("8.8.8.8:53")
//   Resolve("114.114.114.114:53")
func Resolve(addr string) {
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", addr)
		},
	}
}

// Dialer contains options for connecting to an address.
type Dialer interface {
	Dial(ctx context.Context, network string, address string) (io.ReadWriteCloser, error)
}

// OpenFile select the appropriate method to open the file based on the incoming args automatically.
//
// Examples:
//   OpenFile("/etc/hosts")
//   OpenFile("https://raw.githubusercontent.com/mohanson/daze/master/README.md")
func OpenFile(name string) (io.ReadCloser, error) {
	if strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://") {
		resp, err := http.Get(name)
		if err != nil {
			return nil, err
		}
		return resp.Body, nil
	} else {
		return os.Open(name)
	}
}

// Locale is the main process of daze. In most cases, it is usually deployed as a daemon on a local machine.
type Locale struct {
	Listen string
	Dialer Dialer
}

// Serve traffic in HTTP Proxy/Tunnel format.
//
// Introduction:
//   See https://en.wikipedia.org/wiki/Proxy_server
//   See https://en.wikipedia.org/wiki/HTTP_tunnel
//
// Warning: The performance of HTTP Proxy is very poor, unless you have a good reason, please use ServeSocks4 or
// ServeSocks5 instead. Why the poor performance is that I did not implement http persistent connection(a well-known
// name is KeepAlive) because It will trigger some bugs on Firefox. Firefox always sends traffic from different sites
// to the one persistent connection. I have been debugging for a long time.
//
// Fuck.
func (l *Locale) ServeProxy(ctx context.Context, app io.ReadWriteCloser) error {
	reader := bufio.NewReader(app)

	for {
		if err := func() error {
			r, err := http.ReadRequest(reader)
			if err != nil {
				return err
			}

			var port string
			if r.URL.Port() == "" {
				port = "80"
			} else {
				port = r.URL.Port()
			}

			srv, err := l.Dialer.Dial(ctx, "tcp", r.URL.Hostname()+":"+port)
			if err != nil {
				return err
			}
			defer srv.Close()
			servReader := bufio.NewReader(srv)

			if r.Method == "CONNECT" {
				log.Println(ctx.Value("cid"), " proto", "format=tunnel")
				_, err := app.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
				if err != nil {
					return err
				}
				Link(app, srv)
				return nil
			}

			log.Println(ctx.Value("cid"), " proto", "format=hproxy")
			if r.Method == "GET" && r.Header.Get("Upgrade") == "websocket" {
				if err := r.Write(srv); err != nil {
					return err
				}
				Link(app, srv)
				return nil
			}
			if err := r.Write(srv); err != nil {
				return err
			}
			resp, err := http.ReadResponse(servReader, r)
			if err != nil {
				return err
			}
			return resp.Write(app)
		}(); err != nil {
			break
		}
	}
	return nil
}

// Serve traffic in SOCKS4/SOCKS4a format.
//
// Introduction:
//   See https://en.wikipedia.org/wiki/SOCKS
//   See http://ftp.icm.edu.pl/packages/socks/socks4/SOCKS4.protocol
func (l *Locale) ServeSocks4(ctx context.Context, app io.ReadWriteCloser) error {
	var (
		reader    = bufio.NewReader(app)
		fCode     uint8
		fDstPort  = make([]byte, 2)
		fDstIP    = make([]byte, 4)
		fHostName []byte
		dstHost   string
		dstPort   uint16
		dst       string
		srv       io.ReadWriteCloser
		err       error
	)
	app = ReadWriteCloser{
		Reader: reader,
		Writer: app,
		Closer: app,
	}
	reader.Discard(1)
	fCode, _ = reader.ReadByte()
	io.ReadFull(reader, fDstPort)
	dstPort = binary.BigEndian.Uint16(fDstPort)
	io.ReadFull(reader, fDstIP)
	_, err = reader.ReadBytes(0x00)
	if err != nil {
		return err
	}
	if bytes.Equal(fDstIP[:3], []byte{0x00, 0x00, 0x00}) && fDstIP[3] != 0x00 {
		fHostName, err = reader.ReadBytes(0x00)
		if err != nil {
			return err
		}
		fHostName = fHostName[:len(fHostName)-1]
		dstHost = string(fHostName)
	} else {
		dstHost = net.IP(fDstIP).String()
	}
	dst = dstHost + ":" + strconv.Itoa(int(dstPort))
	log.Println(ctx.Value("cid"), " proto", "format=socks4")
	switch fCode {
	case 0x01:
		srv, err = l.Dialer.Dial(ctx, "tcp", dst)
		if err != nil {
			app.Write([]byte{0x00, 0x5b, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
			return err
		} else {
			defer srv.Close()
			app.Write([]byte{0x00, 0x5a, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
			Link(app, srv)
			return nil
		}
	case 0x02:
		panic("unreachable")
	}
	return nil
}

// Serve traffic in SOCKS5 format.
//
// Introduction:
//   See https://en.wikipedia.org/wiki/SOCKS
//   See https://tools.ietf.org/html/rfc1928
func (l *Locale) ServeSocks5(ctx context.Context, app io.ReadWriteCloser) error {
	var (
		reader   = bufio.NewReader(app)
		fN       uint8
		fCmd     uint8
		fAT      uint8
		fDstAddr []byte
		fDstPort = make([]byte, 2)
		dstHost  string
		dstPort  uint16
		dst      string
		err      error
	)
	app = ReadWriteCloser{
		Reader: reader,
		Writer: app,
		Closer: app,
	}
	reader.Discard(1)
	fN, _ = reader.ReadByte()
	reader.Discard(int(fN))
	app.Write([]byte{0x05, 0x00})
	reader.Discard(1)
	fCmd, _ = reader.ReadByte()
	reader.Discard(1)
	fAT, _ = reader.ReadByte()
	switch fAT {
	case 0x01:
		fDstAddr = make([]byte, 4)
		io.ReadFull(reader, fDstAddr)
		dstHost = net.IP(fDstAddr).String()
	case 0x03:
		fN, _ = reader.ReadByte()
		fDstAddr = make([]byte, int(fN))
		io.ReadFull(reader, fDstAddr)
		dstHost = string(fDstAddr)
	case 0x04:
		fDstAddr = make([]byte, 16)
		io.ReadFull(reader, fDstAddr)
		dstHost = net.IP(fDstAddr).String()
	}
	if _, err = io.ReadFull(app, fDstPort); err != nil {
		return err
	}
	dstPort = binary.BigEndian.Uint16(fDstPort)
	dst = dstHost + ":" + strconv.Itoa(int(dstPort))
	switch fCmd {
	case 0x01:
		return l.ServeSocks5TCP(ctx, app, dst)
	case 0x02:
		panic("unreachable")
	case 0x03:
		return l.ServeSocks5UDP(ctx, app)
	}
	return nil
}

// Socks5 TCP protocol.
func (l *Locale) ServeSocks5TCP(ctx context.Context, app io.ReadWriteCloser, dst string) error {
	log.Println(ctx.Value("cid"), " proto", "format=socks5")
	srv, err := l.Dialer.Dial(ctx, "tcp", dst)
	if err != nil {
		app.Write([]byte{0x05, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		return err
	} else {
		defer srv.Close()
		app.Write([]byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
		Link(app, srv)
		return nil
	}
}

// Socks5 UDP protocol.
func (l *Locale) ServeSocks5UDP(ctx context.Context, app io.ReadWriteCloser) error {
	var (
		bndAddr     *net.UDPAddr
		bndPort     uint16
		bnd         *net.UDPConn
		appAddr     *net.UDPAddr
		appSize     int
		appHeadSize int
		appHead     []byte
		dstHost     string
		dstPort     uint16
		dst         string
		srv         io.ReadWriteCloser
		b           bool
		cpl         = map[string]io.ReadWriteCloser{}
		buf         = make([]byte, 2048)
		err         error
	)

	defer app.Close()
	bndAddr, _ = net.ResolveUDPAddr("udp", "127.0.0.1:0")
	bnd, _ = net.ListenUDP("udp", bndAddr)
	defer bnd.Close()
	bndPort = uint16(bnd.LocalAddr().(*net.UDPAddr).Port)
	copy(buf, []byte{0x05, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00})
	binary.BigEndian.PutUint16(buf[8:10], bndPort)
	app.Write(buf[:10])

	go func() {
		io.Copy(ioutil.Discard, app)
		app.Close()
		bnd.Close()
	}()

	for {
		appSize, appAddr, err = bnd.ReadFromUDP(buf)
		if err != nil {
			break
		}

		switch buf[3] {
		case 0x01:
			appHeadSize = 10
		case 0x03:
			appHeadSize = int(buf[4]) + 7
		case 0x04:
			appHeadSize = 22
		}

		appHead = make([]byte, appHeadSize)
		copy(appHead, buf[0:appHeadSize])

		switch appHead[3] {
		case 0x01:
			dstHost = net.IP(appHead[4:8]).String()
			dstPort = binary.BigEndian.Uint16(appHead[8:10])
		case 0x03:
			l := appHead[4]
			dstHost = string(appHead[5 : 5+l])
			dstPort = binary.BigEndian.Uint16(appHead[5+l : 7+l])
		case 0x04:
			dstHost = net.IP(appHead[4:20]).String()
			dstPort = binary.BigEndian.Uint16(appHead[20:22])
		}
		dst = dstHost + ":" + strconv.Itoa(int(dstPort))

		srv, b = cpl[dst]
		if b {
			goto send
		}
	init:
		log.Println(ctx.Value("cid"), " proto", "format=socks5")
		srv, err = l.Dialer.Dial(ctx, "udp", dst)
		if err != nil {
			log.Println(ctx.Value("cid"), " error", err)
			continue
		}
		cpl[dst] = srv

		go func(srv io.ReadWriteCloser, appHead []byte, appAddr *net.UDPAddr) {
			var (
				buf = make([]byte, 2048)
				l   = len(appHead)
				n   int
				err error
			)
			copy(buf, appHead)
			for {
				n, err = srv.Read(buf[l:])
				if err != nil {
					break
				}
				_, err = bnd.WriteToUDP(buf[:l+n], appAddr)
				if err != nil {
					break
				}
			}
			srv.Close()
		}(srv, appHead, appAddr)
	send:
		_, err = srv.(io.ReadWriteCloser).Write(buf[appHeadSize:appSize])
		if err != nil {
			log.Println(ctx.Value("cid"), " error", err)
			srv.Close()
			goto init
		}
	}
	for _, e := range cpl {
		e.Close()
	}
	return nil
}

// We should be very clear about what it does. It judges the traffic type and processes it with a different
// handler(ServeProxy/ServeSocks4/ServeSocks5).
func (l *Locale) Serve(ctx context.Context, app io.ReadWriteCloser) error {
	var (
		buf = make([]byte, 1)
		err error
	)
	_, err = io.ReadFull(app, buf)
	if err != nil {
		// There are some clients that will establish a link in advance without sending any messages so that they can
		// immediately get the connected conn when they really need it. When they leave, it makes no sense to report a
		// EOF error.
		if err == io.EOF {
			return nil
		}
		return err
	}
	app = ReadWriteCloser{
		Reader: io.MultiReader(bytes.NewReader(buf), app),
		Writer: app,
		Closer: app,
	}
	if buf[0] == 0x05 {
		return l.ServeSocks5(ctx, app)
	}
	if buf[0] == 0x04 {
		return l.ServeSocks4(ctx, app)
	}
	return l.ServeProxy(ctx, app)
}

// Run.
func (l *Locale) Run() error {
	s, err := net.Listen("tcp", l.Listen)
	if err != nil {
		return err
	}
	defer s.Close()
	log.Println("listen and serve on", l.Listen)

	i := uint32(math.MaxUint32)
	for {
		c, err := s.Accept()
		if err != nil {
			log.Println(err)
			continue
		}
		go func(c net.Conn) {
			defer c.Close()
			buf := make([]byte, 4)
			binary.BigEndian.PutUint32(buf, atomic.AddUint32(&i, 1))
			cid := hex.EncodeToString(buf)
			ctx := context.WithValue(context.Background(), "cid", cid)
			log.Printf("%s accept remote=%s", cid, c.RemoteAddr())
			if err := l.Serve(ctx, c); err != nil {
				log.Println(cid, " error", err)
			}
			log.Println(cid, "closed")
		}(c)
	}
}

// NewLocale returns a Locale.
func NewLocale(listen string, dialer Dialer) *Locale {
	return &Locale{
		Listen: listen,
		Dialer: dialer,
	}
}

// Direct is the default dialer for connecting to an address.
type Direct struct {
}

func (d *Direct) Dial(ctx context.Context, network string, address string) (io.ReadWriteCloser, error) {
	log.Printf("%s   dial routing=direct network=%s address=%s", ctx.Value("cid"), network, address)
	return net.DialTimeout(network, address, Conf.DialTimeout)
}

// Squire is a bit smart guy, it can automatically distinguish whether to use a proxy or a local network.
type Squire struct {
	Dialer Dialer
	Direct Dialer
	Router router.Router
}

// Dialer contains options for connecting to an address.
func (s *Squire) Dial(ctx context.Context, network string, address string) (io.ReadWriteCloser, error) {
	host, _, _ := net.SplitHostPort(address)
	switch s.Router.Choose(host) {
	case router.Direct:
		return s.Direct.Dial(ctx, network, address)
	case router.Daze:
		return s.Dialer.Dial(ctx, network, address)
	case router.Fucked:
		return nil, fmt.Errorf("daze: %s has been blocked", host)
	case router.Puzzle:
		return s.Dialer.Dial(ctx, network, address)
	}
	return s.Dialer.Dial(ctx, network, address)
}

// NewSquire.
func NewSquire(dialer Dialer, router router.Router) *Squire {
	return &Squire{
		Dialer: dialer,
		Direct: &Direct{},
		Router: router,
	}
}
