// Copyright (c) 2019-2020 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bpf

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type MapIter func(k, v []byte)

type Map interface {
	GetName() string
	// EnsureExists opens the map, creating and pinning it if needed.
	EnsureExists() error
	// MapFD gets the file descriptor of the map, only valid after calling EnsureExists().
	MapFD() MapFD
	// Path returns the path that the map is (to be) pinned to.
	Path() string

	Iter(MapIter) error
	Update(k, v []byte) error
	Get(k []byte) ([]byte, error)
	Delete(k []byte) error
}

type MapParameters struct {
	Filename   string
	Type       string
	KeySize    int
	ValueSize  int
	MaxEntries int
	Name       string
	Flags      int
	Version    int
}

func versionedStr(ver int, str string) string {
	if ver <= 1 {
		return str
	}

	return fmt.Sprintf("%s%d", str, ver)
}

func (mp *MapParameters) versionedName() string {
	return versionedStr(mp.Version, mp.Name)
}

func (mp *MapParameters) versionedFilename() string {
	return versionedStr(mp.Version, mp.Filename)
}

type MapContext struct {
	RepinningEnabled bool
}

func (c *MapContext) NewPinnedMap(params MapParameters) Map {
	if len(params.versionedName()) >= unix.BPF_OBJ_NAME_LEN {
		logrus.WithField("name", params.Name).Panic("Bug: BPF map name too long")
	}
	m := &PinnedMap{
		context:       c,
		MapParameters: params,
		perCPU:        strings.Contains(params.Type, "percpu"),
	}
	return m
}

type PinnedMap struct {
	context *MapContext
	MapParameters

	fdLoaded bool
	fd       MapFD
	perCPU   bool
}

func (b *PinnedMap) GetName() string {
	return b.versionedName()
}

func (b *PinnedMap) MapFD() MapFD {
	if !b.fdLoaded {
		logrus.Panic("MapFD() called without first calling EnsureExists()")
	}
	return b.fd
}

func (b *PinnedMap) Path() string {
	return b.versionedFilename()
}

func (b *PinnedMap) Close() error {
	err := b.fd.Close()
	b.fdLoaded = false
	b.fd = 0
	return err
}

func (b *PinnedMap) RepinningEnabled() bool {
	if b.context == nil {
		return false
	}
	return b.context.RepinningEnabled
}

// DumpMapCmd returns the command that can be used to dump a map or an error
func DumpMapCmd(m Map) ([]string, error) {
	if pm, ok := m.(*PinnedMap); ok {
		return []string{
			"bpftool",
			"--json",
			"--pretty",
			"map",
			"dump",
			"pinned",
			pm.versionedFilename(),
		}, nil
	}

	return nil, errors.Errorf("unrecognized map type %T", m)
}

// IterMapCmdOutput iterates over the outout of a command obtained by DumpMapCmd
func IterMapCmdOutput(output []byte, f MapIter) error {
	var mp []mapEntry
	err := json.Unmarshal(output, &mp)
	if err != nil {
		return errors.Errorf("cannot parse json output: %v\n%s", err, output)
	}

	for _, me := range mp {
		k, err := hexStringsToBytes(me.Key)
		if err != nil {
			return errors.Errorf("failed parsing entry %s key: %e", me, err)
		}
		v, err := hexStringsToBytes(me.Value)
		if err != nil {
			return errors.Errorf("failed parsing entry %s val: %e", me, err)
		}
		f(k, v)
	}

	return nil
}

func (b *PinnedMap) Iter(f MapIter) error {
	cmd, err := DumpMapCmd(b)
	if err != nil {
		return err
	}

	prog := cmd[0]
	args := cmd[1:]

	printCommand(prog, args...)
	output, err := exec.Command(prog, args...).Output()
	if err != nil {
		return errors.Errorf("failed to dump in map (%s): %s\n%s", b.versionedFilename(), err, output)
	}

	if err := IterMapCmdOutput(output, f); err != nil {
		return errors.WithMessagef(err, "map %s", b.versionedFilename())
	}

	return nil
}

