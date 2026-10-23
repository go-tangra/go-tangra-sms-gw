package parity

import (
	"encoding/binary"
	"hash/fnv"
	"os"
)

// LegacyJSONStyle returns the protobuf-go output flavour ("spaced" or
// "compact", see public.legacy_json_style) of the Go binary at path. It
// reproduces google.golang.org/protobuf/internal/detrand: the seed is an FNV-64
// hash of the file size and eight 64-byte samples of the executable, and an
// odd seed adds the space after "," (and the no-break space after "proto:").
func LegacyJSONStyle(path string) (string, error) {
	f, err := os.Open(path) // #nosec G304 -- operator-supplied binary
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const numSamples = 8
	var buf [64]byte
	h := fnv.New64()
	binary.LittleEndian.PutUint64(buf[:8], uint64(fi.Size())) // #nosec G115 -- file size
	h.Write(buf[:8])
	for i := int64(0); i < numSamples; i++ {
		if _, err := f.ReadAt(buf[:], i*fi.Size()/numSamples); err != nil {
			return "", err
		}
		h.Write(buf[:])
	}
	if h.Sum64()%2 == 1 {
		return "spaced", nil
	}
	return "compact", nil
}
