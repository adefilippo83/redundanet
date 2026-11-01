// Command cryptobench measures the primitives the native engine would spend
// its CPU on, single-threaded and on N cores, so the Pi 4 numbers pick the
// cipher and show whether 2-of-4 encoding keeps up with the uplink.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"flag"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/klauspost/reedsolomon"
	"github.com/zeebo/blake3"
	"golang.org/x/crypto/chacha20poly1305"
)

func main() {
	size := flag.Int("size", 1<<20, "buffer size in bytes")
	secs := flag.Float64("seconds", 2, "time per measurement")
	threads := flag.String("threads", "1,4", "thread counts to measure")
	flag.Parse()

	buf := make([]byte, *size)
	_, _ = rand.Read(buf)
	key := make([]byte, 32)
	_, _ = rand.Read(key)
	nonce := make([]byte, 12)

	block, _ := aes.NewCipher(key)
	gcm, _ := cipher.NewGCM(block)
	chacha, _ := chacha20poly1305.New(key)
	rs, _ := reedsolomon.New(2, 2)
	shards, _ := rs.Split(buf)

	fmt.Printf("%s %s, %d cores, buffer %d KiB\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), *size>>10)
	cases := []struct {
		name string
		fn   func(out []byte)
	}{
		{"AES-256-GCM seal", func(out []byte) { gcm.Seal(out[:0], nonce, buf, nil) }},
		{"ChaCha20-Poly1305 seal", func(out []byte) { chacha.Seal(out[:0], nonce, buf, nil) }},
		{"BLAKE3 hash", func([]byte) { blake3.Sum256(buf) }},
		{"SHA-256 hash", func([]byte) { sha256.Sum256(buf) }},
		{"RS 2-of-4 encode", func([]byte) { _ = rs.Encode(shards) }},
		{"RS 2-of-4 reconstruct", func([]byte) {
			shards[0], shards[3] = nil, nil
			_ = rs.Reconstruct(shards)
		}},
	}
	var ns []int
	for _, s := range splitInts(*threads) {
		ns = append(ns, s)
	}
	fmt.Printf("%-24s", "")
	for _, n := range ns {
		fmt.Printf("%12s", fmt.Sprintf("%d thr MB/s", n))
	}
	fmt.Println()
	for _, c := range cases {
		fmt.Printf("%-24s", c.name)
		for _, n := range ns {
			fmt.Printf("%12.0f", measure(c.fn, n, *size, *secs))
		}
		fmt.Println()
	}
}

func measure(fn func(out []byte), threads, size int, secs float64) float64 {
	deadline := time.Now().Add(time.Duration(secs * float64(time.Second)))
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for t := 0; t < threads; t++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out := make([]byte, size+64)
			n := 0
			for time.Now().Before(deadline) {
				fn(out)
				n++
			}
			mu.Lock()
			total += n
			mu.Unlock()
		}()
	}
	start := time.Now()
	wg.Wait()
	return float64(total) * float64(size) / time.Since(start).Seconds() / 1e6
}

func splitInts(s string) []int {
	var out []int
	for _, part := range splitComma(s) {
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func splitComma(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
