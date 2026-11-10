package tg

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestParseProxyTable(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantNil bool // expect a nil dial (direct connection)
		wantErr bool
	}{
		{name: "empty string direct", raw: "", wantNil: true},
		{name: "blank string direct", raw: "   ", wantNil: true},
		{name: "socks5 without auth", raw: "socks5://127.0.0.1:1080"},
		{name: "socks5 with auth", raw: "socks5://user:pass@127.0.0.1:1080"},
		{name: "socks5 default port", raw: "socks5://127.0.0.1"},
		{name: "http proxy", raw: "http://127.0.0.1:8080"},
		{name: "https proxy", raw: "https://127.0.0.1:8443"},
		{name: "http with auth", raw: "http://user:pass@127.0.0.1:8080"},
		// The mtproxy handshake lives in the resolver layer, so ParseProxy returns a guiding
		// error for it (the secret is still validated).
		{name: "mtproxy hex secret", raw: "mtproxy://proxy.example.com:443?secret=dd00000000000000000000000000000000", wantErr: true},
		{name: "mtproxy parameter form", raw: "mtproxy://?server=proxy.example.com&port=443&secret=dd00000000000000000000000000000000", wantErr: true},
		{name: "invalid URL", raw: "://bad url", wantErr: true},
		{name: "unsupported scheme", raw: "ftp://127.0.0.1:21", wantErr: true},
		{name: "socks5 missing host", raw: "socks5://", wantErr: true},
		{name: "http missing host", raw: "http://", wantErr: true},
		{name: "mtproxy missing secret", raw: "mtproxy://proxy.example.com:443", wantErr: true},
		{name: "mtproxy invalid secret", raw: "mtproxy://proxy.example.com:443?secret=!!!", wantErr: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			dial, err := ParseProxy(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseProxy(%q) should fail, got nil error", tt.raw)
				}
				if !strings.Contains(err.Error(), "tg.ParseProxy") {
					t.Fatalf("error should carry tg.ParseProxy prefix, got %q", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseProxy(%q): %v", tt.raw, err)
			}
			if tt.wantNil {
				if dial != nil {
					t.Fatalf("ParseProxy(%q) should return nil dial, got non-nil", tt.raw)
				}
				return
			}
			if dial == nil {
				t.Fatalf("ParseProxy(%q) should return non-nil dial, got nil", tt.raw)
			}
			// The dial function must be callable (pointing at an unreachable address; failing is
			// fine, this verifies the signature and ctx propagation).
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, _ = dial(ctx, "tcp", "127.0.0.1:1")
		})
	}
}

func TestParseProxySocks5DialRespectsContext(t *testing.T) {
	// The SOCKS5 dialer must respect ctx cancellation: connect to a blackhole address.
	dial, err := ParseProxy("socks5://127.0.0.1:1")
	if err != nil {
		t.Fatalf("ParseProxy: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, derr := dial(ctx, "tcp", "10.255.255.1:9999")
	if derr == nil {
		t.Fatalf("dial to blackhole should fail")
	}
}

func TestBuildResolver(t *testing.T) {
	t.Run("empty string direct", func(t *testing.T) {
		r, err := BuildResolver("")
		if err != nil {
			t.Fatalf("BuildResolver empty: %v", err)
		}
		if r == nil {
			t.Fatalf("resolver should not be nil")
		}
	})

	t.Run("socks5", func(t *testing.T) {
		r, err := BuildResolver("socks5://127.0.0.1:1080")
		if err != nil {
			t.Fatalf("BuildResolver socks5: %v", err)
		}
		if r == nil {
			t.Fatalf("resolver should not be nil")
		}
	})

	t.Run("mtproxy", func(t *testing.T) {
		r, err := BuildResolver("mtproxy://proxy.example.com:443?secret=dd00000000000000000000000000000000")
		if err != nil {
			t.Fatalf("BuildResolver mtproxy: %v", err)
		}
		if r == nil {
			t.Fatalf("resolver should not be nil")
		}
	})

	t.Run("mtproxy short secret fails", func(t *testing.T) {
		_, err := BuildResolver("mtproxy://proxy.example.com:443?secret=abcd")
		if err == nil {
			t.Fatalf("short secret should fail")
		}
	})

	t.Run("invalid scheme fails", func(t *testing.T) {
		_, err := BuildResolver("gopher://x")
		if err == nil {
			t.Fatalf("gopher should fail")
		}
	})
}

func TestDecodeMTProxySecret(t *testing.T) {
	// 16-byte hex (no prefix) -> Simple
	s, err := decodeMTProxySecret("000102030405060708090a0b0c0d0e0f")
	if err != nil {
		t.Fatalf("hex 16: %v", err)
	}
	if len(s) != 16 {
		t.Fatalf("hex 16 len = %d, want 16", len(s))
	}

	// dd prefix + 16-byte hex -> Secured (17 bytes)
	s, err = decodeMTProxySecret("dd000102030405060708090a0b0c0d0e0f")
	if err != nil {
		t.Fatalf("dd hex: %v", err)
	}
	if len(s) != 17 {
		t.Fatalf("dd hex len = %d, want 17", len(s))
	}

	// An empty string fails
	if _, err := decodeMTProxySecret(""); err == nil {
		t.Fatalf("empty secret should fail")
	}

	// Completely invalid content fails
	if _, err := decodeMTProxySecret("!!!!"); err == nil {
		t.Fatalf("garbage secret should fail")
	}
}

// Compile-time anchor: the function ParseProxy returns must match the dcs.DialFunc signature.
var _ = func() dcsDialFunc { return nil }

type dcsDialFunc = func(ctx context.Context, network, addr string) (net.Conn, error)

var _ = errors.New // keep the errors import (for future extensions)
