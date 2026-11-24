package tg

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gotd/td/telegram/dcs"
	"golang.org/x/net/proxy"
)

// ParseProxy parses a proxy_url string and returns a dial function usable as a dcs.DialFunc.
//
// Supported schemes:
//   - ""           -> returns a nil dial (the caller should connect directly)
//   - socks5://    -> SOCKS5 from golang.org/x/net/proxy
//   - http(s)://   -> HTTP/HTTPS CONNECT tunnel (the HTTPBasicAuth payload carries user/password)
//   - mtproxy://   -> MTProto proxy (secret accepts hex or base64, optionally with a dd prefix)
//
// Invalid input returns a readable error.
func ParseProxy(raw string) (dial func(ctx context.Context, network, addr string) (net.Conn, error), err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("tg.ParseProxy: invalid proxy URL %q: %w", raw, err)
	}

	switch strings.ToLower(u.Scheme) {
	case "socks5":
		return socks5Dialer(u)
	case "http", "https":
		return httpDialer(u)
	case "mtproxy":
		return mtproxyDialer(u)
	default:
		return nil, fmt.Errorf("tg.ParseProxy: unsupported proxy scheme %q (supported: socks5 / http / https / mtproxy)", u.Scheme)
	}
}

// socks5Dialer builds a SOCKS5 dial function on top of golang.org/x/net/proxy.
// It supports user:password authentication (read from the URL userinfo).
func socks5Dialer(u *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("tg.ParseProxy: socks5 proxy is missing a hostname: %q", u.String())
	}
	port := u.Port()
	if port == "" {
		port = "1080"
	}
	addr := net.JoinHostPort(host, port)

	var auth *proxy.Auth
	if u.User != nil {
		pwd, _ := u.User.Password()
		auth = &proxy.Auth{User: u.User.Username(), Password: pwd}
	}

	d, err := proxy.SOCKS5("tcp", addr, auth, proxy.Direct)
	if err != nil {
		return nil, fmt.Errorf("tg.ParseProxy: create SOCKS5 dialer: %w", err)
	}
	cd, ok := d.(proxy.ContextDialer)
	if !ok {
		return nil, fmt.Errorf("tg.ParseProxy: SOCKS5 dialer does not support context")
	}
	return cd.DialContext, nil
}

// httpDialer builds an HTTP/HTTPS CONNECT tunnel dial function.
//
// Why the CONNECT handshake is written by hand instead of using http.Transport: Transport
// never exposes the underlying net.Conn of the tunnel as resp.Body, so the connection it
// hands back cannot be used as a bare TCP stream and gotd session initialization never
// becomes ready. This code performs the CONNECT handshake itself and returns the bare
// connection.
func httpDialer(u *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("tg.ParseProxy: http(s) proxy is missing a hostname: %q", u.String())
	}
	useTLS := strings.EqualFold(u.Scheme, "https")
	port := u.Port()
	if port == "" {
		if useTLS {
			port = "443"
		} else {
			port = "80"
		}
	}
	proxyAddr := net.JoinHostPort(host, port)

	// The proxy authorization header only needs to be computed once, when the dial function is built.
	var authHeader string
	if u.User != nil {
		pwd, _ := u.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(u.User.Username() + ":" + pwd))
		authHeader = "Proxy-Authorization: Basic " + token + "\r\n"
	}

	return func(ctx context.Context, _, addr string) (net.Conn, error) {
		d := net.Dialer{
			KeepAlive: 30 * time.Second, // enable TCP KeepAlive so proxy software or routers do not drop idle long-lived connections
		}
		rawConn, err := d.DialContext(ctx, "tcp", proxyAddr)
		if err != nil {
			return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
		}
		conn := net.Conn(rawConn)
		if useTLS {
			tlsConn := tls.Client(rawConn, &tls.Config{
				ServerName: host,
				MinVersion: tls.VersionTLS12,
			})
			if herr := tlsConn.HandshakeContext(ctx); herr != nil {
				_ = rawConn.Close()
				return nil, fmt.Errorf("TLS handshake with proxy %s: %w", proxyAddr, herr)
			}
			conn = tlsConn
		}
		// Use the ctx deadline during the handshake so a dead proxy cannot block forever.
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}

		// Watch ctx cancellation only during the CONNECT handshake: if ctx times out or is
		// cancelled while handshaking, close the connection and return immediately.
		// Calling stop() before returning guarantees the callback will never fire on the live
		// connection handed to the caller; if stop() returns false, cancellation already won.
		stop := context.AfterFunc(ctx, func() {
			_ = conn.Close()
		})
		defer stop()

		req := "CONNECT " + addr + " HTTP/1.1\r\nHost: " + addr + "\r\n" + authHeader + "\r\n"
		if _, werr := conn.Write([]byte(req)); werr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("send CONNECT request: %w", werr)
		}

		br := bufio.NewReader(conn)
		resp, rerr := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
		if rerr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("read CONNECT response: %w", rerr)
		}
		if resp.StatusCode != http.StatusOK {
			_ = conn.Close()
			return nil, fmt.Errorf("proxy returned status code %d", resp.StatusCode)
		}
		if !stop() {
			_ = conn.Close()
			return nil, ctx.Err()
		}
		_ = conn.SetDeadline(time.Time{})

		// bufio may already hold bytes that arrived after the tunnel opened; they must be
		// handed to the caller as well.
		return &proxyTunnelConn{Conn: conn, r: br}, nil
	}, nil
}

