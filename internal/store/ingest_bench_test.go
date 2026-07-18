package store

import (
	"bytes"
	"fmt"
	"math/rand"
	"testing"
	"time"
)

func TestIngestThroughput(t *testing.T) {
	s := newTestStore(t)
	rng := rand.New(rand.NewSource(11))
	var sb bytes.Buffer
	for sb.Len() < 24<<20 {
		fmt.Fprintf(&sb, "2026-07-18T%02d:%02d:%02dZ host%03d svc[%d]: event=%d payload=%x\n",
			rng.Intn(24), rng.Intn(60), rng.Intn(60), rng.Intn(100), rng.Intn(9999), rng.Intn(1<<30), rng.Int63())
	}
	data := sb.Bytes()
	t0 := time.Now()
	m, err := s.Put("big.log", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	dt := time.Since(t0)
	t.Logf("ingest %d MiB in %s = %.1f MiB/s", len(data)>>20, dt.Round(time.Millisecond), float64(len(data))/dt.Seconds()/(1<<20))
	got := getBytes(t, s, m.ID)
	if !bytes.Equal(got, data) {
		t.Fatal("読み戻し不一致")
	}
}
