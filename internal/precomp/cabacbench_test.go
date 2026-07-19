package precomp

import (
	"os"
	"testing"
)

func BenchmarkCabacUnwrapMP4(b *testing.B) {
	orig, err := os.ReadFile("testdata/h264/phone_like.mp4")
	if err != nil {
		b.Skip(err)
	}
	b.SetBytes(int64(len(orig)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := TryUnwrapMP4H264(orig, 0); !ok {
			b.Fatal("not adopted")
		}
	}
}

func BenchmarkCabacReconstructMP4(b *testing.B) {
	orig, _ := os.ReadFile("testdata/h264/phone_like.mp4")
	u, ok := TryUnwrapMP4H264(orig, 0)
	if !ok {
		b.Skip("not adopted")
	}
	b.SetBytes(int64(len(orig)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ReconstructMP4H264(u.Recipe, u.Chunked); err != nil {
			b.Fatal(err)
		}
	}
}
