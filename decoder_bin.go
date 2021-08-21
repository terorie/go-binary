package bin

import (
	"fmt"
	"reflect"

	"go.uber.org/zap"
)

func (dec *Decoder) decodeWithOptionBin(v interface{}, option *option) (err error) {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr {
		return &InvalidDecoderError{reflect.TypeOf(v)}
	}

	// We decode rv not rv.Elem because the Unmarshaler interface
	// test must be applied at the top level of the value.
	err = dec.decodeBin(rv, option)
	if err != nil {
		return err
	}
	return nil
}

func (dec *Decoder) decodeBin(rv reflect.Value, opt *option) (err error) {
	if opt == nil {
		opt = newDefaultOption()
	}
	dec.currentFieldOpt = opt

	unmarshaler, rv := indirect(rv, opt.isOptional())

	if traceEnabled {
		zlog.Debug("decode: type",
			zap.Stringer("value_kind", rv.Kind()),
			zap.Bool("has_unmarshaler", (unmarshaler != nil)),
			zap.Reflect("options", opt),
		)
	}

	if opt.isOptional() {
		isPresent, e := dec.ReadByte()
		if e != nil {
			err = fmt.Errorf("decode: %t isPresent, %s", rv.Type(), e)
			return
		}

		if isPresent == 0 {
			if traceEnabled {
				zlog.Debug("decode: skipping optional value", zap.Stringer("type", rv.Kind()))
			}

			rv.Set(reflect.Zero(rv.Type()))
			return
		}

		// we have ptr here we should not go get the element
		unmarshaler, rv = indirect(rv, false)
	}

	if unmarshaler != nil {
		if traceEnabled {
			zlog.Debug("decode: using UnmarshalBinary method to decode type")
		}
		return unmarshaler.UnmarshalBinary(dec)
	}
	rt := rv.Type()

	switch rv.Kind() {
	case reflect.String:
		s, e := dec.ReadString()
		if e != nil {
			err = e
			return
		}
		rv.SetString(s)
		return
	case reflect.Uint8:
		var n byte
		n, err = dec.ReadByte()
		rv.SetUint(uint64(n))
		return
	case reflect.Int8:
		var n int8
		n, err = dec.ReadInt8()
		rv.SetInt(int64(n))
		return
	case reflect.Int16:
		var n int16
		n, err = dec.ReadInt16(opt.Order)
		rv.SetInt(int64(n))
		return
	case reflect.Int32:
		var n int32
		n, err = dec.ReadInt32(opt.Order)
		rv.SetInt(int64(n))
		return
	case reflect.Int64:
		var n int64
		n, err = dec.ReadInt64(opt.Order)
		rv.SetInt(int64(n))
		return
	case reflect.Uint16:
		var n uint16
		n, err = dec.ReadUint16(opt.Order)
		rv.SetUint(uint64(n))
		return
	case reflect.Uint32:
		var n uint32
		n, err = dec.ReadUint32(opt.Order)
		rv.SetUint(uint64(n))
		return
	case reflect.Uint64:
		var n uint64
		n, err = dec.ReadUint64(opt.Order)
		rv.SetUint(n)
		return
	case reflect.Float32:
		var n float32
		n, err = dec.ReadFloat32(opt.Order)
		rv.SetFloat(float64(n))
		return
	case reflect.Float64:
		var n float64
		n, err = dec.ReadFloat64(opt.Order)
		rv.SetFloat(n)
		return
	case reflect.Bool:
		var r bool
		r, err = dec.ReadBool()
		rv.SetBool(r)
		return
	case reflect.Interface:
		// skip
		return nil
	}
	switch rt.Kind() {
	case reflect.Array:
		length := rt.Len()
		if traceEnabled {
			zlog.Debug("decoding: reading array", zap.Int("length", length))
		}
		for i := 0; i < length; i++ {
			if err = dec.decodeBin(rv.Index(i), opt); err != nil {
				return
			}
		}
		return
	case reflect.Slice:
		var l int
		if opt.hasSizeOfSlice() {
			l = opt.getSizeOfSlice()
		} else {
			length, err := dec.ReadUvarint64()
			if err != nil {
				return err
			}
			l = int(length)
		}

		if traceEnabled {
			zlog.Debug("reading slice", zap.Int("len", l), typeField("type", rv))
		}

		rv.Set(reflect.MakeSlice(rt, l, l))
		for i := 0; i < l; i++ {
			if err = dec.decodeBin(rv.Index(i), opt); err != nil {
				return
			}
		}

	case reflect.Struct:
		if err = dec.decodeStruct(rt, rv); err != nil {
			return
		}

	default:
		return fmt.Errorf("decode: unsupported type %q", rt)
	}

	return
}

