// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package ocidl downloads a macOS VM image published to an OCI registry, as
// used by corp CI today (for example
// ghcr.io/cirruslabs/macos-tahoe-xcode:26.5), into a mantsana image
// directory (see package mantsana/image):
//
//	manifest.json      mantsana image manifest, written last
//	disk.img           raw disk, sparse (all-zero 4 KiB pages are left as holes)
//	nvram.img          the VM's auxiliary storage (NVRAM)
//	oci-manifest.json  the OCI manifest it was downloaded from
//
// It is for development and for the transition from Tart; mantsana's own
// image distribution will be different (see DESIGN.md).
//
// Disk layers are 512 MiB chunks of the raw disk, each compressed with
// Apple's LZ4 stream framing and annotated with its uncompressed size and
// SHA-256, which is verified. Chunks download in parallel, and an
// interrupted download resumes where it left off.
package ocidl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tailscale/tb/mantsana/image"
	"tailscale.com/types/logger"
)

// Layer media types and annotations of these VM images.
const (
	mediaTypeDisk   = "application/vnd.cirruslabs.tart.disk.v2"
	mediaTypeConfig = "application/vnd.cirruslabs.tart.config.v1"
	mediaTypeNVRAM  = "application/vnd.cirruslabs.tart.nvram.v1"

	annotUncompressedSize   = "org.cirruslabs.tart.uncompressed-size"
	annotUncompressedDigest = "org.cirruslabs.tart.uncompressed-content-digest"
)

// progressFile records completed disk chunks, one index per line, after a
// first line naming the manifest digest they belong to.
const progressFile = ".oci-img-dl-progress"

// ociManifestFile is where the OCI manifest is saved, for reference.
const ociManifestFile = "oci-manifest.json"

const pageSize = 4096

// Options configures a download.
type Options struct {
	// Parallel is how many disk chunks to download at once. Default 8.
	Parallel int
	// Logf receives progress messages. Default logger.Discard.
	Logf logger.Logf
}

// ErrComplete is returned (wrapped) by Download when the directory already
// holds a complete image.
var ErrComplete = errors.New("already a complete image")

// chunk is one disk layer and where its contents go in disk.img.
type chunk struct {
	index int
	desc  descriptor
	off   int64
	size  int64 // uncompressed
}

