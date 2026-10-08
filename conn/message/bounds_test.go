package message

import (
	"github.com/stretchr/testify/require"
	"testing"
)

func TestDecodeMissingRouteLength(t *testing.T) {
	for _, data := range [][]byte{{byte(Request) << 1, 1}, {byte(Notify) << 1}, {byte(Push) << 1}} {
		require.NotPanics(t, func() {
			msg, err := Decode(data)
			require.ErrorIs(t, err, ErrInvalidMessage)
			require.Nil(t, msg)
		})
	}
}
