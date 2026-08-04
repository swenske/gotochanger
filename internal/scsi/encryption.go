package scsi

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Security protocol / page constants for SECURITY PROTOCOL IN/OUT
// (Milestone 10, real tape encryption) - verified against `stenc`
// (github.com/scsitape/stenc, an open-source SCSI Tape Encryption
// Manager), the same "real, working open-source implementation" citation
// tier this package already leans on for VPD pages (sg3_utils) and MAM
// attributes (sg3_utils' sg_read_attr/sg_write_attr) - specifically its
// src/scsiencrypt.h/.cpp, which build and parse these exact CDBs/pages
// against real LTO hardware.
const (
	securityProtocolTDE = 0x20 // "Tape Data Encryption" - the only security protocol this package implements

	secPageSupported              = 0x00
	secPageSetDataEncryption      = 0x10 // SPOUT only
	secPageDeviceEncryptionStatus = 0x20 // SPIN only

	// SDE (Set Data Encryption)/DES (Device Encryption Status) encryption/
	// decryption mode values - stenc's own encrypt_mode/decrypt_mode enums
	// define off=0/external=1/on=2 and off=0/raw=1/on=2/mixed=3
	// respectively; this package only implements off/on for both (the two
	// values meaningful for "encrypt everything on this volume" vs "don't"
	// - external/raw/mixed all describe finer real-hardware behaviors this
	// project's own simplified, whole-volume-at-a-time encryption model
	// has no analogue for, see library.Volume.Encrypted's own doc
	// comment), rejecting any other value rather than guessing at it.
	sdeModeOff = 0x00
	sdeModeOn  = 0x02

	// aesKeyLen is the only key length this package accepts - AES-256,
	// matching the plan's own choice (stdlib crypto/aes, no external
	// dependency, per this project's minimal-dependency philosophy - see
	// internal/api/password_hash.go's own hand-rolled-PBKDF2 precedent).
	aesKeyLen = 32

	// sdeKeyFormatPlaintext is the only WRITE ATTRIBUTE... no, SECURITY
	// PROTOCOL OUT key format this package accepts: the key bytes
	// themselves, not a wrapped/vendor-specific format (T10 defines
	// other key_format values for wrapped keys under an external key
	// manager - a real capability this project has no analogue for and
	// must not silently mishandle by treating wrapped key bytes as if
	// they were the raw AES key).
	sdeKeyFormatPlaintext = 0x00
)

// buildSupportedSecurityPages builds the security protocol 0x20 "list of
// supported pages" response: a 4-byte header (available data length)
// followed by the list of 16-bit big-endian page codes this package
// implements within this protocol - same header convention this package
// already uses for VPD/log/mode page 0x00 listings elsewhere, adapted to
// this protocol's 16-bit (not 8-bit) page code width. This exact overall
// response shape for this specific page is a best-effort, self-consistent
// construction, not independently verified against a primary source the
// way the CDB/SDE/DES page layouts above are - low-stakes, since a real
// initiator that already knows it wants TDE (like stenc, or any real key
// manager) queries the pages it needs directly rather than this listing.
func buildSupportedSecurityPages() []byte {
	body := make([]byte, 4)
	binary.BigEndian.PutUint16(body[0:2], secPageSetDataEncryption)
	binary.BigEndian.PutUint16(body[2:4], secPageDeviceEncryptionStatus)
	header := make([]byte, 4)
	binary.BigEndian.PutUint16(header[2:4], uint16(len(body)))
	return append(header, body...)
}

// buildDeviceEncryptionStatusPage builds the Device Encryption Status
// page (0x20, SPIN-only): a 24-byte header (page code, page length,
// scope, encryption mode, decryption mode, algorithm index, key instance
// counter, flags, KAD format, ASDK count, 8 reserved bytes) with no KAD
// (Key-Associated Data) entries - this package has no external key
// manager/KAD concept to report, matching library.Volume.Encrypted's own
// documented "whole volume, one key, one session" simplification. Byte
// layout verified against stenc's page_des struct (see this file's own
// doc comment) - the scope/flags/KAD-format bit fields that struct
// defines are left entirely zero (this package's own encryption state is
// always I_T-nexus-scoped to this one session, which is scope value 0
// anyway, and there is genuinely nothing to report in flags/KAD-format
// with no KAD entries).
func (d *Drive) buildDeviceEncryptionStatusPage() []byte {
	p := make([]byte, 24)
	binary.BigEndian.PutUint16(p[0:2], secPageDeviceEncryptionStatus)
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)-4))
	p[5] = d.encryptMode
	p[6] = d.decryptMode
	p[7] = d.algorithmIndex
	binary.BigEndian.PutUint32(p[8:12], d.keyInstanceCounter)
	return p
}

