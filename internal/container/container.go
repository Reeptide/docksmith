// Package container manages persistent container records.
//
// There is no daemon. A container is a directory on disk holding everything
// needed to describe it after the process that started it has gone away:
//
//	<root>/containers/<id>/
//	    config.json      the Record below
//	    rootfs/          assembled image layers
//	    container.log    stdout and stderr, interleaved
package container

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"docksmith/internal/network"
	"docksmith/internal/runtime"
)

// State is where a container is in its lifecycle.
type State string

const (
	StateCreated State = "created"
	StateRunning State = "running"
	StateExited  State = "exited"
)

// Record is the on-disk description of a container.
type Record struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Image   string `json:"image"`
	ImageID string `json:"imageId"`

	Command    []string          `json:"command"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Mounts     []runtime.Mount   `json:"mounts,omitempty"`

	// Networking. Rules records the iptables rules installed for this
	// container so teardown deletes exactly what was added, rather than
	// pattern-matching a ruleset other tools are also editing.
	NetMode string                 `json:"netMode,omitempty"`
	IP      string                 `json:"ip,omitempty"`
	Ports   []network.PortMapping  `json:"ports,omitempty"`
	Rules   []network.Rule         `json:"rules,omitempty"`
	Network *runtime.NetworkConfig `json:"network,omitempty"`

	State    State `json:"state"`
	Pid      int   `json:"pid,omitempty"`
	ExitCode int   `json:"exitCode"`
	Detached bool  `json:"detached"`
	AutoRm   bool  `json:"autoRm"`

	Created    string `json:"created"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`

	// StartTime is field 22 of /proc/<pid>/stat, in clock ticks since boot.
	// Recorded alongside Pid because a pid on its own is not a stable handle:
	// pids are recycled, so after a reboot or pid wraparound a stale record
	// could match an unrelated live process — and `stop` would then signal
	// somebody else's process as root.
	StartTime uint64 `json:"startTime,omitempty"`

	// Dir is the container's directory. Not serialised; set on load.
	Dir string `json:"-"`
}

// NewID returns a 64-character hex id, displayed truncated to 12.
func NewID() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating container id: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// ShortID truncates an id for display, matching the width used by `images`.
func ShortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ContainersDir returns the directory holding all container records.
func ContainersDir(stateRoot string) string {
	return filepath.Join(stateRoot, "containers")
}

// Dir returns one container's directory.
func Dir(stateRoot, id string) string {
	return filepath.Join(ContainersDir(stateRoot), id)
}

// RootFSPath is where the assembled image layers live for a container.
func (r *Record) RootFSPath() string { return filepath.Join(r.Dir, "rootfs") }

// LogPath is the combined stdout/stderr log.
func (r *Record) LogPath() string { return filepath.Join(r.Dir, "container.log") }

// ConfigPath is the serialised record.
func (r *Record) ConfigPath() string { return filepath.Join(r.Dir, "config.json") }

// Create allocates a new container directory and writes its initial record.
func Create(stateRoot string, r *Record) error {
	if r.ID == "" {
		return fmt.Errorf("container has no id")
	}
	r.Dir = Dir(stateRoot, r.ID)
	// 0755 so an unprivileged `ps` and `logs` can read what a root `run` wrote.
	if err := os.MkdirAll(r.RootFSPath(), 0755); err != nil {
		return err
	}
	if r.Created == "" {
		r.Created = time.Now().UTC().Format(time.RFC3339)
	}
	if r.State == "" {
		r.State = StateCreated
	}
	return Save(r)
}

// Save writes the record atomically, so a reader never sees a partial file.
func Save(r *Record) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.Dir, "config.json.tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), r.ConfigPath())
}

// Load reads one container record by full id.
func Load(stateRoot, id string) (*Record, error) {
	dir := Dir(stateRoot, id)
	data, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no such container: %s", ShortID(id))
		}
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, fmt.Errorf("container %s: corrupt config: %w", ShortID(id), err)
	}
	r.Dir = dir
	return &r, nil
}

// List returns every container record, newest first.
func List(stateRoot string) ([]*Record, error) {
	entries, err := os.ReadDir(ContainersDir(stateRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Record
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		r, err := Load(stateRoot, e.Name())
		if err != nil {
			continue // half-created or corrupt; not worth failing the listing
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out, nil
}

// Resolve finds a container by full id, unique id prefix, or exact name.
func Resolve(stateRoot, ref string) (*Record, error) {
	if ref == "" {
		return nil, fmt.Errorf("no container specified")
	}
	all, err := List(stateRoot)
	if err != nil {
		return nil, err
	}

	var matches []*Record
	for _, r := range all {
		if r.Name == ref || r.ID == ref {
			return r, nil // exact match always wins over a prefix
		}
		if strings.HasPrefix(r.ID, ref) {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no such container: %s", ref)
	case 1:
		return matches[0], nil
	default:
		ids := make([]string, 0, len(matches))
		for _, m := range matches {
			ids = append(ids, ShortID(m.ID))
		}
		return nil, fmt.Errorf("ambiguous container %q, matches: %s", ref, strings.Join(ids, ", "))
	}
}

// Remove deletes a container's directory and everything in it.
func Remove(r *Record) error {
	if r.Dir == "" {
		return fmt.Errorf("container %s has no directory", ShortID(r.ID))
	}
	return os.RemoveAll(r.Dir)
}