// proxyTunnelConn is the tunnel connection after the CONNECT handshake: Read goes through
// bufio so bytes buffered during the handshake are not lost.
type proxyTunnelConn struct {
	net.Conn
	r *bufio.Reader
}

// Read reads from bufio first so bytes buffered during the handshake are not lost.
func (c *proxyTunnelConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// mtproxyDialer builds the dial function for an MTProto proxy.
// The URL looks like mtproxy://host:port?secret=<hex-or-base64> and also accepts the
// host/port/secret parameter style of https://t.me/proxy?server=...&port=...&secret=...
func mtproxyDialer(u *url.URL) (func(context.Context, string, string) (net.Conn, error), error) {
	q := u.Query()
	host := u.Hostname()
	port := u.Port()
	// Accept the server/port parameter form as well
	if host == "" {
		host = q.Get("server")
	}
	if port == "" {
		port = q.Get("port")
	}
	if host == "" || port == "" {
		return nil, fmt.Errorf("tg.ParseProxy: mtproxy address is missing host or port: %q", u.String())
	}
	secretStr := q.Get("secret")
	if secretStr == "" {
		return nil, fmt.Errorf("tg.ParseProxy: mtproxy is missing the secret parameter: %q", u.String())
	}

	secret, err := decodeMTProxySecret(secretStr)
	if err != nil {
		return nil, fmt.Errorf("tg.ParseProxy: %w", err)
	}
	// Validate up front that dcs.MTProxy can parse the secret (tag validity), so a readable
	// error is returned.
	addr := net.JoinHostPort(host, port)
	if _, err := dcs.MTProxy(addr, secret, dcs.MTProxyOptions{}); err != nil {
		return nil, fmt.Errorf("tg.ParseProxy: create MTProto proxy: %w", err)
	}
	// The mtproxy handshake happens inside the resolver layer and cannot be reduced to a
	// plain dial function; return a clear error that points the caller at BuildResolver.
	return nil, fmt.Errorf("tg.ParseProxy: mtproxy must be used through BuildResolver (the handshake lives in the resolver layer)")
}

// decodeMTProxySecret decodes an MTProto proxy secret: hex (the common case) or base64url.
// It accepts a leading dd cloaked prefix (only the dd marker is stripped; the cloak domain
// stays in the secret and is handled by FakeTLS).
func decodeMTProxySecret(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("mtproxy secret is empty")
	}
	// dd prefix: the 0xdd marker byte followed by hex (per the Telegram MTProxy spec dd means
	// the Secured type and 0xdd is the tag byte, which must remain in the secret passed to
	// ParseSecret).
	if strings.HasPrefix(strings.ToLower(s), "dd") {
		rest := s[2:]
		if dec, err := hexDecodeString(rest); err == nil {
			return append([]byte{0xdd}, dec...), nil
		}
	}
	if dec, err := hexDecodeString(s); err == nil {
		return dec, nil
	}
	if dec, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return dec, nil
	}
	if dec, err := base64.StdEncoding.DecodeString(s); err == nil {
		return dec, nil
	}
	return nil, fmt.Errorf("mtproxy secret %q cannot be decoded as hex or base64", s)
}

