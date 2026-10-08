package compression

import (
	"bytes"
	"compress/zlib"
	"io"
	"path/filepath"
	"testing"

	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var ins = []struct {
	name string
	data string
}{
	{"compression_deflate_test_1", "test"},
	{"compression_deflate_test_2", "{a:1,b:2}"},
	{"compression_deflate_test_3", "Neque porro quisquam est qui dolorem ipsum quia dolor sit amet, consectetur, adipisci velit"},
}

func TestCompressionDeflate(t *testing.T) {
	for _, in := range ins {
		t.Run(in.name, func(t *testing.T) {
			b, err := DeflateData([]byte(in.data))
			require.NoError(t, err)
			// Deflate output may change between Go releases. Its contract is
			// a standards-compliant zlib stream containing the original bytes.
			reader, err := zlib.NewReader(bytes.NewReader(b))
			require.NoError(t, err)
			decoded, err := io.ReadAll(reader)
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			assert.Equal(t, []byte(in.data), decoded)
		})
	}
}

func TestCompressionInflate(t *testing.T) {
	for _, in := range ins {
		t.Run(in.name, func(t *testing.T) {
			inputFile := filepath.Join("fixtures", in.name+".golden")
			input := helpers.ReadFile(t, inputFile)

			result, err := InflateData(input)
			require.NoError(t, err)

			assert.Equal(t, string(result), in.data)
		})
	}
}

func TestCompressionInflateIncorrectData(t *testing.T) {
	t.Run("compression_deflate_incorrect_data", func(t *testing.T) {
		input := "arbitrary data"

		result, err := InflateData([]byte(input))
		require.Error(t, err)
		assert.Nil(t, result)
	})
}
