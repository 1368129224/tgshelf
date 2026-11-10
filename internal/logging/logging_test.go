package logging

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// TestRedacter verifies redaction of regex patterns and exact secrets.
func TestRedacter(t *testing.T) {
	// All values are **synthetic placeholders**; never put real credentials here:
	// secretToken deliberately matches the shape of a Telegram bot token
	// (\d{8,11}:[A-Za-z0-9_-]{35}) so the regex redaction branch is really exercised,
	// but it is fake and invalid for any API.
	secretToken := "1000000000:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	secretHash := "0123456789abcdef0123456789abcdef"
	r := NewRedacter(secretToken, secretHash)

	cases := []struct {
		name      string
		input     string
		forbidden []string
	}{
		{
			name:      "phone number redaction",
			input:     "user phone is +8613800138000 and 13912345678",
			forbidden: []string{"13800138000", "13912345678"},
		},
		{
			name:      "proxy password redaction",
			input:     "connect via socks5://admin:SecretPass123@127.0.0.1:1080",
			forbidden: []string{"admin:SecretPass123@"},
		},
		{
			name:      "bot token regex redaction",
			input:     "got token 1234567890:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456789 in log",
			forbidden: []string{"1234567890:ABCDEFGHIJKLMNOPQRSTUVWXYZ123456789"},
		},
		{
			name:      "injected exact secret redaction",
			input:     fmt.Sprintf("token=%s, hash=%s", secretToken, secretHash),
			forbidden: []string{secretToken, secretHash},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := r.Redact(c.input)
			for _, f := range c.forbidden {
				if strings.Contains(out, f) {
					t.Fatalf("still contains sensitive data after redaction %q: %s", f, out)
				}
			}
		})
	}
}

// TestRedactingCore verifies that Message and Field are fully redacted on zap writes.
func TestRedactingCore(t *testing.T) {
	var buf bytes.Buffer
	enc := zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig())
	baseCore := zapcore.NewCore(enc, zapcore.AddSync(&buf), zapcore.DebugLevel)

	secretAPIHash := "abcdef0123456789abcdef0123456789"
	secretPhone := "+8613800138000"
	secretToken := "123456789:ABCdefGhIjkLmNoPqRsTuVwXyZ123456789"

	redacter := NewRedacter(secretAPIHash)
	core := WrapCore(baseCore, redacter)
	logger := zap.New(core)

	logger.Info("user logged in",
		zap.String("phone", secretPhone),
		zap.String("token", secretToken),
		zap.Error(errors.New("bad hash: "+secretAPIHash)),
	)

	logOutput := buf.String()
	for _, forbidden := range []string{secretPhone, secretToken, secretAPIHash} {
		if strings.Contains(logOutput, forbidden) {
			t.Fatalf("zap log output leaks sensitive data %q: %s", forbidden, logOutput)
		}
	}
}

// TestRotateWriter verifies that rotation happens once the file reaches maxBytes.
func TestRotateWriter(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "test.log")

	// 100-byte limit per file, 2 backups kept
	rw, err := NewRotateWriter(logFile, 100, 2)
	if err != nil {
		t.Fatalf("NewRotateWriter: %v", err)
	}
	defer func() { _ = rw.Close() }()

	// write 50 bytes
	data50 := bytes.Repeat([]byte("a"), 50)
	if _, err := rw.Write(data50); err != nil {
		t.Fatal(err)
	}
	// write 60 more bytes (triggers the first rotation)
	data60 := bytes.Repeat([]byte("b"), 60)
	if _, err := rw.Write(data60); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(logFile + ".1"); err != nil {
		t.Fatalf("expected a .1 backup file: %v", err)
	}

	// write 100 more bytes (triggers the second rotation)
	data100 := bytes.Repeat([]byte("c"), 100)
	if _, err := rw.Write(data100); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(logFile + ".2"); err != nil {
		t.Fatalf("expected a .2 backup file: %v", err)
	}
}
