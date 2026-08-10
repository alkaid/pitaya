package message

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/alkaid/pitaya/v2/conn/codec"
	"github.com/alkaid/pitaya/v2/helpers"
	"github.com/alkaid/pitaya/v2/util/compression"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var update = flag.Bool("update", false, "update .golden files")

func resetDicts(t *testing.T) {
	t.Helper()
	routesCodesMutex.Lock()
	defer routesCodesMutex.Unlock()
	routes = make(map[string]uint16)
	codes = make(map[uint16]string)
}

func TestNew(t *testing.T) {
	t.Parallel()

	message := New()

	assert.NotNil(t, message)
}

var encodeTables = map[string]struct {
	message *Message
	routes  map[string]uint16
	msgErr  bool
	gzip    uint8
	err     error
}{
	"test_wrong_type": {&Message{Type: 0xff, Data: []byte{}}, nil, false, 0x0, ErrWrongMessageType},

	"test_request_type": {&Message{Type: Request, Route: "a", Data: []uint8{}}, nil, false, 0x0, nil},
	"test_request_type_compressed": {&Message{Type: Request, Route: "a", Data: []byte{}, compressed: true},
		map[string]uint16{"a": 1}, false, 0x0, nil},

	"test_notify_type": {&Message{Type: Notify, Route: "a", Data: []byte{}}, nil, false, 0x0, nil},
	"test_notify_type_compressed": {&Message{Type: Notify, Route: "a", Data: []byte{}, compressed: true},
		map[string]uint16{"a": 1}, false, 0x0, nil},

	"test_push_type": {&Message{Type: Push, Route: "a", Data: []byte{}}, nil, false, 0x0, nil},
	"test_push_type_compressed": {&Message{Type: Push, Route: "a", Data: []byte{}, compressed: true},
		map[string]uint16{"a": 1}, false, 0x0, nil},

	"test_reponse_type":           {&Message{Type: Response, Data: []byte{}}, nil, false, 0x0, nil},
	"test_reponse_type_with_data": {&Message{Type: Response, Data: []byte{0x01}}, nil, false, 0x0, nil},
	"test_reponse_type_with_id":   {&Message{Type: Response, ID: 129, Data: []byte{}}, nil, false, 0x0, nil},

	"test_reponse_type_with_error": {&Message{Type: Response, Data: []byte{0x01}, Err: true}, nil, true, 0x0, nil},
	"test_must_gzip": {&Message{Type: Response,
		Data: []byte("blablablablablablablablablablablablabla"), Err: true}, nil, true, 0x10, nil},
}

func TestEncode(t *testing.T) {
	for name, table := range encodeTables {
		t.Run(name, func(t *testing.T) {
			message := table.message
			SetDictionary(table.routes)

			messageEncoder := NewMessagesEncoder(table.gzip == 0x10)
			result, err := messageEncoder.Encode(message)
			gp := filepath.Join("fixtures", name+".golden")

			if *update {
				t.Log("updating golden file")
				helpers.WriteFile(t, gp, result)
			}

			expected := helpers.ReadFile(t, gp)

			if err == nil {
				assert.Equal(t, table.gzip, expected[0]&gzipMask)
				assert.Equal(t, expected, result)
			} else {
				assert.Nil(t, result)
			}

			assert.Equal(t, table.err, err)

			resetDicts(t)
		})
	}
}

var decodeTables = map[string]struct {
	message *Message
	routes  map[string]uint16
	msgErr  bool
	gzip    uint8
	err     error
}{
	"test_wrong_type": {&Message{Type: 0xff, Data: []byte{}}, nil, false, 0x0, ErrWrongMessageType},

	"test_request_type": {&Message{Type: Request, Route: "a", Data: []uint8{}}, nil, false, 0x0, nil},
	"test_request_type_compressed": {&Message{Type: Request, Route: "a", Data: []byte{}, compressed: true},
		map[string]uint16{"a": 1}, false, 0x0, nil},

	"test_notify_type": {&Message{Type: Notify, Route: "a", Data: []byte{}}, nil, false, 0x0, nil},
	"test_notify_type_compressed": {&Message{Type: Notify, Route: "a", Data: []byte{}, compressed: true},
		map[string]uint16{"a": 1}, false, 0x0, nil},

	"test_push_type": {&Message{Type: Push, Route: "a", Data: []byte{}}, nil, false, 0x0, nil},
	"test_push_type_compressed": {&Message{Type: Push, Route: "a", Data: []byte{}, compressed: true},
		map[string]uint16{"a": 1}, false, 0x0, nil},

	"test_reponse_type":           {&Message{Type: Response, Data: []byte{}}, nil, false, 0x0, nil},
	"test_reponse_type_with_data": {&Message{Type: Response, Data: []byte{0x01}}, nil, false, 0x0, nil},
	"test_reponse_type_with_id":   {&Message{Type: Response, ID: 129, Data: []byte{}}, nil, false, 0x0, nil},

	"test_reponse_type_with_error": {&Message{Type: Response, Data: []byte{0x01}, Err: true}, nil, true, 0x0, nil},
	"test_must_gzip": {&Message{Type: Response,
		Data: []byte("blablablablablablablablablablablablabla"), Err: true}, nil, true, 0x10, nil},
}

