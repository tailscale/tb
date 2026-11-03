// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package image describes mantsana's on-disk VM images.
//
// An image is a directory of plain files: a manifest, a raw disk image, and
// the macOS auxiliary storage (NVRAM) blob. A warm subdirectory holds a
// host-local memory snapshot for fast restore; it is derived data, keyed to
// the host OS build, and rebuilt when stale.
//
// For now images are just files on local disk. Later the disk will be
// fetched lazily from a content-addressed chunk store; that plugs in below
// this package at the NBD BaseImage seam, so this layout does not change.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// File names within an image directory and its warm subdirectory.
const (
	ManifestFile = "manifest.json"
	DiskFile     = "disk.img"
	AuxFile      = "nvram.img" // not "aux.img": AUX is a reserved file name on Windows
	WarmDir      = "warm"
	StateFile    = "state.vzvmsave" // in WarmDir only
	WarmFile     = "warm.json"      // in WarmDir only
)

// Display is a guest display geometry. It is part of the image because a
// memory snapshot bakes it into the saved configuration.
type Display struct {
	Width  int `json:"width"`
	Height int `json:"height"`
	PPI    int `json:"ppi"`
}

// DefaultDisplay is used when a manifest doesn't specify a display.
var DefaultDisplay = Display{Width: 1280, Height: 800, PPI: 72}

// DefaultMTU is used when a manifest doesn't specify an MTU.
const DefaultMTU = 1500

// DefaultMAC is the guest MAC address images use unless they have a reason
// not to. It is guestnet's default guest MAC, so every image looks the same
// to guestnet; each guest's network is private, so there is no conflict.
var DefaultMAC = MACAddr{0x02, 0x76, 0x7a, 0x00, 0x00, 0x02}

// MACAddr is a net.HardwareAddr that encodes as text in JSON.
type MACAddr net.HardwareAddr

// String returns the colon-separated form, or "" for an empty address.
func (m MACAddr) String() string { return net.HardwareAddr(m).String() }

