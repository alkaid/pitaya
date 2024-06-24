package viperx

import (
	"github.com/mitchellh/mapstructure"
	"reflect"
	"time"

	"github.com/spf13/cast"
	"github.com/spf13/viper"
)

// Viperx 自定义viper,主要是修改是
//
// 1.time的默认时区由UTC改为local
//
// 2.反序列化添加时间转换处理
type Viperx struct {
	*viper.Viper
}

func NewViperx() *Viperx {
	return &Viperx{viper.New()}
}

// GetTime content from viper.GetTime
//
// 区别是默认时区由UTC改为local 使符合 https://en.wikipedia.org/wiki/ISO_8601#Local_time_(unqualified)
//
//	@receiver v
//	@param key
//	@return time.Time
func (v *Viperx) GetTime(key string) time.Time {
	return cast.ToTimeInDefaultLocation(v.Get(key), time.Local)
}

// Unmarshal content from viper.Unmarshal
//
// 区别是加了时间转换 DefaultToTimeHookFunc
func (v *Viperx) Unmarshal(rawVal any, opts ...viper.DecoderConfigOption) error {
	beforeOpts := []viper.DecoderConfigOption{DefaultDecoderOpts()}
	beforeOpts = append(beforeOpts, opts...)
	return v.Viper.Unmarshal(rawVal, beforeOpts...)
}

// UnmarshalExact content from viper.UnmarshalExact
//
// 区别是加了时间转换 DefaultToTimeHookFunc
func (v *Viperx) UnmarshalExact(rawVal any, opts ...viper.DecoderConfigOption) error {
	beforeOpts := []viper.DecoderConfigOption{DefaultDecoderOpts()}
	beforeOpts = append(beforeOpts, opts...)
	return v.Viper.UnmarshalExact(rawVal, beforeOpts...)
}

// UnmarshalKey content from viper.UnmarshalKey
//
// 区别是加了时间转换 DefaultToTimeHookFunc
func (v *Viperx) UnmarshalKey(key string, rawVal any, opts ...viper.DecoderConfigOption) error {
	beforeOpts := []viper.DecoderConfigOption{DefaultDecoderOpts()}
	beforeOpts = append(beforeOpts, opts...)
	return v.Viper.UnmarshalKey(key, rawVal, beforeOpts...)
}

// Unmarshal content from viper.Unmarshal
//
// 区别是加了时间转换 DefaultToTimeHookFunc
func Unmarshal(rawVal any, opts ...viper.DecoderConfigOption) error {
	beforeOpts := []viper.DecoderConfigOption{DefaultDecoderOpts()}
	beforeOpts = append(beforeOpts, opts...)
	return viper.Unmarshal(rawVal, beforeOpts...)
}

// UnmarshalExact content from viper.UnmarshalExact
//
// 区别是加了时间转换 DefaultToTimeHookFunc
func UnmarshalExact(rawVal any, opts ...viper.DecoderConfigOption) error {
	beforeOpts := []viper.DecoderConfigOption{DefaultDecoderOpts()}
	beforeOpts = append(beforeOpts, opts...)
	return viper.UnmarshalExact(rawVal, beforeOpts...)
}

// UnmarshalKey content from viper.UnmarshalKey
//
// 区别是加了时间转换 DefaultToTimeHookFunc
func UnmarshalKey(key string, rawVal any, opts ...viper.DecoderConfigOption) error {
	beforeOpts := []viper.DecoderConfigOption{DefaultDecoderOpts()}
	beforeOpts = append(beforeOpts, opts...)
	return viper.UnmarshalKey(key, rawVal, beforeOpts...)
}

// DefaultHookFunc 默认decode hook,相比viper源码,多了time转换 DefaultToTimeHookFunc
//
//	@return mapstructure.DecodeHookFunc
func DefaultHookFunc() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
		DefaultToTimeHookFunc(),
	)
}

// DefaultToTimeHookFunc 默认时间转换,用于自定义viper解码
//
//	@return mapstructure.DecodeHookFunc
func DefaultToTimeHookFunc() mapstructure.DecodeHookFunc {
	return func(f reflect.Type, t reflect.Type, data any) (any, error) {
		if f.Kind() != reflect.String {
			return data, nil
		}
		if t != reflect.TypeOf(time.Time{}) {
			return data, nil
		}
		asString := data.(string)
		if asString == "" {
			return time.Time{}, nil
		}
		// 注意这里和viper源码GetTime的区别是这里默认时区不是utc而是local
		return cast.ToTimeInDefaultLocationE(data, time.Local)
	}
}

// DefaultDecoderOpts 默认反序列化时的DecodeHook选项 引用 DefaultHookFunc
//
//	@receiver m
//	@param d
func DefaultDecoderOpts() viper.DecoderConfigOption {
	return func(d *mapstructure.DecoderConfig) {
		//d.WeaklyTypedInput = true
		d.DecodeHook = DefaultHookFunc()
	}
}
