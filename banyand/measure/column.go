// Licensed to Apache Software Foundation (ASF) under one or more contributor
// license agreements. See the NOTICE file distributed with
// this work for additional information regarding copyright
// ownership. Apache Software Foundation (ASF) licenses this file to you under
// the Apache License, Version 2.0 (the "License"); you may
// not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package measure

import (
	"sync"

	"github.com/apache/skywalking-banyandb/pkg/bytes"
	"github.com/apache/skywalking-banyandb/pkg/encoding"
	"github.com/apache/skywalking-banyandb/pkg/fs"
	"github.com/apache/skywalking-banyandb/pkg/logger"
	pbv1 "github.com/apache/skywalking-banyandb/pkg/pb/v1"
)

var (
	int64SlicePool = sync.Pool{
		New: func() interface{} {
			return make([]int64, 0, 1024)
		},
	}
	float64SlicePool = sync.Pool{
		New: func() interface{} {
			return make([]float64, 0, 1024)
		},
	}
)

type column struct {
	name      string
	values    [][]byte
	valueType pbv1.ValueType
}

func (c *column) reset() {
	c.name = ""

	values := c.values
	for i := range values {
		values[i] = nil
	}
	c.values = values[:0]
}

func (c *column) resizeValues(valuesLen int) [][]byte {
	values := c.values
	if n := valuesLen - cap(values); n > 0 {
		values = append(values[:cap(values)], make([][]byte, n)...)
	}
	values = values[:valuesLen]
	c.values = values
	return values
}

func (c *column) mustWriteTo(cm *columnMetadata, columnWriter *writer) {
	cm.reset()

	cm.name = c.name
	cm.valueType = c.valueType
	// buffer
	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)

	// marshal values
	// select encoding based on data type
	switch c.valueType {
	case pbv1.ValueTypeInt64:
		// convert byte array to int64 array
		intValues := int64SlicePool.Get().([]int64)
		intValues = intValues[:0]
		if cap(intValues) < len(c.values) {
			intValues = make([]int64, len(c.values))
		} else {
			intValues = intValues[:len(c.values)]
		}
		defer int64SlicePool.Put(intValues)

		for i, v := range c.values {
			if len(v) != 8 {
				var val int64
				for j := 0; j < len(v); j++ {
					val = (val << 8) | int64(v[j])
				}
				intValues[i] = val
			} else {
				intValues[i] = encoding.BytesToInt64(v)
			}
		}
		// use delta encoding for integer column
		var encodeType encoding.EncodeType
		var firstValue int64
		bb.Buf, encodeType, firstValue = encoding.Int64ListToBytes(bb.Buf[:0], intValues)
		cm.encodeType = encodeType
		cm.firstValue = firstValue
		if cm.encodeType == encoding.EncodeTypeUnknown {
			logger.Panicf("invalid encode type for int64 values")
		}
	case pbv1.ValueTypeFloat64:
		// convert byte array to float64 array
		floatValues := float64SlicePool.Get().([]float64)
		floatValues = floatValues[:0]
		if cap(floatValues) < len(c.values) {
			floatValues = make([]float64, len(c.values))
		} else {
			floatValues = floatValues[:len(c.values)]
		}
		defer float64SlicePool.Put(floatValues)

		for i, v := range c.values {
			floatValues[i] = encoding.BytesToFloat64(v)
		}
		// use XOR encoding for float column
		bb.Buf = bb.Buf[:0]
		writer := encoding.NewWriter()
		writer.Reset(bytes.NewByteSliceWriter(&bb.Buf))
		xorEncoder := encoding.NewXOREncoder(writer)
		// convert float64 to uint64 for encoding
		for _, v := range floatValues {
			xorEncoder.Write(encoding.Float64ToUint64(v))
		}
		xorEncoder.Close()
	default:
		bb.Buf = encoding.EncodeBytesBlock(bb.Buf[:0], c.values)
	}

	cm.size = uint64(len(bb.Buf))
	if cm.size > maxValuesBlockSize {
		logger.Panicf("too valuesSize: %d bytes; mustn't exceed %d bytes", cm.size, maxValuesBlockSize)
	}
	cm.offset = columnWriter.bytesWritten
	columnWriter.MustWrite(bb.Buf)
}

