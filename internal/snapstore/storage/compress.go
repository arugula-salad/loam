// Derived from github.com/e2b-dev/infra, packages/shared/pkg/storage/compress_encode.go,
// compress_decode.go, compress_upload.go, compress_config.go and buffer_pool.go at commit
// cf536c2 (Copyright 2023 FoundryLabs, Inc., Apache-2.0, see ../LICENSE-e2b). Modified for loam:
// a static config instead of environment and feature flags, no metering or logging, the
// uncompressed case through the same streaming path.

package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/klauspost/compress/zstd"
	lz4 "github.com/pierrec/lz4/v4"
	"golang.org/x/sync/errgroup"

	"github.com/arugula-salad/loam/internal/snapstore/header"
)

// CompressConfig says how layer objects are written.
type CompressConfig struct {
	// Type is the codec; CompressionNone stores the bytes as they are, without a frame table.
	Type header.CompressionType
	// Level is the codec's level (zstd: 1 fastest .. 4 best; lz4 ignores it).
	Level int
	// FrameSize is the uncompressed size of each independently compressed frame: the unit a
	// read fetches and decodes.
	FrameSize int
	// PartSize is how many compressed bytes a multipart upload part collects at least. S3 wants
	// 5 MiB or more for every part but the last.
	PartSize int64
	// EncodeWorkers is how many frames compress at once.
	EncodeWorkers int
	// Uploads is how many parts upload at once.
	Uploads int
}

// DefaultCompressConfig is what the spike measured: zstd level 1 in 2 MiB frames (a 64 MiB
// memory file to 0.97 MB).
func DefaultCompressConfig() CompressConfig {
	return CompressConfig{
		Type:          header.CompressionZstd,
		Level:         1,
		FrameSize:     2 << 20,
		PartSize:      16 << 20,
		EncodeWorkers: 4,
		Uploads:       4,
	}
}

func (c CompressConfig) withDefaults() CompressConfig {
	d := DefaultCompressConfig()
	if c.FrameSize <= 0 {
		c.FrameSize = d.FrameSize
	}
	if c.PartSize <= 0 {
		c.PartSize = d.PartSize
	}
	if c.EncodeWorkers <= 0 {
		c.EncodeWorkers = d.EncodeWorkers
	}
	if c.Uploads <= 0 {
		c.Uploads = d.Uploads
	}
	if c.Type == header.CompressionZstd && c.Level <= 0 {
		c.Level = 1
	}
	return c
}

// compressor compresses individual frames. Implementations are pooled and reused across frames
// within a single upload.
type compressor interface {
	compress(src []byte) ([]byte, error)
}

// lz4Compressor wraps a pooled lz4.Writer, reused via Reset between frames to avoid
// re-allocating its hash tables (~64KB).
type lz4Compressor struct{ w *lz4.Writer }

func (c *lz4Compressor) compress(src []byte) ([]byte, error) {
	var buf bytes.Buffer
	buf.Grow(lz4.CompressBlockBound(len(src)))
	c.w.Reset(&buf)
	if _, err := c.w.Write(src); err != nil {
		return nil, fmt.Errorf("lz4 compress: %w", err)
	}
	if err := c.w.Close(); err != nil {
		return nil, fmt.Errorf("lz4 compress close: %w", err)
	}
	return buf.Bytes(), nil
}

type zstdCompressor struct{ enc *zstd.Encoder }

func (z *zstdCompressor) compress(src []byte) ([]byte, error) {
	return z.enc.EncodeAll(src, make([]byte, 0, len(src))), nil
}

// noneCompressor stores frames as they are.
type noneCompressor struct{}

func (noneCompressor) compress(src []byte) ([]byte, error) { return bytes.Clone(src), nil }

