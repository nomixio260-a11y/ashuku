package chunker

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"strconv"
	"sync"
	"testing"
)

// TestGoldenBoundaries はチャンク境界が固定であることを保証する回帰テスト。
// この golden は置き換え元 fastcdc-go と境界一致を確認したうえで固定した値
// (移行時の parity 検証済み)。ここが変わると既存保存データとの dedup 互換が
// 壊れるため、実装変更でこのテストが落ちたら**意図的な境界変更でない限り**
// 差し戻すこと。
func TestGoldenBoundaries(t *testing.T) {
	cases := []struct {
		avg               int
		nchunks           int
		lenHash, dataHash string
	}{
		{1 << 20, 7,
			"45ea063b14276c802f0022e6636e2206859204e0a7998abab519567aa52d8895",
			"a591762ba183f90b7039b972f19b5398c96284ef2972c693cf178feb8513967e"},
		{1 << 16, 113,
			"8083f68c927e74c16296d4935618c68e1d82b7cb392eb7a8c097e5815ab8b1a2",
			"a591762ba183f90b7039b972f19b5398c96284ef2972c693cf178feb8513967e"},
	}
	for _, tc := range cases {
		b := make([]byte, 8<<20)
		rand.New(rand.NewSource(1)).Read(b)
		c, err := New(bytes.NewReader(b), tc.avg)
		if err != nil {
			t.Fatal(err)
		}
		var lens bytes.Buffer
		hh := sha256.New()
		n := 0
		for {
			ch, err := c.Next()
			if err != nil {
				break
			}
			lens.WriteString(strconv.Itoa(len(ch.Data)))
			lens.WriteByte(',')
			hh.Write(ch.Data)
			n++
		}
		if n != tc.nchunks {
			t.Fatalf("avg=%d: チャンク数 %d, want %d", tc.avg, n, tc.nchunks)
		}
		lh := sha256.Sum256(lens.Bytes())
		if got := hex.EncodeToString(lh[:]); got != tc.lenHash {
			t.Fatalf("avg=%d: 境界(長さ列)が変わった: %s", tc.avg, got)
		}
		if got := hex.EncodeToString(hh.Sum(nil)); got != tc.dataHash {
			t.Fatalf("avg=%d: チャンク内容ハッシュが変わった: %s", tc.avg, got)
		}
	}
}

// TestReassembly はチャンクを連結すると原文にビット一致で戻ることを確認する。
func TestReassembly(t *testing.T) {
	for _, data := range [][]byte{
		nil,
		[]byte("short"),
		randDataC(3 << 20),
		bytes.Repeat([]byte("abc"), 500000),
	} {
		c, err := New(bytes.NewReader(data), 1<<18)
		if err != nil {
			t.Fatal(err)
		}
		var out []byte
		for {
			ch, err := c.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, ch.Data...)
		}
		if !bytes.Equal(out, data) {
			t.Fatalf("再結合が原文と一致しない: len got=%d want=%d", len(out), len(data))
		}
	}
}

// TestConcurrentChunkingRaceFree は複数 Chunker を並行に走らせても
// データ競合しないことを確認する(gear テーブルが読み取り専用)。
// -race で実行したとき、大域テーブルへの競合がないことが担保になる。
func TestConcurrentChunkingRaceFree(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			data := randDataCSeed(2<<20, seed)
			c, err := New(bytes.NewReader(data), 1<<18)
			if err != nil {
				t.Error(err)
				return
			}
			var out []byte
			for {
				ch, err := c.Next()
				if err != nil {
					break
				}
				out = append(out, ch.Data...)
			}
			if !bytes.Equal(out, data) {
				t.Error("並行チャンク化で再結合不一致")
			}
		}(int64(g))
	}
	wg.Wait()
}

func randDataC(n int) []byte { return randDataCSeed(n, 7) }
func randDataCSeed(n int, seed int64) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}
