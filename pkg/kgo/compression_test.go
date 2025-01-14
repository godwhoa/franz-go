package kgo

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"sync"
	"testing"
)

func TestNewCompressor(t *testing.T) {
	t.Parallel()
	for i, test := range []struct {
		codecs []CompressionCodec
		fail   bool
	}{
		{codecs: []CompressionCodec{{codec: -1}}, fail: true},
		{codecs: []CompressionCodec{{codec: 5}}, fail: true},

		{codecs: []CompressionCodec{{codec: 0}}},
		{codecs: []CompressionCodec{{codec: 1}, {codec: 0}}},
		{codecs: []CompressionCodec{{codec: 2}, {codec: 0}}},
		{codecs: []CompressionCodec{{codec: 3}}},
		{codecs: []CompressionCodec{{codec: 4}}},
		{codecs: []CompressionCodec{{codec: 4}, {codec: 3}}},

		{codecs: []CompressionCodec{{codec: 1, level: 127}}}, // bad gzip level is defaulted fine
		{codecs: []CompressionCodec{{codec: 3, level: 127}}}, // bad lz4 level, same
		{codecs: []CompressionCodec{{codec: 4, level: 127}}}, // bad zstd level, same

		{codecs: []CompressionCodec{
			{codec: 4},
			{codec: 4},
			{codec: 3},
			{codec: 2},
			{codec: 1, level: 1},
		}},
	} {
		_, err := newCompressor(test.codecs...)
		fail := err != nil
		if fail != test.fail {
			t.Errorf("#%d: ok? %v, exp ok? %v", i, !fail, !test.fail)
		}
	}
}

func TestCompressDecompress(t *testing.T) {
	t.Parallel()
	d := newDecompressor()
	in := []byte("foo")
	var wg sync.WaitGroup
	for _, produceVersion := range []int16{
		0, 7,
	} {
		wg.Add(1)
		go func(produceVersion int16) {
			defer wg.Done()
			for _, codecs := range [][]CompressionCodec{
				{{codec: 0}},
				{{codec: 1}},
				{{codec: 2}},
				{{codec: 3}},
				{{codec: 4}},
				{{codec: 4}, {codec: 3}},
			} {
				c, _ := newCompressor(codecs...)
				if c == nil {
					if codecs[0].codec == 0 {
						continue
					}
					t.Errorf("unexpected nil compressor from codecs %v", codecs)
				}
				for i := 0; i < 3; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						w := sliceWriters.Get().(*sliceWriter)
						defer sliceWriters.Put(w)
						got, used := c.compress(w, in, produceVersion)

						got, err := d.decompress(got, byte(used))
						if err != nil {
							t.Errorf("unexpected decompress err: %v", err)
							return
						}
						if !bytes.Equal(got, in) {
							t.Errorf("got decompress %s != exp compress in %s", got, in)
						}
					}()
				}
			}
		}(produceVersion)
	}
	wg.Wait()
}

func BenchmarkCompress(b *testing.B) {
	c, _ := newCompressor(CompressionCodec{codec: 2}) // snappy
	in := []byte("foo")
	for i := 0; i < b.N; i++ {
		w := sliceWriters.Get().(*sliceWriter)
		c.compress(w, in, 0)
		sliceWriters.Put(w)
	}
}

func Test_xerialDecode(t *testing.T) {
	tests := []struct {
		name            string
		compressedInput string
		want            []byte
		wantErr         bool
	}{
		{
			"Compressed data ok",
			"glNOQVBQWQAAAAABAAAAAQAAAA8NMEhlbGxvLCBXb3JsZCE=",
			[]byte("Hello, World!"),
			false,
		},
		{
			"Compressed data without header",
			"/wYAAHNOYVBwWQERAACChVPDSGVsbG8sIFdvcmxkIQ==",
			nil,
			true,
		},
		{
			"Compressed data less than minimum length, malformed",
			"glNOQVBQWQAAAAABAAAAAQAAAA==",
			nil,
			true,
		},
		{
			"Compressed data not the advertised length",
			"glNOQVBQWQAAAAABAAAAAQAAAA8NMEhlbGxvLCBXb3Js",
			nil,
			true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := base64.StdEncoding.DecodeString(test.compressedInput)
			if err != nil {
				t.Errorf("base64 decode error = %v", err)
				return
			}
			got, err := xerialDecode(data)
			if (err != nil) != test.wantErr {
				t.Errorf("xerialDecode() error = %v, wantErr %v", err, test.wantErr)
				return
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Errorf("got decompress %s != exp compress in %s", got, test.want)
			}
		})
	}
}