// newCompressorPool returns a pool of compressors for the config, validating it eagerly: invalid
// zstd options fail here rather than in pool.Get.
func newCompressorPool(cfg CompressConfig) (*sync.Pool, error) {
	pool := &sync.Pool{}
	switch cfg.Type {
	case header.CompressionZstd:
		opts := []zstd.EOption{
			zstd.WithEncoderLevel(zstd.EncoderLevel(cfg.Level)),
			zstd.WithEncoderCRC(true),
			zstd.WithWindowSize(cfg.FrameSize),
			zstd.WithEncoderConcurrency(1),
		}
		first, err := zstd.NewWriter(nil, opts...)
		if err != nil {
			return nil, fmt.Errorf("zstd encoder: %w", err)
		}
		pool.Put(&zstdCompressor{enc: first})
		pool.New = func() any {
			enc, _ := zstd.NewWriter(nil, opts...)
			return &zstdCompressor{enc: enc}
		}
	case header.CompressionLZ4:
		opts := []lz4.Option{
			lz4.BlockSizeOption(lz4.Block4Mb),
			lz4.BlockChecksumOption(true),
			lz4.ChecksumOption(false),
			lz4.ConcurrencyOption(1),
			lz4.CompressionLevelOption(lz4.Fast),
		}
		first := lz4.NewWriter(nil)
		if err := first.Apply(opts...); err != nil {
			return nil, fmt.Errorf("lz4 encoder: %w", err)
		}
		pool.Put(&lz4Compressor{w: first})
		pool.New = func() any {
			w := lz4.NewWriter(nil)
			_ = w.Apply(opts...)
			return &lz4Compressor{w: w}
		}
	case header.CompressionNone:
		pool.New = func() any { return noneCompressor{} }
	default:
		return nil, fmt.Errorf("unsupported compression type: %s", cfg.Type)
	}
	return pool, nil
}

var lz4DecoderPool sync.Pool

// zstd decoder concurrency is 1: higher values hurt throughput for single 2 MiB frame decodes.
var zstdDecoderPool sync.Pool

// decompressReader decodes one frame. Its Close drains the decoder so the codec consumes the
// frame footer and surfaces a CRC or truncation error: a caller reading only the exact
// uncompressed frame size never pulls the footer through, so zstd would otherwise report success
// on a footer-corrupted or truncated frame. Bounded: the source is a single frame.
type decompressReader struct {
	inner   io.ReadCloser
	dec     io.Reader
	release func()
	readErr error
}

// NewDecompressReader wraps r with a decoder for ct.
func NewDecompressReader(r io.ReadCloser, ct header.CompressionType) (io.ReadCloser, error) {
	d := &decompressReader{inner: r}
	switch ct {
	case header.CompressionLZ4:
		dec, _ := lz4DecoderPool.Get().(*lz4.Reader)
		if dec == nil {
			dec = lz4.NewReader(r)
		} else {
			dec.Reset(r)
		}
		d.dec, d.release = dec, func() { dec.Reset(nil); lz4DecoderPool.Put(dec) }
	case header.CompressionZstd:
		dec, _ := zstdDecoderPool.Get().(*zstd.Decoder)
		var err error
		if dec == nil {
			dec, err = zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		} else if err = dec.Reset(r); err != nil {
			dec.Close()
		}
		if err != nil {
			return nil, fmt.Errorf("zstd decoder: %w", err)
		}
		d.dec, d.release = dec, func() { dec.Reset(nil); zstdDecoderPool.Put(dec) }
	default:
		return nil, fmt.Errorf("unsupported compression type: %s", ct)
	}
	return d, nil
}

func (d *decompressReader) Read(p []byte) (int, error) {
	n, err := d.dec.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		d.readErr = err
	}
	return n, err
}

func (d *decompressReader) Close() error {
	if d.readErr == nil {
		// lz4's WriteTo answers io.EOF for a stream already read to its end.
		if _, err := io.Copy(io.Discard, d.dec); err != nil && !errors.Is(err, io.EOF) {
			d.readErr = err
		}
	}
	d.release()
	err := d.inner.Close()
	if d.readErr != nil {
		return d.readErr
	}
	return err
}

// Uploaded is what UploadFramed wrote.
type Uploaded struct {
	// Frames is the object's frame table, nil when it is stored uncompressed.
	Frames *header.FrameTable
	// Size is the uncompressed size; Checksum its SHA-256.
	Size     int64
	Checksum [32]byte
	// Stored is the size of the object.
	Stored int64
}

type frame struct {
	uncompressedSize int
	compressed       []byte
}

type part struct {
	index          int
	frames         []*frame
	compressedSize atomic.Int64
	compress       *errgroup.Group
}

func newPart(index int, parent context.Context, workers int) (*part, context.Context) {
	p := &part{index: index}
	var ctx context.Context
	p.compress, ctx = errgroup.WithContext(parent)
	p.compress.SetLimit(workers)
	return p, ctx
}

