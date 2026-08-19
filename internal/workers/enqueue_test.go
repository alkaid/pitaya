package workers

import (
	"bytes"
	"errors"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateJidFromReader(t *testing.T) {
	id, err := generateJidFrom(bytes.NewReader([]byte("abcdefghijkl")))
	require.NoError(t, err)
	assert.Equal(t, "6162636465666768696a6b6c", id)

	readErr := errors.New("random source failed")
	id, err = generateJidFrom(iotest.ErrReader(readErr))
	assert.Empty(t, id)
	assert.ErrorIs(t, err, readErr)
}
