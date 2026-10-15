package header

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// LayerID names a layer: the blocks one snapshot generation changed, stored as one object per
// file (E2B calls it a build). It is 16 bytes, written as a UUID, so the header format keeps
// E2B's layout byte for byte.
type LayerID [16]byte

var (
	// Empty marks a range that reads as zeros and is stored nowhere (E2B's uuid.Nil).
	Empty LayerID
	// Base marks a range that is unchanged from the file's base: for a disk, the template image
	// the nodelet builds or has cached, which the store never holds. Its storage offset is the
	// offset in the base file. A memory file has no base and never uses it. Loam's addition.
	Base = LayerID{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
)

// NewLayerID returns a random (version 4) layer ID.
func NewLayerID() LayerID {
	var id LayerID
	rand.Read(id[:])
	id[6] = id[6]&0x0f | 0x40
	id[8] = id[8]&0x3f | 0x80
	return id
}

// ParseLayerID parses the canonical 36-character UUID form.
func ParseLayerID(s string) (LayerID, error) {
	var id LayerID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return id, fmt.Errorf("layer id %q: not a UUID", s)
	}
	h := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(id[:], []byte(h)); err != nil {
		return id, fmt.Errorf("layer id %q: %w", s, err)
	}
	return id, nil
}

// IsStored reports whether the ID names a layer object in the store, rather than Empty or Base.
func (id LayerID) IsStored() bool { return id != Empty && id != Base }

func (id LayerID) String() string {
	h := hex.EncodeToString(id[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (id LayerID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }

func (id *LayerID) UnmarshalText(b []byte) error {
	v, err := ParseLayerID(string(b))
	if err != nil {
		return err
	}
	*id = v
	return nil
}
