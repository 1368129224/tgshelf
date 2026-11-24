// Package logging provides a logging component with automatic redaction of sensitive
// data, log rotation and structured context support.
package logging

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"go.uber.org/zap/zapcore"
)

var (
	// botTokenRe matches a Telegram bot token, including inside Bot API URLs such as
	// https://api.telegram.org/bot123456789:ABC.../getMe.
	botTokenRe = regexp.MustCompile(`(^|[^0-9A-Za-z_]|bot)\d{8,11}:[A-Za-z0-9_-]{35}\b`)

	// proxyAuthRe matches a proxy URL carrying credentials: scheme://user:pass@host
	proxyAuthRe = regexp.MustCompile(`([a-zA-Z0-9+.-]+://)[^/\s:@]+:[^/\s:@]+@`)

	// phoneIntlRe matches international phone numbers with a leading '+' (e.g. +8613800138000).
	phoneIntlRe = regexp.MustCompile(`(^|[^\d-])(\+\d{1,4})(\d{3})\d{4}(\d{4})\b`)

	// phoneCNRe matches standalone 11-digit mainland China mobile numbers (e.g. 13912345678)
	// without matching negative Telegram channel IDs (-100...) or longer numeric IDs.
	phoneCNRe = regexp.MustCompile(`(^|[^\d-])(1[3-9]\d)\d{4}(\d{4})\b`)
)

// Redacter redacts strings and fields.
type Redacter struct {
	mu           sync.RWMutex
	exactSecrets []string
}

// NewRedacter creates a redacter; pass known sensitive plaintext strings (such as
// bot_token and api_hash from the config).
func NewRedacter(secrets ...string) *Redacter {
	r := &Redacter{}
	r.AddSecrets(secrets...)
	return r
}

// AddSecrets appends known secrets dynamically (blank and very short words are ignored so
// ordinary words are not damaged).
func (r *Redacter) AddSecrets(secrets ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range secrets {
		trimmed := strings.TrimSpace(s)
		if len(trimmed) >= 6 {
			r.exactSecrets = append(r.exactSecrets, trimmed)
		}
	}
}

// Redact filters every known piece of sensitive data out of the text.
func (r *Redacter) Redact(text string) string {
	if text == "" {
		return ""
	}

	// 1. replace known exact secrets first
	r.mu.RLock()
	secrets := r.exactSecrets
	r.mu.RUnlock()

	res := text
	for _, secret := range secrets {
		if strings.Contains(res, secret) {
			res = strings.ReplaceAll(res, secret, "[REDACTED]")
		}
	}

	// 2. match generic credential patterns with regular expressions
	// 2.1 bot token (preserving any leading delimiter or "bot" URL prefix)
	res = botTokenRe.ReplaceAllString(res, "${1}[REDACTED_BOT_TOKEN]")

	// 2.2 proxy credentials: replace user:pass@ with ***:***@ while preserving scheme://
	res = proxyAuthRe.ReplaceAllString(res, "${1}***:***@")

	// 2.3 phone number mask: keep the first 3 and last 4 digits and star the middle 4
	// (e.g. +86138****8000 or 139****5678)
	res = phoneIntlRe.ReplaceAllString(res, "${1}${2}${3}****${4}")
	res = phoneCNRe.ReplaceAllString(res, "${1}${2}****${3}")

	return res
}

// redactField recursively redacts sensitive content in a zap field.
func (r *Redacter) redactField(f zapcore.Field) zapcore.Field {
	switch f.Type {
	case zapcore.StringType:
		f.String = r.Redact(f.String)
	case zapcore.ByteStringType:
		if b, ok := f.Interface.([]byte); ok {
			f.Interface = []byte(r.Redact(string(b)))
		}
	case zapcore.ErrorType:
		if err, ok := f.Interface.(error); ok && err != nil {
			f.Interface = &redactedError{msg: r.Redact(err.Error())}
		}
	case zapcore.StringerType:
		if s, ok := f.Interface.(fmt.Stringer); ok && s != nil {
			f.Interface = &redactedStringer{text: r.Redact(s.String())}
		}
	case zapcore.ArrayMarshalerType:
		if am, ok := f.Interface.(zapcore.ArrayMarshaler); ok && am != nil {
			f.Interface = &redactedArrayMarshaler{am: am, r: r}
		}
	case zapcore.ObjectMarshalerType:
		if om, ok := f.Interface.(zapcore.ObjectMarshaler); ok && om != nil {
			f.Interface = &redactedObjectMarshaler{om: om, r: r}
		}
	default:
		// try to redact common cases for other types carrying a String value or Any
		if str, ok := f.Interface.(string); ok {
			f.Interface = r.Redact(str)
		}
	}
	return f
}

