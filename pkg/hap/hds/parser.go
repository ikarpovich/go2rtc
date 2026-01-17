package hds

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// DataStream binary format tags
const (
	TagTrue       = 0x01
	TagFalse      = 0x02
	TagTerminator = 0x03
	TagNull       = 0x04
	TagUUID       = 0x05
	TagDate       = 0x06
	TagIntMinus1  = 0x07
	// 0x08-0x2E: Integer 0-38 inline
	TagInt8   = 0x30
	TagInt16  = 0x31
	TagInt32  = 0x32
	TagInt64  = 0x33
	TagFloat32 = 0x35
	TagFloat64 = 0x36
	// 0x40-0x60: UTF8 string length 0-32 inline
	TagUTF8Len8    = 0x61
	TagUTF8Len16   = 0x62
	TagUTF8Len32   = 0x63
	TagUTF8Len64   = 0x64
	TagUTF8Null    = 0x6F
	// 0x70-0x90: Data length 0-32 inline
	TagDataLen8    = 0x91
	TagDataLen16   = 0x92
	TagDataLen32   = 0x93
	TagDataLen64   = 0x94
	TagDataTerm    = 0x9F
	// 0xA0-0xCF: Compression reference index 0-47
	// 0xD0-0xDE: Array length 0-14 inline
	TagArrayTerm = 0xDF
	// 0xE0-0xEE: Dictionary length 0-14 inline
	TagDictTerm = 0xEF
)

var (
	ErrInvalidTag    = errors.New("invalid tag")
	ErrUnexpectedEOF = errors.New("unexpected EOF")
	ErrInvalidUTF8   = errors.New("invalid UTF-8")
)

// Reader decodes DataStream binary format
type Reader struct {
	data []byte
	pos  int
}

// NewReader creates a new DataStream reader
func NewReader(data []byte) *Reader {
	return &Reader{data: data}
}

// Decode reads and decodes the next value
func (r *Reader) Decode() (any, error) {
	if r.pos >= len(r.data) {
		return nil, io.EOF
	}

	tag := r.data[r.pos]
	r.pos++

	switch {
	case tag == TagTrue:
		return true, nil
	case tag == TagFalse:
		return false, nil
	case tag == TagNull:
		return nil, nil
	case tag == TagTerminator:
		return nil, nil
	case tag == TagUUID:
		return r.readUUID()
	case tag == TagDate:
		return r.readDate()
	case tag == TagIntMinus1:
		return int64(-1), nil
	case tag >= 0x08 && tag <= 0x2E:
		return int64(tag - 0x08), nil
	case tag == TagInt8:
		return r.readInt8()
	case tag == TagInt16:
		return r.readInt16()
	case tag == TagInt32:
		return r.readInt32()
	case tag == TagInt64:
		return r.readInt64()
	case tag == TagFloat32:
		return r.readFloat32()
	case tag == TagFloat64:
		return r.readFloat64()
	case tag >= 0x40 && tag <= 0x60:
		return r.readUTF8(int(tag - 0x40))
	case tag == TagUTF8Len8:
		return r.readUTF8WithLen8()
	case tag == TagUTF8Len16:
		return r.readUTF8WithLen16()
	case tag == TagUTF8Len32:
		return r.readUTF8WithLen32()
	case tag == TagUTF8Len64:
		return r.readUTF8WithLen64()
	case tag == TagUTF8Null:
		return r.readUTF8Null()
	case tag >= 0x70 && tag <= 0x90:
		return r.readData(int(tag - 0x70))
	case tag == TagDataLen8:
		return r.readDataWithLen8()
	case tag == TagDataLen16:
		return r.readDataWithLen16()
	case tag == TagDataLen32:
		return r.readDataWithLen32()
	case tag == TagDataLen64:
		return r.readDataWithLen64()
	case tag >= 0xD0 && tag <= 0xDE:
		return r.readArray(int(tag - 0xD0))
	case tag == TagArrayTerm:
		return r.readArrayTerm()
	case tag >= 0xE0 && tag <= 0xEE:
		return r.readDict(int(tag - 0xE0))
	case tag == TagDictTerm:
		return r.readDictTerm()
	default:
		return nil, fmt.Errorf("%w: 0x%02X", ErrInvalidTag, tag)
	}
}