// encryptionActive reports whether this drive session currently has a
// real, usable AES-256 key set via SECURITY PROTOCOL OUT - write6/read6
// consult this (not d.encryptMode alone) before touching encryption at
// all, so an incompletely-negotiated session (mode set to "on" but key
// somehow absent) never silently falls back to writing plaintext.
func (d *Drive) encryptionActive() bool {
	return d.encryptMode == sdeModeOn && len(d.encryptionKey) == aesKeyLen
}

// encryptionNonce derives this project's own deterministic AES-GCM nonce
// for a chunk starting at byte offset pos: 4 zero bytes followed by pos
// as an 8-byte big-endian integer, filling GCM's required 12-byte nonce
// size exactly. Safe specifically because of this project's own write
// model, not as a generally-reusable technique: GCM's security guarantee
// requires a (key, nonce) pair never be reused for two *different*
// plaintexts, and this project's write6 always calls truncateToEOD (and,
// for encrypted volumes, appendEncTag's own equivalent invalidation)
// before any new data lands at a given position - the old ciphertext at
// that position is destroyed before the position is ever reused, so the
// same nonce value is never asked to authenticate two still-existing,
// different ciphertexts under the same key. Re-keying mid-volume (a new
// SECURITY PROTOCOL OUT call without a preceding rewind-and-overwrite) is
// still safe even though it reuses the same nonce *values* under a
// *different* key, since GCM's uniqueness requirement is scoped to the
// (key, nonce) pair, not the nonce alone.
func encryptionNonce(pos int64) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint64(b[4:], uint64(pos))
	return b
}

