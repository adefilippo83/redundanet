// Command tcpbench is the tinc baseline: a plain TCP sink and source, run
// inside the tinc network namespace on two nodes, so the relay numbers have
// something to be compared with on the same pair at the same hour.
//
//	tcpbench -listen :5201
//	tcpbench -connect 10.100.0.10:5201 -size 64
package main

import (
	"crypto/rand"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"time"
)

func main() {
	listen := flag.String("listen", "", "sink: address to listen on")
	connect := flag.String("connect", "", "source: sink address")
	size := flag.Int64("size", 64, "MiB to send")
	flag.Parse()
	switch {
	case *listen != "":
		ln, err := net.Listen("tcp", *listen)
		if err != nil {
			fail(err)
		}
		for {
			c, err := ln.Accept()
			if err != nil {
				fail(err)
			}
			go func() {
				defer c.Close()
				var hdr [8]byte
				if _, err := io.ReadFull(c, hdr[:]); err != nil {
					return
				}
				want := int64(binary.BigEndian.Uint64(hdr[:]))
				start := time.Now()
				n, _ := io.CopyN(io.Discard, c, want)
				binary.BigEndian.PutUint64(hdr[:], uint64(n))
				_, _ = c.Write(hdr[:])
				fmt.Printf("%s: %d MiB from %s in %s\n", time.Now().Format("15:04:05"), n>>20, c.RemoteAddr(), time.Since(start).Round(time.Millisecond))
			}()
		}
	case *connect != "":
		bytes := *size << 20
		c, err := net.DialTimeout("tcp", *connect, 10*time.Second)
		if err != nil {
			fail(err)
		}
		defer c.Close()
		var hdr [8]byte
		binary.BigEndian.PutUint64(hdr[:], uint64(bytes))
		if _, err := c.Write(hdr[:]); err != nil {
			fail(err)
		}
		// Random bytes: tinc compresses (Compression = 9 on the live
		// network) and zeros would measure zlib, not the link. Real traffic
		// is Tahoe ciphertext, which does not compress.
		buf := make([]byte, 1<<20)
		if _, err := rand.Read(buf); err != nil {
			fail(err)
		}
		start := time.Now()
		var sent int64
		for sent < bytes {
			n := int64(len(buf))
			if bytes-sent < n {
				n = bytes - sent
			}
			w, err := c.Write(buf[:n])
			sent += int64(w)
			if err != nil {
				fail(fmt.Errorf("after %d bytes: %w", sent, err))
			}
		}
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			fail(err)
		}
		d := time.Since(start)
		fmt.Printf("%d MiB to %s in %s: %.1f Mbit/s\n", *size, *connect, d.Round(time.Millisecond), float64(bytes)*8/d.Seconds()/1e6)
	default:
		flag.Usage()
		os.Exit(2)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