func (p *part) addFrame(ctx context.Context, buf []byte, pool *sync.Pool, free func()) {
	f := &frame{uncompressedSize: len(buf)}
	p.frames = append(p.frames, f)
	p.compress.Go(func() error {
		defer free()
		if err := ctx.Err(); err != nil {
			return err
		}
		c := pool.Get().(compressor)
		out, err := c.compress(buf)
		pool.Put(c)
		if err != nil {
			return err
		}
		f.compressed = out
		p.compressedSize.Add(int64(len(out)))
		return nil
	})
}

// UploadFramed streams in into key as a multipart upload of independently compressed frames: a
// read loop cuts frames and hands them to compression workers, parts collect frames until they
// hold cfg.PartSize compressed bytes, and parts upload concurrently. An object no read can
// decode piecemeal would make every miss fetch the whole layer.
func UploadFramed(ctx context.Context, b Backend, key string, in io.Reader, cfg CompressConfig) (Uploaded, error) {
	cfg = cfg.withDefaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	compressors, err := newCompressorPool(cfg)
	if err != nil {
		return Uploaded{}, err
	}
	up, err := b.NewMultipart(ctx, key)
	if err != nil {
		return Uploaded{}, err
	}
	completed := false
	defer func() {
		if !completed {
			up.Abort(context.WithoutCancel(ctx))
		}
	}()

	// The read loop holds one slot for the duration of the stream; uploads take the rest.
	work, workCtx := errgroup.WithContext(ctx)
	work.SetLimit(cfg.Uploads + 1)
	q := make(chan *part, 2)
	hasher := sha256.New()
	var total int64
	work.Go(func() error {
		defer close(q)
		var bufs sync.Pool
		p, compressCtx := newPart(1, workCtx, cfg.EncodeWorkers)
		for {
			if err := workCtx.Err(); err != nil {
				return err
			}
			bp, _ := bufs.Get().(*[]byte)
			if bp == nil {
				b := make([]byte, cfg.FrameSize)
				bp = &b
			}
			n, err := io.ReadFull(in, *bp)
			eof := errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
			if err != nil && !eof {
				return fmt.Errorf("read frame: %w", err)
			}
			if n > 0 {
				hasher.Write((*bp)[:n])
				total += int64(n)
				p.addFrame(compressCtx, (*bp)[:n], compressors, func() { bufs.Put(bp) })
			}
			if eof {
				if len(p.frames) > 0 {
					select {
					case q <- p:
					case <-workCtx.Done():
						return workCtx.Err()
					}
				}
				return nil
			}
			if p.compressedSize.Load() >= cfg.PartSize {
				select {
				case q <- p:
				case <-workCtx.Done():
					return workCtx.Err()
				}
				p, compressCtx = newPart(p.index+1, workCtx, cfg.EncodeWorkers)
			}
		}
	})

	var sizes []header.FrameSize
	var stored int64
	var loopErr error
	parts := 0
	for p := range q {
		if err := p.compress.Wait(); err != nil {
			loopErr = fmt.Errorf("compress frames: %w", err)
			cancel()
			break
		}
		var compressed [][]byte
		for _, f := range p.frames {
			sizes = append(sizes, header.FrameSize{U: int32(f.uncompressedSize), C: int32(len(f.compressed))})
			compressed = append(compressed, f.compressed)
			stored += int64(len(f.compressed))
		}
		idx := p.index
		parts++
		work.Go(func() error { return up.UploadPart(workCtx, idx, compressed...) })
	}
	// Drain q so the read loop can exit, then wait for every upload in flight.
	for range q {
	}
	if err := errors.Join(loopErr, work.Wait()); err != nil {
		return Uploaded{}, err
	}
	// S3 refuses to complete an upload with no parts, and Garage an empty part: an empty
	// object is a plain empty PUT.
	if parts == 0 {
		up.Abort(context.WithoutCancel(ctx))
		completed = true
		if err := b.Put(ctx, key, []byte{}); err != nil {
			return Uploaded{}, fmt.Errorf("upload an empty object: %w", err)
		}
		res := Uploaded{}
		copy(res.Checksum[:], hasher.Sum(nil))
		if cfg.Type != header.CompressionNone {
			res.Frames = header.NewFrameTable(cfg.Type, nil)
		}
		return res, nil
	}
	if err := up.Complete(ctx); err != nil {
		return Uploaded{}, err
	}
	completed = true
	res := Uploaded{Size: total, Stored: stored}
	copy(res.Checksum[:], hasher.Sum(nil))
	if cfg.Type != header.CompressionNone {
		res.Frames = header.NewFrameTable(cfg.Type, sizes)
	}
	return res, nil
}
