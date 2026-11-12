package nfs

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestRecordTruncatedMark(t *testing.T) {
	for _, data := range [][]byte{{}, {0x80}, {0x80, 0}, {0x80, 0, 0}, {0, 0, 0, 1, 'x'}} {
		want := io.ErrUnexpectedEOF
		if len(data) == 0 {
			want = io.EOF
		}
		if _, err := readRecord(bufio.NewReader(bytes.NewReader(data)), nil); !errors.Is(err, want) {
			t.Errorf("record %x: got %v, want %v", data, err, want)
		}
	}
}

func BenchmarkReadRecord(b *testing.B) {
	record := make([]byte, 260)
	binary.BigEndian.PutUint32(record, lastFragment|256)
	source := bytes.NewReader(record)
	reader := bufio.NewReaderSize(source, 64)
	storage := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		source.Reset(record)
		reader.Reset(source)
		data, err := readRecord(reader, storage)
		if err != nil || len(data) != 256 {
			b.Fatalf("record: %d bytes, %v", len(data), err)
		}
	}
}