// sealChunk AES-256-GCM-encrypts plaintext as one atomic chunk starting
// at byte offset pos, returning the ciphertext (exactly len(plaintext)
// bytes - GCM's ciphertext is always the same length as its plaintext;
// only the separate tag carries the +16-byte AEAD overhead) and that
// chunk's own authentication tag, kept apart deliberately: writing
// ciphertext at the same offset/length the plaintext would have occupied
// is what lets this project's existing flat-backing-file model (position
// addressing, truncateToEOD, capacity/EOD checks, filemarks - all written
// long before this milestone, all assuming "file size == recorded data
// length") keep working completely unchanged; the tag is instead recorded
// in a parallel sidecar file (see encTag/readEncTags/appendEncTag below),
// the same "keep it out of the backing byte stream" convention
// filemarks.go already established for filemark positions.
func (d *Drive) sealChunk(pos int64, plaintext []byte) (ciphertext, tag []byte, err error) {
	block, err := aes.NewCipher(d.encryptionKey)
	if err != nil {
		return nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	sealed := gcm.Seal(nil, encryptionNonce(pos), plaintext, nil)
	tagLen := gcm.Overhead()
	return sealed[:len(sealed)-tagLen], sealed[len(sealed)-tagLen:], nil
}

// openChunk is sealChunk's inverse: authenticates and decrypts one chunk
// given its own starting offset, ciphertext, and tag, using d's
// *currently* set key - deliberately always the current session key,
// never one recorded per-chunk: a real drive can't decrypt data written
// under a different key either, so a chunk written under an old key
// failing to authenticate against the current one is correct behavior,
// not a bug (see write6/read6's own doc comments). ok=false covers both
// "no key currently set" (aes.NewCipher on a nil/wrong-length key
// fails) and "wrong key" (GCM authentication failure) identically - both
// correctly surface as the same security-class CHECK CONDITION to the
// initiator either way (see read6's own doc comment).
func (d *Drive) openChunk(pos int64, ciphertext, tag []byte) (plaintext []byte, ok bool) {
	block, err := aes.NewCipher(d.encryptionKey)
	if err != nil {
		return nil, false
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, false
	}
	sealed := append(append([]byte(nil), ciphertext...), tag...)
	plaintext, err = gcm.Open(nil, encryptionNonce(pos), sealed, nil)
	if err != nil {
		return nil, false
	}
	return plaintext, true
}

// decryptRange reconstructs the plaintext for [start, start+length) of
// an encrypted volume by finding every recorded chunk (see encTag) that
// overlaps the requested range, authenticating and decrypting each in
// full (GCM authenticates a chunk as one atomic unit - a sub-range of a
// chunk cannot be verified without the rest of it), and slicing out just
// the requested portion of each. Returns as much plaintext as actually
// exists if the recorded (chunked) data simply runs out before the
// requested length - the direct encrypted-volume analogue of a real
// short ReadAt at end-of-file, left for the caller (read6) to turn into
// the same BLANK CHECK/ILI handling the unencrypted path already has.
// cryptErr=true is a categorically different outcome - an existing chunk
// failed to authenticate (wrong or missing key) - which read6 must
// report as a distinct, security-class failure instead.
func (d *Drive) decryptRange(volPath string, start, length int64) (out []byte, cryptErr bool) {
	if length <= 0 {
		return nil, false
	}
	tags, err := readEncTags(volPath)
	if err != nil {
		return nil, false
	}
	sort.Slice(tags, func(i, j int) bool { return tags[i].start < tags[j].start })

	f, err := os.Open(volPath)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	end := start + length
	pos := start
	for _, t := range tags {
		if pos >= end {
			break
		}
		chunkEnd := t.start + t.length
		if chunkEnd <= pos {
			continue // this chunk ends before the range we still need - skip it
		}
		if t.start > pos {
			break // gap: nothing covers [pos, t.start) - treat as end of recorded data
		}
		ciphertext := make([]byte, t.length)
		if _, err := f.ReadAt(ciphertext, t.start); err != nil {
			break
		}
		plaintext, ok := d.openChunk(t.start, ciphertext, t.tag)
		if !ok {
			return out, true
		}
		from := pos - t.start
		to := int64(len(plaintext))
		if chunkEnd > end {
			to = end - t.start
		}
		out = append(out, plaintext[from:to]...)
		pos = t.start + to
	}
	return out, false
}

// encTag is one recorded encryption chunk: the plaintext byte range
// [start, start+length) this chunk covers in the backing file, and the
// AES-GCM authentication tag sealChunk produced for it (see decryptRange/
// openChunk for how it's used on read).
type encTag struct {
	start  int64
	length int64
	tag    []byte
}

// encTagsPath returns the sidecar file this project records a volume's
// encryption chunk tags in - same "keep it out of the real recorded byte
// stream" convention as filemarksPath.
func encTagsPath(volPath string) string {
	return volPath + ".enctags"
}

// readEncTags returns every recorded chunk for volPath, one per line as
// "<start>:<length>:<base64 tag>" - no sidecar file at all is not an
// error, same convention as readFilemarks (a volume that was never
// encrypted simply has none).
func readEncTags(volPath string) ([]encTag, error) {
	data, err := os.ReadFile(encTagsPath(volPath))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []encTag
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue // a corrupt line is skipped, not fatal to the whole read - same posture as readFilemarks
		}
		start, err1 := strconv.ParseInt(parts[0], 10, 64)
		length, err2 := strconv.ParseInt(parts[1], 10, 64)
		tag, err3 := base64.StdEncoding.DecodeString(parts[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		out = append(out, encTag{start: start, length: length, tag: tag})
	}
	return out, nil
}

func writeEncTagsFile(volPath string, tags []encTag) error {
	var b strings.Builder
	for _, t := range tags {
		b.WriteString(strconv.FormatInt(t.start, 10))
		b.WriteByte(':')
		b.WriteString(strconv.FormatInt(t.length, 10))
		b.WriteByte(':')
		b.WriteString(base64.StdEncoding.EncodeToString(t.tag))
		b.WriteByte('\n')
	}
	return os.WriteFile(encTagsPath(volPath), []byte(b.String()), 0o644)
}

// dropEncTagsFrom removes every recorded chunk whose own start is >= pos
// - called before a fresh write or an ERASE lands new/no data at pos,
// invalidating any old chunk a real magnetic medium could no longer
// possibly still hold intact once a real, physical WRITE or ERASE
// happens at that position (same "real tape offers no way to record at
// one position while keeping data further along the medium" reasoning
// truncateToEOD/invalidateFilemarksFrom already document).
func dropEncTagsFrom(volPath string, pos int64) error {
	existing, err := readEncTags(volPath)
	if err != nil {
		return err
	}
	kept := existing[:0]
	for _, t := range existing {
		if t.start < pos {
			kept = append(kept, t)
		}
	}
	if len(kept) == len(existing) {
		return nil
	}
	return writeEncTagsFile(volPath, kept)
}

// appendEncTag records a freshly-written chunk, first dropping any
// existing chunk at or beyond its own start (see dropEncTagsFrom) -
// mirrors recordFilemark's exact "a fresh write invalidates everything
// from this point forward" shape.
func appendEncTag(volPath string, t encTag) error {
	if err := dropEncTagsFrom(volPath, t.start); err != nil {
		return err
	}
	existing, err := readEncTags(volPath)
	if err != nil {
		return err
	}
	return writeEncTagsFile(volPath, append(existing, t))
}