// MarshalText implements encoding.TextMarshaler.
func (m MACAddr) MarshalText() ([]byte, error) { return []byte(m.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (m *MACAddr) UnmarshalText(b []byte) error {
	if len(b) == 0 {
		*m = nil
		return nil
	}
	hw, err := net.ParseMAC(string(b))
	if err != nil {
		return err
	}
	if len(hw) != 6 {
		return fmt.Errorf("MAC %q is not 48 bits", b)
	}
	*m = MACAddr(hw)
	return nil
}

// Manifest describes an image. The hardware model, machine identifier, MAC,
// display and MTU are image properties because a memory snapshot depends on
// them. Two VMs from one image share them, which is fine since their
// networks are isolated.
type Manifest struct {
	Version int    `json:"version"`
	OS      string `json:"os"`   // "macos" (later "linux")
	Arch    string `json:"arch"` // "arm64"

	// HardwareModel and MachineID are the VZMacHardwareModel and
	// VZMacMachineIdentifier dataRepresentation blobs (base64 in JSON).
	// Empty for non-macOS images.
	HardwareModel []byte `json:"hardware_model,omitempty"`
	MachineID     []byte `json:"machine_id,omitempty"`

	MAC         MACAddr `json:"mac,omitempty"`
	CPUs        int     `json:"cpus"`
	MemoryBytes uint64  `json:"memory_bytes"`
	Display     Display `json:"display"`
	MTU         uint32  `json:"mtu"`
	DiskBytes   int64   `json:"disk_bytes,omitempty"`

	Created     time.Time `json:"created,omitzero"`
	Description string    `json:"description,omitempty"`
}

// fillDefaults sets unset optional fields.
func (m *Manifest) fillDefaults() {
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Arch == "" {
		m.Arch = "arm64"
	}
	if m.Display == (Display{}) {
		m.Display = DefaultDisplay
	}
	if m.MTU == 0 {
		m.MTU = DefaultMTU
	}
}

// Validate reports whether m is usable.
func (m *Manifest) Validate() error {
	if m.Version != 1 {
		return fmt.Errorf("unsupported manifest version %d", m.Version)
	}
	switch m.OS {
	case "macos", "linux":
	default:
		return fmt.Errorf("unsupported os %q", m.OS)
	}
	if m.CPUs <= 0 || m.MemoryBytes == 0 {
		return errors.New("cpus and memory_bytes must be set")
	}
	// Virtualization.framework rejects network MTUs outside this range.
	if m.MTU < 1500 || m.MTU > 65535 {
		return fmt.Errorf("mtu %d out of range 1500..65535", m.MTU)
	}
	return nil
}

// LoadManifest reads and validates the manifest at path, filling defaults.
func LoadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := new(Manifest)
	if err := json.Unmarshal(b, m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	m.fillDefaults()
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// SaveManifest atomically writes m to path.
func SaveManifest(path string, m *Manifest) error {
	return writeJSON(path, m)
}

// WarmInfo records what a warm snapshot was built against, so a stale one is
// never restored.
type WarmInfo struct {
	HostOSBuild     string    `json:"host_os_build"`
	MantsanaVersion string    `json:"mantsana_version,omitempty"`
	ManifestHash    string    `json:"manifest_hash"`
	CPUs            int       `json:"cpus"`
	MemoryBytes     uint64    `json:"memory_bytes"`
	Created         time.Time `json:"created,omitzero"`

	// Stale is set when a restore from this snapshot failed. StaleReason
	// says why.
	Stale       bool   `json:"stale,omitempty"`
	StaleReason string `json:"stale_reason,omitempty"`
}

// Image is an image directory on disk.
type Image struct {
	Name     string
	Dir      string
	Manifest Manifest
	// Warm is the warm snapshot's info, or nil if the image has no complete
	// warm snapshot.
	Warm *WarmInfo
}

// DiskPath returns the path to the pristine raw disk image.
func (im *Image) DiskPath() string { return filepath.Join(im.Dir, DiskFile) }

// AuxPath returns the path to the pristine auxiliary storage blob.
func (im *Image) AuxPath() string { return filepath.Join(im.Dir, AuxFile) }

// WarmPath returns the path of file within the warm subdirectory, or the
// subdirectory itself if file is empty.
func (im *Image) WarmPath(file string) string {
	return filepath.Join(im.Dir, WarmDir, file)
}

// ManifestHash returns a stable hash of the manifest file's contents, so a
// changed image invalidates its warm snapshot.
func (im *Image) ManifestHash() (string, error) {
	b, err := os.ReadFile(filepath.Join(im.Dir, ManifestFile))
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// WarmUsable reports whether the warm snapshot can be restored on a host
// running hostOSBuild with the given CPU count and memory size. If not, why
// says why, for logs and the API.
func (im *Image) WarmUsable(hostOSBuild string, cpus int, memoryBytes uint64) (ok bool, why string) {
	w := im.Warm
	switch {
	case w == nil:
		return false, "no warm snapshot"
	case w.Stale:
		return false, "stale: " + w.StaleReason
	case w.HostOSBuild != hostOSBuild:
		return false, fmt.Sprintf("host OS changed (snapshot %s, host %s)", w.HostOSBuild, hostOSBuild)
	case w.CPUs != cpus || w.MemoryBytes != memoryBytes:
		return false, fmt.Sprintf("snapshot is %d CPUs/%d bytes, want %d/%d", w.CPUs, w.MemoryBytes, cpus, memoryBytes)
	}
	hash, err := im.ManifestHash()
	if err != nil {
		return false, err.Error()
	}
	if hash != w.ManifestHash {
		return false, "image manifest changed since snapshot"
	}
	return true, ""
}

// SaveWarmInfo writes wi as the warm snapshot's info, creating the warm
// directory if needed, and sets im.Warm.
func (im *Image) SaveWarmInfo(wi *WarmInfo) error {
	if err := os.MkdirAll(im.WarmPath(""), 0o755); err != nil {
		return err
	}
	if err := writeJSON(im.WarmPath(WarmFile), wi); err != nil {
		return err
	}
	im.Warm = wi
	return nil
}

// MarkWarmStale records that the warm snapshot must not be used again, for
// example because restoring it failed.
func (im *Image) MarkWarmStale(reason string) error {
	if im.Warm == nil {
		return nil
	}
	wi := *im.Warm
	wi.Stale = true
	wi.StaleReason = reason
	return im.SaveWarmInfo(&wi)
}

// DeleteWarm removes the warm snapshot.
func (im *Image) DeleteWarm() error {
	im.Warm = nil
	return os.RemoveAll(im.WarmPath(""))
}

// loadWarm loads the warm info. A missing warm directory is (nil, nil). A
// warm.json without all of its files is an error, since it means a warm-up
// was interrupted or files were removed by hand.
func (im *Image) loadWarm() (*WarmInfo, error) {
	b, err := os.ReadFile(im.WarmPath(WarmFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	wi := new(WarmInfo)
	if err := json.Unmarshal(b, wi); err != nil {
		return nil, fmt.Errorf("%s: %w", im.WarmPath(WarmFile), err)
	}
	for _, f := range []string{DiskFile, AuxFile, StateFile} {
		if _, err := os.Stat(im.WarmPath(f)); err != nil {
			return nil, fmt.Errorf("incomplete warm snapshot: %w", err)
		}
	}
	return wi, nil
}

// NotFoundError is returned by [Store.Get] for a missing image.
type NotFoundError struct {
	Name string
}

func (e *NotFoundError) Error() string { return fmt.Sprintf("image %q not found", e.Name) }

// Store is a directory of images, one subdirectory each.
type Store struct {
	Root string
}

// DefaultStoreDir returns the default image store directory for the current
// user: on macOS, ~/Library/Application Support/mantsana/images. It is not
// ~/Library/Caches, even though images can be downloaded again, because
// the system may purge caches, and warm snapshots are costly to rebuild.
func DefaultStoreDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = home
	}
	return filepath.Join(dir, "mantsana", "images")
}

// validName reports whether name is usable as a single path element.
func validName(name string) bool {
	return name != "" && name != "." && name != ".." &&
		!strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, ".")
}

// Get loads the named image.
func (s *Store) Get(name string) (*Image, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid image name %q", name)
	}
	dir := filepath.Join(s.Root, name)
	m, err := LoadManifest(filepath.Join(dir, ManifestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, &NotFoundError{Name: name}
	}
	if err != nil {
		return nil, fmt.Errorf("image %q: %w", name, err)
	}
	im := &Image{Name: name, Dir: dir, Manifest: *m}
	if im.Warm, err = im.loadWarm(); err != nil {
		return nil, fmt.Errorf("image %q: %w", name, err)
	}
	return im, nil
}

// List returns the images in the store, sorted by name. Directories without
// a valid manifest are skipped.
func (s *Store) List() ([]*Image, error) {
	ents, err := os.ReadDir(s.Root)
	if err != nil {
		return nil, err
	}
	var out []*Image
	for _, e := range ents {
		if !e.IsDir() || !validName(e.Name()) {
			continue
		}
		if im, err := s.Get(e.Name()); err == nil {
			out = append(out, im)
		}
	}
	return out, nil
}

// Create makes a new image directory containing only a manifest, filling
// manifest defaults. The caller then writes the disk and aux files (for
// example with a macOS installer run). It fails if the image exists.
func (s *Store) Create(name string, m Manifest) (*Image, error) {
	if !validName(name) {
		return nil, fmt.Errorf("invalid image name %q", name)
	}
	m.fillDefaults()
	if m.Created.IsZero() {
		m.Created = time.Now().UTC().Truncate(time.Second)
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.Root, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return nil, err
	}
	if err := SaveManifest(filepath.Join(dir, ManifestFile), &m); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	return &Image{Name: name, Dir: dir, Manifest: m}, nil
}

// writeJSON atomically writes v as indented JSON to path.
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