func TestDecode(t *testing.T) {
	for name, table := range decodeTables {
		t.Run(name, func(t *testing.T) {
			SetDictionary(table.routes)

			gp := filepath.Join("fixtures", name+".golden")
			encoded := helpers.ReadFile(t, gp)

			message, err := Decode(encoded)

			if err == nil {
				assert.Equal(t, table.message, message)
			}
			if name == "test_wrong_type" {
				assert.EqualError(t, ErrInvalidMessage, err.Error())
			} else {
				assert.Equal(t, table.err, err)
			}
			resetDicts(t)
		})
	}
}

func TestDecodeRejectsTruncatedRouteLength(t *testing.T) {
	message, err := Decode([]byte("00"))
	assert.Nil(t, message)
	assert.ErrorIs(t, err, ErrInvalidMessage)
}

func TestDecodeRejectsOversizedInflatedPayload(t *testing.T) {
	payload := bytes.Repeat([]byte{0}, codec.MaxPacketSize+1)
	compressed, err := compression.DeflateData(payload)
	require.NoError(t, err)
	frame := append([]byte{byte(Response)<<1 | gzipMask, 0}, compressed...)

	message, err := Decode(frame)
	assert.Nil(t, message)
	assert.ErrorIs(t, err, compression.ErrInflatedDataTooLarge)
}

var dictTables = map[string]struct {
	dicts  []map[string]uint16
	routes map[string]uint16
	codes  map[uint16]string
	err    error
}{
	"test_add_new_route": {[]map[string]uint16{{"a": 1}}, map[string]uint16{"a": 1},
		map[uint16]string{1: "a"}, nil},
	"test_add_new_routes": {[]map[string]uint16{{"a": 1}, {"b": 2}}, map[string]uint16{"a": 1, "b": 2},
		map[uint16]string{1: "a", 2: "b"}, nil},
	"test_override_route": {[]map[string]uint16{{"a": 1}, {"a": 2}}, map[string]uint16{"a": 1},
		map[uint16]string{1: "a"}, errors.New("duplicated route(route: a, code: 1)")},
	"test_override_code": {[]map[string]uint16{{"a": 1}, {"b": 1}}, map[string]uint16{"a": 1},
		map[uint16]string{1: "a"}, errors.New("duplicated route(route: b, code: 1)")},
}

func TestSetDictionary(t *testing.T) {
	for name, table := range dictTables {
		t.Run(name, func(t *testing.T) {
			for _, dict := range table.dicts {
				SetDictionary(dict)
			}

			assert.Equal(t, table.routes, routes)
			assert.Equal(t, table.codes, codes)

			resetDicts(t)
		})
	}
}

func TestSetDictionaryRace(t *testing.T) {
	defer resetDicts(t)

	done := make(chan bool, 2)

	setDictRace := func(dict map[string]uint16) {
		assert.Nil(t, SetDictionary(dict))
		done <- true
	}

	go setDictRace(map[string]uint16{"a": 1})
	go setDictRace(map[string]uint16{"b": 2})

	// wait for both setDictRace to finish
	<-done
	<-done

	expected_codes := map[uint16]string{1: "a", 2: "b"}
	assert.EqualValues(t, expected_codes, codes)

	expected_routes := map[string]uint16{"a": 1, "b": 2}
	assert.EqualValues(t, expected_routes, routes)
}

func TestGetDictionary(t *testing.T) {
	defer resetDicts(t)
	expected := map[string]uint16{"a": 1, "b": 2}
	assert.Nil(t, SetDictionary(expected))

	dict := GetDictionary()
	assert.Equal(t, expected, dict)

	// make sure we're copying the routes maps
	assert.NotEqual(t, fmt.Sprintf("%p", routes), fmt.Sprintf("%p", dict))
}

func FuzzMessageDecode(f *testing.F) {
	reset := func() {
		routesCodesMutex.Lock()
		defer routesCodesMutex.Unlock()
		routes = make(map[string]uint16)
		codes = make(map[uint16]string)
	}
	reset()
	f.Cleanup(reset)
	require.NoError(f, SetDictionary(map[string]uint16{"fuzz.route": 1}))

	seed, err := NewMessagesEncoder(true).Encode(&Message{
		Type:  Request,
		ID:    129,
		Route: "fuzz.route",
		Data:  []byte("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
	})
	require.NoError(f, err)

	f.Add([]byte{})
	f.Add([]byte{byte(Request) << 1})
	f.Add([]byte{byte(Notify) << 1, 1, 'a'})
	f.Add(seed)
	f.Add([]byte{0xff, 0})

	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 64<<10 {
			t.Skip()
		}

		decoded, err := Decode(input)
		if err != nil {
			return
		}
		if len(decoded.Data) > codec.MaxPacketSize {
			t.Fatalf("decoded payload exceeds limit: %d", len(decoded.Data))
		}

		encoded, err := NewMessagesEncoder(false).Encode(&Message{
			Type:  decoded.Type,
			ID:    decoded.ID,
			Route: decoded.Route,
			Data:  append([]byte(nil), decoded.Data...),
			Err:   decoded.Err,
		})
		if err != nil {
			t.Fatalf("decoded message could not be re-encoded: %v", err)
		}
		roundTrip, err := Decode(encoded)
		if err != nil {
			t.Fatalf("round-trip decode failed: %v", err)
		}
		assert.Equal(t, decoded.Type, roundTrip.Type)
		assert.Equal(t, decoded.ID, roundTrip.ID)
		assert.Equal(t, decoded.Route, roundTrip.Route)
		assert.Equal(t, decoded.Data, roundTrip.Data)
		assert.Equal(t, decoded.Err, roundTrip.Err)
	})
}