func (dec *Decoder) decodeStruct(rt reflect.Type, rv reflect.Value) (err error) {
	l := rv.NumField()

	if traceEnabled {
		zlog.Debug("decode: struct", zap.Int("fields", l), zap.Stringer("type", rv.Kind()))
	}

	sizeOfMap := map[string]int{}
	seenBinaryExtensionField := false
	for i := 0; i < l; i++ {
		structField := rt.Field(i)
		fieldTag := parseFieldTag(structField.Tag)

		if fieldTag.Skip {
			if traceEnabled {
				zlog.Debug("decode: skipping struct field with skip flag",
					zap.String("struct_field_name", structField.Name),
				)
			}
			continue
		}

		if !fieldTag.BinaryExtension && seenBinaryExtensionField {
			panic(fmt.Sprintf("the `bin:\"binary_extension\"` tags must be packed together at the end of struct fields, problematic field %q", structField.Name))
		}

		if fieldTag.BinaryExtension {
			seenBinaryExtensionField = true
			// FIXME: This works only if what is in `d.data` is the actual full data buffer that
			//        needs to be decoded. If there is for example two structs in the buffer, this
			//        will not work as we would continue into the next struct.
			//
			//        But at the same time, does it make sense otherwise? What would be the inference
			//        rule in the case of extra bytes available? Continue decoding and revert if it's
			//        not working? But how to detect valid errors?
			if len(dec.data[dec.pos:]) <= 0 {
				continue
			}
		}
		v := rv.Field(i)
		if !v.CanSet() {
			// This means that the field cannot be set, to fix this
			// we need to create a pointer to said field
			if !v.CanAddr() {
				// we cannot create a point to field skipping
				if traceEnabled {
					zlog.Debug("skipping struct field that cannot be addressed",
						zap.String("struct_field_name", structField.Name),
						zap.Stringer("struct_value_type", v.Kind()),
					)
				}
				return fmt.Errorf("unable to decode a none setup struc field %q with type %q", structField.Name, v.Kind())
			}
			v = v.Addr()
		}

		if !v.CanSet() {
			if traceEnabled {
				zlog.Debug("skipping struct field that cannot be addressed",
					zap.String("struct_field_name", structField.Name),
					zap.Stringer("struct_value_type", v.Kind()),
				)
			}
			continue
		}

		option := &option{
			OptionalField: fieldTag.Optional,
			Order:         fieldTag.Order,
		}

		if s, ok := sizeOfMap[structField.Name]; ok {
			option.setSizeOfSlice(s)
		}

		if traceEnabled {
			zlog.Debug("decode: struct field",
				zap.Stringer("struct_field_value_type", v.Kind()),
				zap.String("struct_field_name", structField.Name),
				zap.Reflect("struct_field_tags", fieldTag),
				zap.Reflect("struct_field_option", option),
			)
		}

		if err = dec.decodeBin(v, option); err != nil {
			return
		}

		if fieldTag.SizeOf != "" {
			size := sizeof(structField.Type, v)
			if traceEnabled {
				zlog.Debug("setting size of field",
					zap.String("field_name", fieldTag.SizeOf),
					zap.Int("size", size),
				)
			}
			sizeOfMap[fieldTag.SizeOf] = size
		}
	}
	return
}