func (r *Reader) readBytes(n int) ([]byte, error) {
	if r.pos+n > len(r.data) {
		return nil, ErrUnexpectedEOF
	}
	b := r.data[r.pos : r.pos+n]
	r.pos += n
	return b, nil
}

func (r *Reader) readUUID() (string, error) {
	b, err := r.readBytes(16)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[0], b[1], b[2], b[3], b[4], b[5], b[6], b[7],
		b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15]), nil
}

func (r *Reader) readDate() (time.Time, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return time.Time{}, err
	}
	seconds := math.Float64frombits(binary.LittleEndian.Uint64(b))
	// Date is seconds since 2001-01-01 00:00:00 UTC
	ref := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	return ref.Add(time.Duration(seconds * float64(time.Second))), nil
}

func (r *Reader) readInt8() (int64, error) {
	b, err := r.readBytes(1)
	if err != nil {
		return 0, err
	}
	return int64(int8(b[0])), nil
}

func (r *Reader) readInt16() (int64, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return 0, err
	}
	return int64(int16(binary.LittleEndian.Uint16(b))), nil
}

func (r *Reader) readInt32() (int64, error) {
	b, err := r.readBytes(4)
	if err != nil {
		return 0, err
	}
	return int64(int32(binary.LittleEndian.Uint32(b))), nil
}

func (r *Reader) readInt64() (int64, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return 0, err
	}
	return int64(binary.LittleEndian.Uint64(b)), nil
}

func (r *Reader) readFloat32() (float64, error) {
	b, err := r.readBytes(4)
	if err != nil {
		return 0, err
	}
	return float64(math.Float32frombits(binary.LittleEndian.Uint32(b))), nil
}

func (r *Reader) readFloat64() (float64, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

func (r *Reader) readUTF8(n int) (string, error) {
	b, err := r.readBytes(n)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (r *Reader) readUTF8WithLen8() (string, error) {
	b, err := r.readBytes(1)
	if err != nil {
		return "", err
	}
	return r.readUTF8(int(b[0]))
}

func (r *Reader) readUTF8WithLen16() (string, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return "", err
	}
	return r.readUTF8(int(binary.LittleEndian.Uint16(b)))
}

func (r *Reader) readUTF8WithLen32() (string, error) {
	b, err := r.readBytes(4)
	if err != nil {
		return "", err
	}
	return r.readUTF8(int(binary.LittleEndian.Uint32(b)))
}

func (r *Reader) readUTF8WithLen64() (string, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return "", err
	}
	return r.readUTF8(int(binary.LittleEndian.Uint64(b)))
}

func (r *Reader) readUTF8Null() (string, error) {
	start := r.pos
	for r.pos < len(r.data) && r.data[r.pos] != 0 {
		r.pos++
	}
	if r.pos >= len(r.data) {
		return "", ErrUnexpectedEOF
	}
	s := string(r.data[start:r.pos])
	r.pos++ // skip null terminator
	return s, nil
}

func (r *Reader) readData(n int) ([]byte, error) {
	return r.readBytes(n)
}

func (r *Reader) readDataWithLen8() ([]byte, error) {
	b, err := r.readBytes(1)
	if err != nil {
		return nil, err
	}
	return r.readData(int(b[0]))
}

func (r *Reader) readDataWithLen16() ([]byte, error) {
	b, err := r.readBytes(2)
	if err != nil {
		return nil, err
	}
	return r.readData(int(binary.LittleEndian.Uint16(b)))
}

func (r *Reader) readDataWithLen32() ([]byte, error) {
	b, err := r.readBytes(4)
	if err != nil {
		return nil, err
	}
	return r.readData(int(binary.LittleEndian.Uint32(b)))
}