type redactedError struct {
	msg string
}

func (e *redactedError) Error() string { return e.msg }

type redactedStringer struct {
	text string
}

func (s *redactedStringer) String() string { return s.text }

type redactedArrayMarshaler struct {
	am zapcore.ArrayMarshaler
	r  *Redacter
}

func (m *redactedArrayMarshaler) MarshalLogArray(enc zapcore.ArrayEncoder) error {
	return m.am.MarshalLogArray(&redactingArrayEncoder{ArrayEncoder: enc, r: m.r})
}

type redactingArrayEncoder struct {
	zapcore.ArrayEncoder
	r *Redacter
}

func (e *redactingArrayEncoder) AppendString(v string) {
	e.ArrayEncoder.AppendString(e.r.Redact(v))
}

func (e *redactingArrayEncoder) AppendByteString(v []byte) {
	e.ArrayEncoder.AppendByteString([]byte(e.r.Redact(string(v))))
}

func (e *redactingArrayEncoder) AppendArray(v zapcore.ArrayMarshaler) error {
	return e.ArrayEncoder.AppendArray(&redactedArrayMarshaler{am: v, r: e.r})
}

func (e *redactingArrayEncoder) AppendObject(v zapcore.ObjectMarshaler) error {
	return e.ArrayEncoder.AppendObject(&redactedObjectMarshaler{om: v, r: e.r})
}

type redactedObjectMarshaler struct {
	om zapcore.ObjectMarshaler
	r  *Redacter
}

func (m *redactedObjectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	return m.om.MarshalLogObject(&redactingObjectEncoder{ObjectEncoder: enc, r: m.r})
}

type redactingObjectEncoder struct {
	zapcore.ObjectEncoder
	r *Redacter
}

func (e *redactingObjectEncoder) AddString(key, value string) {
	e.ObjectEncoder.AddString(key, e.r.Redact(value))
}

func (e *redactingObjectEncoder) AddByteString(key string, value []byte) {
	e.ObjectEncoder.AddByteString(key, []byte(e.r.Redact(string(value))))
}

func (e *redactingObjectEncoder) AddArray(key string, v zapcore.ArrayMarshaler) error {
	return e.ObjectEncoder.AddArray(key, &redactedArrayMarshaler{am: v, r: e.r})
}

func (e *redactingObjectEncoder) AddObject(key string, v zapcore.ObjectMarshaler) error {
	return e.ObjectEncoder.AddObject(key, &redactedObjectMarshaler{om: v, r: e.r})
}

// RedactingCore is a redacting decorator wrapping zapcore.Core.
type RedactingCore struct {
	zapcore.Core
	redacter *Redacter
}

// WrapCore wraps an existing zapcore.Core with the redacting filter.
func WrapCore(core zapcore.Core, redacter *Redacter) zapcore.Core {
	if redacter == nil {
		redacter = NewRedacter()
	}
	return &RedactingCore{
		Core:     core,
		redacter: redacter,
	}
}

// With redacts fields before attaching them to a child logger.
func (c *RedactingCore) With(fields []zapcore.Field) zapcore.Core {
	redacted := make([]zapcore.Field, len(fields))
	for i, f := range fields {
		redacted[i] = c.redacter.redactField(f)
	}
	return &RedactingCore{
		Core:     c.Core.With(redacted),
		redacter: c.redacter,
	}
}

// Check checks a log entry.
func (c *RedactingCore) Check(ent zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(ent.Level) {
		return ce.AddCore(ent, c)
	}
	return ce
}

// Write redacts the Message and every Field before writing the entry.
func (c *RedactingCore) Write(ent zapcore.Entry, fields []zapcore.Field) error {
	ent.Message = c.redacter.Redact(ent.Message)
	redacted := make([]zapcore.Field, len(fields))
	for i, f := range fields {
		redacted[i] = c.redacter.redactField(f)
	}
	return c.Core.Write(ent, redacted)
}