func (b *PinnedMap) Update(k, v []byte) error {
	if b.perCPU {
		// Per-CPU maps need a buffer of value-size * num-CPUs.
		logrus.Panic("Per-CPU operations not implemented")
	}
	return UpdateMapEntry(b.fd, k, v)
}

func (b *PinnedMap) Get(k []byte) ([]byte, error) {
	if b.perCPU {
		// Per-CPU maps need a buffer of value-size * num-CPUs.
		logrus.Panic("Per-CPU operations not implemented")
	}
	return GetMapEntry(b.fd, k, b.ValueSize)
}

func appendBytes(strings []string, bytes []byte) []string {
	for _, b := range bytes {
		strings = append(strings, strconv.FormatInt(int64(b), 10))
	}
	return strings
}

func (b *PinnedMap) Delete(k []byte) error {
	logrus.WithField("key", k).Debug("Deleting map entry")
	args := make([]string, 0, 10+len(k))
	args = append(args, "map", "delete",
		"pinned", b.versionedFilename(),
		"key")
	args = appendBytes(args, k)

	cmd := exec.Command("bpftool", args...)
	out, err := cmd.Output()
	if err != nil {
		if err, ok := err.(*exec.ExitError); ok {
			if strings.Contains(string(err.Stderr), "delete failed: No such file or directory") {
				logrus.WithField("k", k).Debug("Item didn't exist.")
				return os.ErrNotExist
			}
		}
		logrus.WithField("out", string(out)).Error("Failed to run bpftool")
	}
	return err
}

func (b *PinnedMap) EnsureExists() error {
	if b.fdLoaded {
		return nil
	}

	_, err := MaybeMountBPFfs()
	if err != nil {
		logrus.WithError(err).Error("Failed to mount bpffs")
		return err
	}
	// FIXME hard-coded dir
	err = os.MkdirAll("/sys/fs/bpf/tc/globals", 0700)
	if err != nil {
		logrus.WithError(err).Error("Failed create dir")
		return err
	}

	_, err = os.Stat(b.versionedFilename())
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		logrus.Debug("Map file didn't exist")
		if b.context.RepinningEnabled {
			logrus.WithField("name", b.Name).Info("Looking for map by name (to repin it)")
			err = RepinMap(b.versionedName(), b.versionedFilename())
			if err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}

	if err == nil {
		logrus.Debug("Map file already exists, trying to open it")
		b.fd, err = GetMapFDByPin(b.versionedFilename())
		if err == nil {
			b.fdLoaded = true
			logrus.WithField("fd", b.fd).WithField("name", b.versionedFilename()).
				Info("Loaded map file descriptor.")
		}
		return err
	}

	logrus.Debug("Map didn't exist, creating it")
	cmd := exec.Command("bpftool", "map", "create", b.versionedFilename(),
		"type", b.Type,
		"key", fmt.Sprint(b.KeySize),
		"value", fmt.Sprint(b.ValueSize),
		"entries", fmt.Sprint(b.MaxEntries),
		"name", b.versionedName(),
		"flags", fmt.Sprint(b.Flags),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		logrus.WithField("out", string(out)).Error("Failed to run bpftool")
		return err
	}
	b.fd, err = GetMapFDByPin(b.versionedFilename())
	if err == nil {
		b.fdLoaded = true
		logrus.WithField("fd", b.fd).WithField("name", b.versionedFilename()).
			Info("Loaded map file descriptor.")
	}
	return err
}

type bpftoolMapMeta struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func RepinMap(name string, filename string) error {
	cmd := exec.Command("bpftool", "map", "list", "-j")
	out, err := cmd.Output()
	if err != nil {
		return errors.Wrap(err, "bpftool map list failed")
	}
	logrus.WithField("maps", string(out)).Debug("Got map metadata.")

	var maps []bpftoolMapMeta
	err = json.Unmarshal(out, &maps)
	if err != nil {
		return errors.Wrap(err, "bpftool returned bad JSON")
	}

	for _, m := range maps {
		if m.Name == name {
			// Found the map, try to repin it.
			cmd := exec.Command("bpftool", "map", "pin", "id", fmt.Sprint(m.ID), filename)
			return errors.Wrap(cmd.Run(), "bpftool failed to repin map")
		}
	}

	return os.ErrNotExist
}
