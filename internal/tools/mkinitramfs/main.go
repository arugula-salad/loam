// Command mkinitramfs packs loam-agent as /init in a gzipped newc initramfs,
// the way scripts/build-initramfs.sh used to with cpio, so building it needs
// nothing but Go (CI runners have no cpio). The archive is reproducible: fixed
// owners, modes, inode numbers and mtimes, and a gzip header without a time
// or name.
//
//	go run ./internal/tools/mkinitramfs -init bin/loam-agent -o dist/initramfs.cpio.gz
package main

import (
	"bytes"
	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
)

// The mount points /init needs before the root disk is up, then /init itself.
var dirs = []string{".", "dev", "newroot", "proc"}

func main() {
	initPath := flag.String("init", "", "the static loam-agent binary to pack as /init")
	out := flag.String("o", "dist/initramfs.cpio.gz", "output file")
	flag.Parse()
	if *initPath == "" {
		log.Fatal("mkinitramfs: -init is required")
	}
	agent, err := os.ReadFile(*initPath)
	if err != nil {
		log.Fatal(err)
	}
	var archive bytes.Buffer
	w := &newc{w: &archive}
	for _, d := range dirs {
		w.entry(d, 0o040755, nil)
	}
	w.entry("init", 0o100755, agent)
	w.entry("TRAILER!!!", 0, nil)
	if w.err != nil {
		log.Fatal(w.err)
	}

	if err := os.MkdirAll(filepath.Dir(*out), 0o755); err != nil {
		log.Fatal(err)
	}
	f, err := os.Create(*out + ".tmp")
	if err != nil {
		log.Fatal(err)
	}
	zw, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	if _, err := io.Copy(zw, &archive); err != nil {
		log.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(*out+".tmp", *out); err != nil {
		log.Fatal(err)
	}
}

// newc writes the "new ASCII" cpio format the kernel unpacks: a 110-byte hex
// header, the NUL-terminated name and the data, each padded to 4 bytes.
type newc struct {
	w   io.Writer
	ino int
	err error
}

func (c *newc) entry(name string, mode uint32, data []byte) {
	if c.err != nil {
		return
	}
	c.ino++
	nlink := 1
	if mode&0o040000 != 0 {
		nlink = 2
	}
	ino := c.ino
	if name == "TRAILER!!!" {
		ino, nlink = 0, 1
	}
	hdr := fmt.Sprintf("070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
	buf := append([]byte(hdr), name...)
	buf = append(buf, 0)
	buf = pad(buf)
	buf = append(buf, data...)
	buf = pad(buf)
	_, c.err = c.w.Write(buf)
}

func pad(b []byte) []byte {
	for len(b)%4 != 0 {
		b = append(b, 0)
	}
	return b
}