// Download downloads the image r into the image directory dir, creating it
// if needed. If dir holds an interrupted download of the same image, it
// resumes; if it holds a complete image, it returns an error wrapping
// ErrComplete.
func Download(ctx context.Context, r Ref, dir string, opts Options) error {
	logf := opts.Logf
	if logf == nil {
		logf = logger.Discard
	}
	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = 8
	}
	c := newClient(r)
	m, rawManifest, err := c.manifest(ctx, r)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(rawManifest)
	manifestDigest := "sha256:" + hex.EncodeToString(sum[:])

	var (
		chunks        []chunk
		config, nvram *descriptor
		total         int64
		compressed    int64
	)
	for i := range m.Layers {
		l := &m.Layers[i]
		switch l.MediaType {
		case mediaTypeDisk:
			n, err := strconv.ParseInt(l.Annotations[annotUncompressedSize], 10, 64)
			if err != nil {
				return fmt.Errorf("disk layer %d: bad %s: %v", len(chunks), annotUncompressedSize, err)
			}
			if l.Annotations[annotUncompressedDigest] == "" {
				return fmt.Errorf("disk layer %d: no %s", len(chunks), annotUncompressedDigest)
			}
			if total%pageSize != 0 {
				return fmt.Errorf("disk layer %d starts at unaligned offset %d", len(chunks), total)
			}
			chunks = append(chunks, chunk{index: len(chunks), desc: *l, off: total, size: n})
			total += n
			compressed += l.Size
		case mediaTypeConfig:
			config = l
		case mediaTypeNVRAM:
			nvram = l
		default:
			return fmt.Errorf("unsupported layer media type %q", l.MediaType)
		}
	}
	if len(chunks) == 0 || config == nil || nvram == nil {
		return errors.New("manifest lacks a disk, config or NVRAM layer")
	}
	logf("%s (%s): %d disk chunks, %.1f GB disk, %.1f GB to download",
		r, manifestDigest[:19], len(chunks), float64(total)/1e9, float64(compressed)/1e9)

	if _, err := os.Stat(filepath.Join(dir, image.ManifestFile)); err == nil {
		return fmt.Errorf("%s is %w; remove it to download again", dir, ErrComplete)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	done, err := loadProgress(logf, dir, manifestDigest)
	if err != nil {
		return err
	}
	diskPath := filepath.Join(dir, image.DiskFile)
	flags := os.O_RDWR | os.O_CREATE
	if len(done) == 0 {
		flags |= os.O_TRUNC // a fresh start, or a different image
	}
	disk, err := os.OpenFile(diskPath, flags, 0o644)
	if err != nil {
		return err
	}
	defer disk.Close()
	if err := disk.Truncate(total); err != nil {
		return err
	}
	prog, err := openProgress(dir, manifestDigest, len(done) == 0)
	if err != nil {
		return err
	}
	defer prog.Close()

	// Small layers first, so a config we can't use fails before the big
	// download.
	configJSON, err := fetchSmall(ctx, logf, c, *config)
	if err != nil {
		return err
	}
	im, err := imageManifest(configJSON, total, r, manifestDigest)
	if err != nil {
		return err
	}
	aux, err := fetchSmall(ctx, logf, c, *nvram)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, image.AuxFile), aux, 0o644); err != nil {
		return err
	}

	var todo []chunk
	var todoBytes int64
	for _, ch := range chunks {
		if !done[ch.index] {
			todo = append(todo, ch)
			todoBytes += ch.desc.Size
		}
	}
	if len(done) > 0 {
		logf("resuming: %d of %d chunks already done", len(chunks)-len(todo), len(chunks))
	}

	var (
		next      atomic.Int64
		gotBytes  atomic.Int64
		doneCount atomic.Int64
		progMu    sync.Mutex
		wg        sync.WaitGroup
		errOnce   sync.Once
		firstErr  error
	)
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	fail := func(err error) {
		errOnce.Do(func() { firstErr = err; cancel() })
	}
	t0 := time.Now()
	go func() {
		tick := time.NewTicker(10 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				got := gotBytes.Load()
				el := time.Since(t0).Seconds()
				rate := float64(got) / el
				eta := time.Duration(float64(todoBytes-got)/max(rate, 1)) * time.Second
				logf("%d/%d chunks, %.1f/%.1f GB, %.0f MB/s, ETA %v",
					doneCount.Load(), len(todo), float64(got)/1e9, float64(todoBytes)/1e9, rate/1e6, eta.Round(time.Second))
			}
		}
	}()
	for range min(parallel, len(todo)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= len(todo) || ctx.Err() != nil {
					return
				}
				ch := todo[i]
				err := retry(ctx, logf, fmt.Sprintf("chunk %d", ch.index), func() error {
					return fetchChunk(ctx, c, disk, ch, &gotBytes)
				})
				if err != nil {
					fail(fmt.Errorf("chunk %d: %w", ch.index, err))
					return
				}
				progMu.Lock()
				_, err = fmt.Fprintf(prog, "%d\n", ch.index)
				progMu.Unlock()
				if err != nil {
					fail(err)
					return
				}
				doneCount.Add(1)
			}
		}()
	}
	wg.Wait()
	if parent.Err() != nil {
		return fmt.Errorf("interrupted after %d of %d chunks (rerun the same command to resume)", doneCount.Load(), len(todo))
	}
	if firstErr != nil {
		return fmt.Errorf("%w (rerun the same command to resume)", firstErr)
	}
	if err := disk.Sync(); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, ociManifestFile), rawManifest, 0o644); err != nil {
		return err
	}
	// The image manifest goes last: its presence is what makes the
	// directory an image.
	if err := image.SaveManifest(filepath.Join(dir, image.ManifestFile), im); err != nil {
		return err
	}
	prog.Close()
	os.Remove(filepath.Join(dir, progressFile))
	logf("done in %v: image %q in %s", time.Since(t0).Round(time.Second), filepath.Base(dir), dir)
	return nil
}

