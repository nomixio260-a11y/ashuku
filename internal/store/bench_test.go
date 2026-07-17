package store

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"sync/atomic"
	"testing"
)

// benchSmallData は i ごとに決定的で互いに重複しない 16KiB を返す
// (dedup もデルタも効かない、純粋な書き込み経路の測定)。
func benchSmallData(i int64) []byte {
	buf := make([]byte, 16<<10)
	x := uint64(i)*2654435761 + 1
	for off := 0; off+8 <= len(buf); off += 8 {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		binary.LittleEndian.PutUint64(buf[off:], x)
	}
	return buf
}

func newBenchStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(b.TempDir(), Config{Compression: "fast", DisableDelta: true, DisablePrecomp: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	return s
}

// BenchmarkSmallPutSequential は単一クライアントが小ファイルを順に
// アップロードするワークロード(1ユーザーの大量投下)。
func BenchmarkSmallPutSequential(b *testing.B) {
	s := newBenchStore(b)
	b.SetBytes(16 << 10)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Put(fmt.Sprintf("f-%d", i), bytes.NewReader(benchSmallData(int64(i)))); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSmallPutConcurrent は多数のユーザーが同時に小ファイルを
// アップロードするワークロード(グループコミットの効果を測る)。
func BenchmarkSmallPutConcurrent(b *testing.B) {
	s := newBenchStore(b)
	var ctr atomic.Int64
	b.SetBytes(16 << 10)
	b.SetParallelism(8) // GOMAXPROCS × 8 ゴルーチン
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := ctr.Add(1)
			if _, err := s.Put(fmt.Sprintf("f-%d", i), bytes.NewReader(benchSmallData(i))); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// BenchmarkSmallPutDup は全員が同一内容をアップロードするワークロード
// (重複排除の高速パスの測定)。
func BenchmarkSmallPutDup(b *testing.B) {
	s := newBenchStore(b)
	data := benchSmallData(1)
	b.SetBytes(16 << 10)
	b.SetParallelism(8)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := s.Put("dup", bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
