// Package selector makes the labels jobs run on inside the organization, in
// the grammar of lanescape/runners' specs/0005-interface.md (Selectors),
// and the keys of saved work disks (specs/0001-actions.md, runners).
package selector

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	// Shape is a runner's name: lanescape-<vCPUs>vcpu-linux-<amd64|arm64>.
	Shape = regexp.MustCompile(`^lanescape-[1-9][0-9]*vcpu-linux-(amd64|arm64)$`)
	// Family is a disk's family: 1 to 32 lowercase letters, digits and
	// hyphens, starting with a letter.
	Family = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	// Key is a disk's key: exactly 20 lowercase hex digits.
	Key = regexp.MustCompile(`^[0-9a-f]{20}$`)
	// Scope is a binding's scope.
	Scope = regexp.MustCompile(`^(main|read|pr[1-9][0-9]*)$`)
)

// NoKey is a family's one key when its disk entry gives neither from nor
// key.
const NoKey = "00000000000000000000"

// Runner is a runner's selector: <shape>-<scope>.
func Runner(shape, scope string) string {
	return strings.ToLower(shape + "-" + scope)
}

// Disk is a disk entry's selector:
// <shape>[-spot]-disk-<family>-<key>-<scope>.
func Disk(shape string, spot bool, family, key, scope string) string {
	var b strings.Builder
	b.WriteString(shape)
	if spot {
		b.WriteString("-spot")
	}
	b.WriteString("-disk-" + family + "-" + key + "-" + scope)
	return strings.ToLower(b.String())
}

// File is a listed file and its contents at the commit the job builds.
type File struct {
	Path     string
	Contents []byte
}

// DiskKey is the key of a disk whose contents depend on files: the first 20
// hex digits of the SHA-256 of the family, the runner, then each path and
// its contents in the order listed, each written as its length in bytes,
// an unsigned 64-bit big-endian number, followed by its bytes.
func DiskKey(family, runner string, files []File) string {
	h := sha256.New()
	part := func(b []byte) {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(b)))
		h.Write(n[:])
		h.Write(b)
	}
	part([]byte(family))
	part([]byte(runner))
	for _, f := range files {
		part([]byte(f.Path))
		part(f.Contents)
	}
	return hex.EncodeToString(h.Sum(nil))[:20]
}
