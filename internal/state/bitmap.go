package state

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/bits"
)

// Bitmap is the bitmap type used to track resume progress.
//
// # Bit order convention
//
// Bit index i maps to byte data[i/8], bit i%8 within that byte (LSB = bit 0):
//
//	i=0  → data[0] & 0x01 (bit0, least significant bit)
//	i=7  → data[0] & 0x80 (bit7, most significant bit)
//	i=8  → data[1] & 0x01 (bit0 of byte 1)
//	i=63 → data[7] & 0x80
//	i=64 → data[8] & 0x01
//
// This matches encoding/binary little-endian byte order and the LSB convention of Go math/bits as well.
type Bitmap struct {
	data []byte
}

// NewBitmap creates an empty bitmap holding n bits (all bits start at 0).
// It returns an empty bitmap when n <= 0.
func NewBitmap(n int) *Bitmap {
	if n <= 0 {
		return &Bitmap{}
	}
	return &Bitmap{data: make([]byte, (n+7)/8)}
}

// grow ensures data can hold bit i (growing on demand).
func (b *Bitmap) grow(i int) {
	need := i/8 + 1
	if need > len(b.data) {
		b.data = append(b.data, make([]byte, need-len(b.data))...)
	}
}

// Set sets bit i to v (true=1, false=0).
// It returns an error when i < 0; when i exceeds the current capacity the bitmap grows on demand.
func (b *Bitmap) Set(i int, v bool) error {
	if i < 0 {
		return fmt.Errorf("bitmap: negative index %d", i)
	}
	b.grow(i)
	if v {
		b.data[i/8] |= 1 << uint(i%8)
	} else {
		b.data[i/8] &^= 1 << uint(i%8)
	}
	return nil
}

// Get reads the value of bit i.
// It returns false and an error when i < 0 or i is out of range (never panics).
func (b *Bitmap) Get(i int) (bool, error) {
	if i < 0 {
		return false, fmt.Errorf("bitmap: negative index %d", i)
	}
	if i/8 >= len(b.data) {
		return false, fmt.Errorf("bitmap: index %d out of range (capacity %d bits)", i, len(b.data)*8)
	}
	return b.data[i/8]>>uint(i%8)&1 == 1, nil
}

// Count returns the number of set bits (Hamming weight).
func (b *Bitmap) Count() int {
	n := 0
	for _, by := range b.data {
		n += bits.OnesCount8(by)
	}
	return n
}

// Len returns the current capacity in bits (byte count * 8).
func (b *Bitmap) Len() int { return len(b.data) * 8 }

// Bytes returns a copy of the underlying byte slice (the caller may modify it safely).
func (b *Bitmap) Bytes() []byte {
	out := make([]byte, len(b.data))
	copy(out, b.data)
	return out
}

// MarshalJSON serializes the bitmap to a base64-encoded JSON string.
func (b Bitmap) MarshalJSON() ([]byte, error) {
	s := base64.StdEncoding.EncodeToString(b.data)
	return json.Marshal(s)
}

// UnmarshalJSON decodes the bitmap from a base64-encoded JSON string.
func (b *Bitmap) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("bitmap: unmarshal: %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return fmt.Errorf("bitmap: base64 decode: %w", err)
	}
	b.data = decoded
	return nil
}
