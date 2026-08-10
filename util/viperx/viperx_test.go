package viperx

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

type decodeHookFixture struct {
	Duration time.Duration `mapstructure:"duration"`
	List     []string      `mapstructure:"list"`
}

func FuzzDefaultDecodeHooks(f *testing.F) {
	f.Add("1s", "one,two")
	f.Add("1m30s", "")
	f.Add("not-a-duration", "one")
	f.Add("-1ns", "one,,three")

	f.Fuzz(func(t *testing.T, duration, list string) {
		if len(duration) > 4<<10 || len(list) > 64<<10 {
			t.Skip()
		}

		decode := func() (decodeHookFixture, error) {
			cfg := NewViperx()
			cfg.Set("fuzz.duration", duration)
			cfg.Set("fuzz.list", list)
			var result decodeHookFixture
			err := cfg.UnmarshalKey("fuzz", &result)
			return result, err
		}

		first, firstErr := decode()
		second, secondErr := decode()
		if (firstErr == nil) != (secondErr == nil) || (firstErr != nil && firstErr.Error() != secondErr.Error()) {
			t.Fatalf("decode hook result is not deterministic: first=%v second=%v", firstErr, secondErr)
		}
		if firstErr != nil {
			if !strings.Contains(strings.ToLower(firstErr.Error()), "duration") &&
				!strings.Contains(strings.ToLower(firstErr.Error()), "list") {
				t.Fatalf("decode error does not identify a field: %v", firstErr)
			}
			return
		}
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("decode hook output is not deterministic: first=%#v second=%#v", first, second)
		}
		if expected, err := time.ParseDuration(duration); err != nil {
			t.Fatalf("mapstructure accepted invalid duration %q", duration)
		} else if first.Duration != expected {
			t.Fatalf("duration mismatch: got %s, want %s", first.Duration, expected)
		}
	})
}
