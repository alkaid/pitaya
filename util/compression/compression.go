package compression

import (
	"bytes"
	"compress/zlib"
	"errors"
	"fmt"
	"io"
	"math"
)

var ErrInflatedDataTooLarge = errors.New("inflated data exceeds limit")

func DeflateData(data []byte) ([]byte, error) {
	var bb bytes.Buffer
	z := zlib.NewWriter(&bb)
	_, err := z.Write(data)
	if err != nil {
		return nil, err
	}
	z.Close()
	return bb.Bytes(), nil
}

func InflateData(data []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	return io.ReadAll(zr)
}

// InflateDataLimit inflates data while rejecting output larger than maxSize.
func InflateDataLimit(data []byte, maxSize int) ([]byte, error) {
	if maxSize < 0 || uint64(maxSize) >= uint64(math.MaxInt64) {
		return nil, fmt.Errorf("invalid inflated data limit: %d", maxSize)
	}

	zr, err := zlib.NewReader(bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	limited := &io.LimitedReader{R: zr, N: int64(maxSize) + 1}
	result, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(result) > maxSize {
		return nil, fmt.Errorf("%w: maximum %d bytes", ErrInflatedDataTooLarge, maxSize)
	}
	return result, nil
}

func IsCompressed(data []byte) bool {
	return len(data) > 2 &&
		(
		// zlib
		(data[0] == 0x78 &&
			(data[1] == 0x9C ||
				data[1] == 0x01 ||
				data[1] == 0xDA ||
				data[1] == 0x5E)) ||
			// gzip
			(data[0] == 0x1F && data[1] == 0x8B))
}