func (c *column) mustReadValues(decoder *encoding.BytesBlockDecoder, reader fs.Reader, cm columnMetadata, count uint64) {
	c.name = cm.name
	c.valueType = cm.valueType
	if c.valueType == pbv1.ValueTypeUnknown {
		for i := uint64(0); i < count; i++ {
			c.values = append(c.values, nil)
		}
		return
	}

	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)
	valuesSize := cm.size
	if valuesSize > maxValuesBlockSize {
		logger.Panicf("%s: block size cannot exceed %d bytes; got %d bytes", reader.Path(), maxValuesBlockSize, valuesSize)
	}
	bb.Buf = bytes.ResizeOver(bb.Buf, int(valuesSize))
	fs.MustReadData(reader, int64(cm.offset), bb.Buf)

	switch c.valueType {
	case pbv1.ValueTypeInt64:
		// decode integer type
		intValues := int64SlicePool.Get().([]int64)
		intValues = intValues[:0]
		if cap(intValues) < int(count) {
			intValues = make([]int64, count)
		} else {
			intValues = intValues[:count]
		}
		defer int64SlicePool.Put(intValues)

		var err error
		intValues, err = encoding.BytesToInt64List(intValues[:0], bb.Buf, cm.encodeType, cm.firstValue, int(count))
		if err != nil {
			logger.Panicf("%s: cannot decode int values: %v", reader.Path(), err)
		}
		// convert int64 array to byte array
		c.values = make([][]byte, count)
		for i, v := range intValues {
			c.values[i] = encoding.Int64ToBytes(nil, v)
		}
	case pbv1.ValueTypeFloat64:
		// decode float type
		reader := encoding.NewReader(bytes.NewByteSliceReader(bb.Buf))
		xorDecoder := encoding.NewXORDecoder(reader)
		c.values = make([][]byte, count)
		for i := uint64(0); i < count; i++ {
			if !xorDecoder.Next() {
				logger.Panicf("cannot decode float value at index %d: %v", i, xorDecoder.Err())
			}
			val := xorDecoder.Value()
			// convert uint64 back to float64
			floatVal := encoding.Uint64ToFloat64(val)
			c.values[i] = encoding.Float64ToBytes(nil, floatVal)
		}
	default:
		var err error
		c.values, err = decoder.Decode(c.values[:0], bb.Buf, count)
		if err != nil {
			logger.Panicf("%s: cannot decode values: %v", reader.Path(), err)
		}
	}
}

func (c *column) mustSeqReadValues(decoder *encoding.BytesBlockDecoder, reader *seqReader, cm columnMetadata, count uint64) {
	c.name = cm.name
	c.valueType = cm.valueType
	if cm.offset != reader.bytesRead {
		logger.Panicf("%s: offset mismatch: %d vs %d", reader.Path(), cm.offset, reader.bytesRead)
	}
	valuesSize := cm.size
	if valuesSize > maxValuesBlockSize {
		logger.Panicf("%s: block size cannot exceed %d bytes; got %d bytes", reader.Path(), maxValuesBlockSize, valuesSize)
	}

	bb := bigValuePool.Generate()
	defer bigValuePool.Release(bb)

	bb.Buf = bytes.ResizeOver(bb.Buf, int(valuesSize))
	reader.mustReadFull(bb.Buf)

	switch c.valueType {
	case pbv1.ValueTypeInt64:
		// decode integer type
		intValues := int64SlicePool.Get().([]int64)
		intValues = intValues[:0]
		if cap(intValues) < int(count) {
			intValues = make([]int64, count)
		} else {
			intValues = intValues[:count]
		}
		defer int64SlicePool.Put(intValues)

		var err error
		intValues, err = encoding.BytesToInt64List(intValues[:0], bb.Buf, cm.encodeType, cm.firstValue, int(count))
		if err != nil {
			logger.Panicf("%s: mustSeqReadValues cannot decode int values: %v", reader.Path(), err)
		}
		// convert int64 array to byte array
		c.values = make([][]byte, count)
		for i, v := range intValues {
			c.values[i] = encoding.Int64ToBytes(nil, v)
		}
	case pbv1.ValueTypeFloat64:
		// decode float type
		reader := encoding.NewReader(bytes.NewByteSliceReader(bb.Buf))
		xorDecoder := encoding.NewXORDecoder(reader)
		c.values = make([][]byte, count)
		for i := uint64(0); i < count; i++ {
			if !xorDecoder.Next() {
				logger.Panicf("mustSeqReadValues cannot decode float value at index %d: %v", i, xorDecoder.Err())
			}
			val := xorDecoder.Value()
			floatVal := encoding.Uint64ToFloat64(val)
			c.values[i] = encoding.Float64ToBytes(nil, floatVal)
		}
	default:
		var err error
		c.values, err = decoder.Decode(c.values[:0], bb.Buf, count)
		if err != nil {
			logger.Panicf("%s: mustSeqReadValues cannot decode values: %v", reader.Path(), err)
		}
	}
}

var bigValuePool = bytes.NewBufferPool("measure-big-value")

type columnFamily struct {
	name    string
	columns []column
}

func (cf *columnFamily) reset() {
	cf.name = ""

	columns := cf.columns
	for i := range columns {
		columns[i].reset()
	}
	cf.columns = columns[:0]
}

func (cf *columnFamily) resizeColumns(columnsLen int) []column {
	columns := cf.columns
	if n := columnsLen - cap(columns); n > 0 {
		columns = append(columns[:cap(columns)], make([]column, n)...)
	}
	columns = columns[:columnsLen]
	cf.columns = columns
	return columns
}