// hexDecodeString is a thin wrapper around encoding/hex.DecodeString so tests can replace it.
func hexDecodeString(s string) ([]byte, error) {
	return hex.DecodeString(s)
}

// NewProxiedHTTPClient builds an http.Client that goes through the proxy for the Bot API.
//
// Why it is needed: the polling and message sending of go-telegram/bot use a separate HTTP
// client and do **not** reuse the MTProto dcs.DialFunc. In environments that can only reach
// the network through a proxy (measured: every direct connection times out), leaving that
// client unproxied makes getMe/getUpdates time out forever - the process starts and
// immediately reports "context deadline exceeded".
//
// An empty proxyURL returns (nil, nil) and the caller keeps the library's default client.
func NewProxiedHTTPClient(proxyURL string) (*http.Client, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return nil, nil
	}
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("tg: invalid proxy URL %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https", "":
	case "socks5":
		// The HTTP client only understands http(s) proxies; socks5 needs its own DialContext.
		return nil, fmt.Errorf("tg: the Bot API proxy does not support socks5 yet (MTProto does), configure an http(s) proxy for the Bot API")
	default:
		return nil, fmt.Errorf("tg: unsupported proxy scheme for the Bot API %q", u.Scheme)
	}
	transport := &http.Transport{
		Proxy:               http.ProxyURL(u),
		MaxIdleConns:        10,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 20 * time.Second,
	}
	return &http.Client{Transport: transport, Timeout: 120 * time.Second}, nil
}

// BuildResolver builds a gotd dcs.Resolver from proxyURL.
//
// An empty proxyURL -> dcs.Plain, a direct connection; otherwise ParseProxy supplies the
// dial function. mtproxy is special: it brings its own full handshake, so the resolver built
// by dcs.MTProxy is returned directly.
func BuildResolver(proxyURL string) (dcs.Resolver, error) {
	proxyURL = strings.TrimSpace(proxyURL)
	if proxyURL == "" {
		return dcs.Plain(dcs.PlainOptions{}), nil
	}

	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("tg.BuildResolver: invalid proxy URL %q: %w", proxyURL, err)
	}

	switch strings.ToLower(u.Scheme) {
	case "mtproxy":
		q := u.Query()
		host := u.Hostname()
		if host == "" {
			host = q.Get("server")
		}
		port := u.Port()
		if port == "" {
			port = q.Get("port")
		}
		secretStr := q.Get("secret")
		if host == "" || port == "" || secretStr == "" {
			return nil, fmt.Errorf("tg.BuildResolver: mtproxy requires host, port and secret: %q", proxyURL)
		}
		secret, err := decodeMTProxySecret(secretStr)
		if err != nil {
			return nil, fmt.Errorf("tg.BuildResolver: %w", err)
		}
		return dcs.MTProxy(net.JoinHostPort(host, port), secret, dcs.MTProxyOptions{})

	default:
		dial, err := ParseProxy(proxyURL)
		if err != nil {
			return nil, err
		}
		if dial == nil {
			return dcs.Plain(dcs.PlainOptions{}), nil
		}
		return dcs.Plain(dcs.PlainOptions{Dial: dial}), nil
	}
}
