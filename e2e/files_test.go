//go:build e2e

package e2e

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/arugula-salad/loam/pkg/api"
	"github.com/arugula-salad/loam/pkg/client"
)

func TestFiles(t *testing.T) {
	c := newClient(t)
	ctx := testCtx(t, 5*time.Minute)
	sb := newSandbox(t, ctx, c, "files", api.CreateSandbox{})
	loam := client.AsUser("loam")

	t.Run("write read stat as the user", func(t *testing.T) {
		payload := bytes.Repeat([]byte("0123456789abcdef"), 1<<16) // 1 MiB
		st, err := c.WriteFile(ctx, sb.Name, "/home/loam/proj/data/blob.bin", bytes.NewReader(payload), client.Mode(0o640), client.MakeParents(), loam)
		if err != nil || st.Size != int64(len(payload)) || st.Mode != "0640" || st.Type != "file" {
			t.Fatalf("write: %+v %v", st, err)
		}
		got, err := c.ReadFile(ctx, sb.Name, "/home/loam/proj/data/blob.bin")
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("read back %d bytes: %v", len(got), err)
		}
		st, err = c.Stat(ctx, sb.Name, "/home/loam/proj/data/blob.bin")
		if err != nil || st.Name != "blob.bin" || st.Size != int64(len(payload)) || st.Mode != "0640" || st.MTime.IsZero() {
			t.Fatalf("stat: %+v %v", st, err)
		}
		// What the API wrote as loam must look like loam's own files from inside.
		if out := sh(t, ctx, c, sb.Name, "stat -c '%U %a' /home/loam/proj/data/blob.bin /home/loam/proj/data"); out != "loam 640\nloam 755\n" {
			t.Fatalf("seen from inside: %q", out)
		}
	})

	t.Run("root by default, permissions for a user", func(t *testing.T) {
		if _, err := c.WriteFile(ctx, sb.Name, "/root/secret", strings.NewReader("s"), client.Mode(0o600)); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, ctx, c, sb.Name, "sudo stat -c '%U %a' /root/secret"); out != "root 600\n" {
			t.Fatalf("root's file: %q", out)
		}
		if _, err := c.ReadFile(ctx, sb.Name, "/root/secret", loam); err == nil {
			t.Fatal("loam read root's 0600 file")
		}
		if _, err := c.WriteFile(ctx, sb.Name, "/etc/loam-e2e", strings.NewReader("x"), loam); err == nil {
			t.Fatal("loam wrote into /etc")
		}
		if err := c.Remove(ctx, sb.Name, "/root/secret", loam); err == nil {
			t.Fatal("loam deleted root's file")
		}
	})

	t.Run("mkdir list rename delete", func(t *testing.T) {
		if st, err := c.Mkdir(ctx, sb.Name, api.Mkdir{Path: "/home/loam/proj/empty/deeper", Parents: true}, loam); err != nil || st.Type != "dir" {
			t.Fatalf("mkdir: %+v %v", st, err)
		}
		if _, err := c.Rename(ctx, sb.Name, "/home/loam/proj/data/blob.bin", "/home/loam/proj/data/moved.bin", loam); err != nil {
			t.Fatal(err)
		}
		entries, err := c.List(ctx, sb.Name, "/home/loam/proj/data")
		if err != nil || len(entries) != 1 || entries[0].Name != "moved.bin" || entries[0].Path != "/home/loam/proj/data/moved.bin" {
			t.Fatalf("list: %+v %v", entries, err)
		}
		if _, err := c.Stat(ctx, sb.Name, "/home/loam/proj/data/blob.bin"); !client.IsNotFound(err) {
			t.Fatalf("stat of the old name: %v", err)
		}
		if _, err := c.ReadFile(ctx, sb.Name, "/home/loam/nope"); !client.IsNotFound(err) {
			t.Fatalf("missing file: %v", err)
		}
		if err := c.Remove(ctx, sb.Name, "/home/loam/proj", loam); err == nil {
			t.Fatal("a non-recursive delete of a non-empty directory succeeded")
		}
		if err := c.Remove(ctx, sb.Name, "/home/loam/proj", loam, client.Recursive()); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Stat(ctx, sb.Name, "/home/loam/proj"); !client.IsNotFound(err) {
			t.Fatalf("after a recursive delete: %v", err)
		}
	})

	t.Run("tar up and down", func(t *testing.T) {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "a/", Typeflag: tar.TypeDir, Mode: 0o755})
		tw.WriteHeader(&tar.Header{Name: "a/b.txt", Typeflag: tar.TypeReg, Mode: 0o600, Size: 5})
		tw.Write([]byte("hello"))
		tw.WriteHeader(&tar.Header{Name: "run.sh", Typeflag: tar.TypeReg, Mode: 0o755, Size: 9})
		tw.Write([]byte("#!/bin/sh"))
		tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "a/b.txt"})
		tw.Close()
		// The directory doesn't exist yet: PUT creates it.
		if err := c.PutTar(ctx, sb.Name, "/home/loam/tree", &buf, loam); err != nil {
			t.Fatal(err)
		}
		if out := sh(t, ctx, c, sb.Name, "cd /home/loam/tree && cat link && echo && stat -c '%U %a %n' a/b.txt run.sh"); out != "hello\nloam 600 a/b.txt\nloam 755 run.sh\n" {
			t.Fatalf("extracted: %q", out)
		}
		r, err := c.GetTar(ctx, sb.Name, "/home/loam/tree")
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var names []string
		tr := tar.NewReader(r)
		for {
			h, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			n := strings.TrimSuffix(strings.TrimPrefix(h.Name, "./"), "/")
			if n == "" || n == "." {
				continue
			}
			if n == "a/b.txt" {
				if b, _ := io.ReadAll(tr); string(b) != "hello" {
					t.Fatalf("a/b.txt in the tar: %q", b)
				}
			}
			names = append(names, n)
		}
		sort.Strings(names)
		if strings.Join(names, " ") != "a a/b.txt link run.sh" {
			t.Fatalf("tar entries (relative to the path): %q", names)
		}
	})

	t.Run("watch", func(t *testing.T) {
		if _, err := c.Mkdir(ctx, sb.Name, api.Mkdir{Path: "/home/loam/w/sub", Parents: true}, loam); err != nil {
			t.Fatal(err)
		}
		wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		w, err := c.WatchFiles(wctx, sb.Name, "/home/loam/w", client.Recursive())
		if err != nil {
			t.Fatal(err)
		}
		defer w.Close()
		events := make(chan api.WatchEvent, 64)
		go func() {
			defer close(events)
			for {
				ev, err := w.Next()
				if err != nil {
					return
				}
				events <- ev
			}
		}()
		time.Sleep(500 * time.Millisecond) // let the watch settle before the changes
		sh(t, ctx, c, sb.Name, "echo x > /home/loam/w/sub/f; sleep 0.2; rm /home/loam/w/sub/f")
		want := map[string]bool{"create": false, "write": false, "remove": false}
		for ev := range events {
			if ev.Path == "/home/loam/w/sub/f" {
				want[ev.Type] = true
			}
			if want["create"] && want["write"] && want["remove"] {
				return
			}
		}
		t.Fatalf("watch saw %v", want)
	})
}