// vmConfig is the VM configuration layer of these images.
type vmConfig struct {
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	HardwareModel []byte `json:"hardwareModel"` // VZMacHardwareModel.dataRepresentation
	ECID          []byte `json:"ecid"`          // VZMacMachineIdentifier.dataRepresentation
	CPUCount      int    `json:"cpuCount"`
	MemorySize    uint64 `json:"memorySize"`
	DiskFormat    string `json:"diskFormat"`
	Display       struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"display"`
}

// imageManifest converts the VM configuration layer into a mantsana image
// manifest.
func imageManifest(configJSON []byte, diskBytes int64, r Ref, manifestDigest string) (*image.Manifest, error) {
	var vc vmConfig
	if err := json.Unmarshal(configJSON, &vc); err != nil {
		return nil, fmt.Errorf("parsing VM config: %w", err)
	}
	if vc.OS != "darwin" || vc.Arch != "arm64" {
		return nil, fmt.Errorf("VM is %s/%s; only darwin/arm64 is supported", vc.OS, vc.Arch)
	}
	if vc.DiskFormat != "" && vc.DiskFormat != "raw" {
		return nil, fmt.Errorf("disk format %q; only raw is supported", vc.DiskFormat)
	}
	if len(vc.HardwareModel) == 0 || len(vc.ECID) == 0 {
		return nil, errors.New("VM config lacks hardwareModel or ecid")
	}
	m := &image.Manifest{
		Version:       1,
		OS:            "macos",
		Arch:          "arm64",
		HardwareModel: vc.HardwareModel,
		MachineID:     vc.ECID,
		// Our fixed guest MAC rather than the image's, so every image looks
		// the same to guestnet.
		MAC:         image.DefaultMAC,
		CPUs:        vc.CPUCount,
		MemoryBytes: vc.MemorySize,
		Display:     image.DefaultDisplay,
		MTU:         image.DefaultMTU,
		DiskBytes:   diskBytes,
		Created:     time.Now().UTC().Truncate(time.Second),
		Description: fmt.Sprintf("downloaded from %s (%s)", r, manifestDigest),
	}
	if vc.Display.Width > 0 && vc.Display.Height > 0 {
		m.Display = image.Display{Width: vc.Display.Width, Height: vc.Display.Height, PPI: 72}
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// fetchChunk downloads one disk chunk, writes its non-zero pages to disk,
// and verifies its uncompressed digest. got counts compressed bytes read.
func fetchChunk(ctx context.Context, c *client, disk *os.File, ch chunk, got *atomic.Int64) error {
	body, err := c.blob(ctx, ch.desc.Digest)
	if err != nil {
		return err
	}
	defer body.Close()
	var counted int64
	defer func() { got.Add(-counted) }() // only completed chunks count toward progress
	h := sha256.New()
	off := ch.off
	cr := &countingReader{r: body, n: got, local: &counted}
	err = decodeAppleLZ4(cr, func(b []byte) error {
		if off+int64(len(b)) > ch.off+ch.size {
			return errors.New("chunk decodes to more than its uncompressed size")
		}
		h.Write(b)
		if err := writeNonZero(disk, b, off); err != nil {
			return err
		}
		off += int64(len(b))
		return nil
	})
	if err != nil {
		return err
	}
	if off != ch.off+ch.size {
		return fmt.Errorf("chunk decoded to %d bytes, want %d", off-ch.off, ch.size)
	}
	want := ch.desc.Annotations[annotUncompressedDigest]
	if gotDigest := "sha256:" + hex.EncodeToString(h.Sum(nil)); gotDigest != want {
		return fmt.Errorf("uncompressed digest %s, want %s", gotDigest, want)
	}
	counted = 0 // keep this chunk's bytes in the total
	return nil
}

// writeNonZero writes b at off, skipping all-zero 4 KiB pages so they stay
// holes in the sparse file. off must be page aligned.
func writeNonZero(f *os.File, b []byte, off int64) error {
	var zeros [pageSize]byte
	start := -1 // start of the current run of non-zero pages
	flush := func(end int) error {
		if start < 0 {
			return nil
		}
		_, err := f.WriteAt(b[start:end], off+int64(start))
		start = -1
		return err
	}
	for i := 0; i < len(b); i += pageSize {
		p := b[i:min(i+pageSize, len(b))]
		if bytes.Equal(p, zeros[:len(p)]) {
			if err := flush(i); err != nil {
				return err
			}
			continue
		}
		if start < 0 {
			start = i
		}
	}
	return flush(len(b))
}

// countingReader counts bytes read into two counters.
type countingReader struct {
	r     io.Reader
	n     *atomic.Int64
	local *int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	*c.local += int64(n)
	return n, err
}

// retry calls f until it succeeds, up to 5 times with exponential backoff,
// logging failures as what.
func retry(ctx context.Context, logf logger.Logf, what string, f func() error) error {
	var err error
	for attempt := range 5 {
		if err = f(); err == nil || ctx.Err() != nil {
			return err
		}
		logf("%s, attempt %d: %v", what, attempt+1, err)
		select {
		case <-time.After(time.Second << attempt):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

// fetchSmall downloads a small layer, verifying its digest, with retries.
func fetchSmall(ctx context.Context, logf logger.Logf, c *client, d descriptor) (b []byte, err error) {
	err = retry(ctx, logf, d.MediaType+" layer", func() error {
		b, err = fetchSmallOnce(ctx, c, d)
		return err
	})
	return b, err
}

func fetchSmallOnce(ctx context.Context, c *client, d descriptor) ([]byte, error) {
	if d.Size > 1<<30 {
		return nil, fmt.Errorf("%s layer is %d bytes; too big", d.MediaType, d.Size)
	}
	body, err := c.blob(ctx, d.Digest)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	b, err := io.ReadAll(io.LimitReader(body, d.Size+1))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	if got := "sha256:" + hex.EncodeToString(sum[:]); got != d.Digest {
		return nil, fmt.Errorf("%s layer: digest %s, want %s", d.MediaType, got, d.Digest)
	}
	return b, nil
}

// loadProgress returns the chunks already downloaded into dir for the
// manifest with the given digest.
func loadProgress(logf logger.Logf, dir, manifestDigest string) (map[int]bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, progressFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[int]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	done := map[int]bool{}
	if len(lines) == 0 || lines[0] != manifestDigest {
		logf("existing download in %s is of a different image; starting over", dir)
		return done, nil
	}
	for _, l := range lines[1:] {
		if i, err := strconv.Atoi(l); err == nil {
			done[i] = true
		}
	}
	return done, nil
}

// openProgress opens the progress file for appending, starting it afresh
// if fresh.
func openProgress(dir, manifestDigest string, fresh bool) (*os.File, error) {
	path := filepath.Join(dir, progressFile)
	if fresh {
		if err := os.WriteFile(path, []byte(manifestDigest+"\n"), 0o644); err != nil {
			return nil, err
		}
	}
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
}