func (r *Reader) readDataWithLen64() ([]byte, error) {
	b, err := r.readBytes(8)
	if err != nil {
		return nil, err
	}
	return r.readData(int(binary.LittleEndian.Uint64(b)))
}

func (r *Reader) readArray(n int) ([]any, error) {
	arr := make([]any, 0, n)
	for i := 0; i < n; i++ {
		val, err := r.Decode()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	return arr, nil
}

func (r *Reader) readArrayTerm() ([]any, error) {
	arr := make([]any, 0)
	for {
		if r.pos >= len(r.data) {
			return nil, ErrUnexpectedEOF
		}
		if r.data[r.pos] == TagTerminator {
			r.pos++
			break
		}
		val, err := r.Decode()
		if err != nil {
			return nil, err
		}
		arr = append(arr, val)
	}
	return arr, nil
}

func (r *Reader) readDict(n int) (map[string]any, error) {
	dict := make(map[string]any, n)
	for i := 0; i < n; i++ {
		key, err := r.Decode()
		if err != nil {
			return nil, err
		}
		keyStr, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("dict key must be string, got %T", key)
		}
		val, err := r.Decode()
		if err != nil {
			return nil, err
		}
		dict[keyStr] = val
	}
	return dict, nil
}

func (r *Reader) readDictTerm() (map[string]any, error) {
	dict := make(map[string]any)
	for {
		if r.pos >= len(r.data) {
			return nil, ErrUnexpectedEOF
		}
		if r.data[r.pos] == TagTerminator {
			r.pos++
			break
		}
		key, err := r.Decode()
		if err != nil {
			return nil, err
		}
		keyStr, ok := key.(string)
		if !ok {
			return nil, fmt.Errorf("dict key must be string, got %T", key)
		}
		val, err := r.Decode()
		if err != nil {
			return nil, err
		}
		dict[keyStr] = val
	}
	return dict, nil
}

// Writer encodes values to DataStream binary format
type Writer struct {
	buf []byte
}

// NewWriter creates a new DataStream writer
func NewWriter() *Writer {
	return &Writer{buf: make([]byte, 0, 1024)}
}

// Bytes returns the encoded data
func (w *Writer) Bytes() []byte {
	return w.buf
}

// Reset clears the buffer
func (w *Writer) Reset() {
	w.buf = w.buf[:0]
}

// Encode writes a value to the buffer
func (w *Writer) Encode(v any) error {
	switch val := v.(type) {
	case nil:
		w.buf = append(w.buf, TagNull)
	case bool:
		if val {
			w.buf = append(w.buf, TagTrue)
		} else {
			w.buf = append(w.buf, TagFalse)
		}
	case int:
		return w.encodeInt(int64(val))
	case int8:
		return w.encodeInt(int64(val))
	case int16:
		return w.encodeInt(int64(val))
	case int32:
		return w.encodeInt(int64(val))
	case int64:
		return w.encodeInt(val)
	case uint:
		return w.encodeInt(int64(val))
	case uint8:
		return w.encodeInt(int64(val))
	case uint16:
		return w.encodeInt(int64(val))
	case uint32:
		return w.encodeInt(int64(val))
	case uint64:
		return w.encodeInt(int64(val))
	case float32:
		return w.encodeFloat32(val)
	case float64:
		return w.encodeFloat64(val)
	case string:
		return w.encodeUTF8(val)
	case []byte:
		return w.encodeData(val)
	case []any:
		return w.encodeArray(val)
	case map[string]any:
		return w.encodeDict(val)
	case time.Time:
		return w.encodeDate(val)
	default:
		return fmt.Errorf("unsupported type: %T", v)
	}
	return nil
}

func (w *Writer) encodeInt(v int64) error {
	if v == -1 {
		w.buf = append(w.buf, TagIntMinus1)
		return nil
	}
	if v >= 0 && v <= 38 {
		w.buf = append(w.buf, byte(v+0x08))
		return nil
	}
	if v >= math.MinInt8 && v <= math.MaxInt8 {
		w.buf = append(w.buf, TagInt8, byte(v))
		return nil
	}
	if v >= math.MinInt16 && v <= math.MaxInt16 {
		w.buf = append(w.buf, TagInt16)
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(v))
		w.buf = append(w.buf, b...)
		return nil
	}
	if v >= math.MinInt32 && v <= math.MaxInt32 {
		w.buf = append(w.buf, TagInt32)
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(v))
		w.buf = append(w.buf, b...)
		return nil
	}
	w.buf = append(w.buf, TagInt64)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, uint64(v))
	w.buf = append(w.buf, b...)
	return nil
}

func (w *Writer) encodeFloat32(v float32) error {
	w.buf = append(w.buf, TagFloat32)
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, math.Float32bits(v))
	w.buf = append(w.buf, b...)
	return nil
}

func (w *Writer) encodeFloat64(v float64) error {
	w.buf = append(w.buf, TagFloat64)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, math.Float64bits(v))
	w.buf = append(w.buf, b...)
	return nil
}

func (w *Writer) encodeUTF8(v string) error {
	n := len(v)
	if n <= 32 {
		w.buf = append(w.buf, byte(0x40+n))
		w.buf = append(w.buf, v...)
		return nil
	}
	if n <= math.MaxUint8 {
		w.buf = append(w.buf, TagUTF8Len8, byte(n))
		w.buf = append(w.buf, v...)
		return nil
	}
	if n <= math.MaxUint16 {
		w.buf = append(w.buf, TagUTF8Len16)
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(n))
		w.buf = append(w.buf, b...)
		w.buf = append(w.buf, v...)
		return nil
	}
	if n <= math.MaxUint32 {
		w.buf = append(w.buf, TagUTF8Len32)
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(n))
		w.buf = append(w.buf, b...)
		w.buf = append(w.buf, v...)
		return nil
	}
	return fmt.Errorf("string too long: %d", n)
}

func (w *Writer) encodeData(v []byte) error {
	n := len(v)
	if n <= 32 {
		w.buf = append(w.buf, byte(0x70+n))
		w.buf = append(w.buf, v...)
		return nil
	}
	if n <= math.MaxUint8 {
		w.buf = append(w.buf, TagDataLen8, byte(n))
		w.buf = append(w.buf, v...)
		return nil
	}
	if n <= math.MaxUint16 {
		w.buf = append(w.buf, TagDataLen16)
		b := make([]byte, 2)
		binary.LittleEndian.PutUint16(b, uint16(n))
		w.buf = append(w.buf, b...)
		w.buf = append(w.buf, v...)
		return nil
	}
	if n <= math.MaxUint32 {
		w.buf = append(w.buf, TagDataLen32)
		b := make([]byte, 4)
		binary.LittleEndian.PutUint32(b, uint32(n))
		w.buf = append(w.buf, b...)
		w.buf = append(w.buf, v...)
		return nil
	}
	return fmt.Errorf("data too long: %d", n)
}

func (w *Writer) encodeArray(v []any) error {
	n := len(v)
	if n <= 14 {
		w.buf = append(w.buf, byte(0xD0+n))
	} else {
		w.buf = append(w.buf, TagArrayTerm)
	}
	for _, item := range v {
		if err := w.Encode(item); err != nil {
			return err
		}
	}
	if n > 14 {
		w.buf = append(w.buf, TagTerminator)
	}
	return nil
}

func (w *Writer) encodeDict(v map[string]any) error {
	n := len(v)
	if n <= 14 {
		w.buf = append(w.buf, byte(0xE0+n))
	} else {
		w.buf = append(w.buf, TagDictTerm)
	}
	for key, val := range v {
		if err := w.encodeUTF8(key); err != nil {
			return err
		}
		if err := w.Encode(val); err != nil {
			return err
		}
	}
	if n > 14 {
		w.buf = append(w.buf, TagTerminator)
	}
	return nil
}

func (w *Writer) encodeDate(v time.Time) error {
	ref := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
	seconds := v.Sub(ref).Seconds()
	w.buf = append(w.buf, TagDate)
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, math.Float64bits(seconds))
	w.buf = append(w.buf, b...)
	return nil
}
