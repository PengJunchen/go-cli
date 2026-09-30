package approval

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const projectSourceManifestVersion = 1

var (
	// ErrProjectSourceUntrusted means no project source manifest is authorized.
	ErrProjectSourceUntrusted = errors.New("approval: project sources are not trusted")
	// ErrProjectSourceLegacy means the trust entry predates source manifests.
	ErrProjectSourceLegacy = errors.New("approval: legacy project trust has no source manifest")
	// ErrProjectSourceManifestMismatch means the captured project sources changed.
	ErrProjectSourceManifestMismatch = errors.New("approval: project source manifest changed")
	// ErrProjectSourceExpired means the stored project source trust has expired.
	ErrProjectSourceExpired = errors.New("approval: project source trust expired")
	// ErrProjectSourceSnapshotInvalid means the raw snapshot is invalid.
	ErrProjectSourceSnapshotInvalid = errors.New("approval: invalid project source snapshot")
	// ErrProjectSourceManagerUnavailable means the trust manager has no store.
	ErrProjectSourceManagerUnavailable = errors.New("approval: project source trust manager is unavailable")
	// ErrTransactionalTrustStoreUnavailable means the trust manager's backend
	// cannot provide the lock-scoped generation check required for dispatch.
	ErrTransactionalTrustStoreUnavailable = errors.New("approval: transactional trust store is unavailable")
	// ErrProjectSourceTrustStale means this capability no longer matches the
	// current persisted project trust generation.
	ErrProjectSourceTrustStale = errors.New("approval: project source trust generation is stale")
)

// ProjectSourceFileInput is one file's caller-captured source data. RelativePath
// is relative to the canonical workspace root, and Bytes are copied by the
// snapshot constructor.
type ProjectSourceFileInput struct {
	ID           string
	RelativePath string
	Present      bool
	Bytes        []byte
}

// ProjectSourceDirectoryInput describes a bounded directory scan result.
// Members contains root-relative paths and is copied and sorted by the
// snapshot constructor.
type ProjectSourceDirectoryInput struct {
	ID           string
	RelativePath string
	Present      bool
	Members      []string
}

// ProjectSourceRecord is a read-only view of one file in a source manifest.
type ProjectSourceRecord struct {
	ID           string `json:"id"`
	RelativePath string `json:"relative_path"`
	Present      bool   `json:"present"`
	Digest       string `json:"digest,omitempty"`
}

// ProjectSourceDirectoryRecord is a read-only view of one directory in a
// source manifest.
type ProjectSourceDirectoryRecord struct {
	ID           string   `json:"id"`
	RelativePath string   `json:"relative_path"`
	Present      bool     `json:"present"`
	Members      []string `json:"members,omitempty"`
}

type projectSourceFile struct {
	record ProjectSourceRecord
	bytes  []byte
}

// ProjectSourceSnapshot is an immutable, non-authoritative capture of project
// executable sources. Only a successful store comparison can turn it into a
// VerifiedProjectSources capability.
type ProjectSourceSnapshot struct {
	root            string
	manifestVersion int
	files           map[string]projectSourceFile
	fileRecords     []ProjectSourceRecord
	directories     []ProjectSourceDirectoryRecord
	manifestDigest  string
	valid           bool
}

// NewProjectSourceSnapshot builds a raw snapshot from source bytes already
// captured by the caller. It does not grant trust or create a verified
// capability.
func NewProjectSourceSnapshot(
	root string,
	fileInputs []ProjectSourceFileInput,
	directoryInputs []ProjectSourceDirectoryInput,
) (*ProjectSourceSnapshot, error) {
	canonicalRoot, err := canonicalProjectRoot(root)
	if err != nil {
		return nil, fmt.Errorf("approval: canonicalize project root: %w", err)
	}
	snapshot := &ProjectSourceSnapshot{
		root:            canonicalRoot,
		manifestVersion: projectSourceManifestVersion,
		files:           make(map[string]projectSourceFile, len(fileInputs)),
	}
	seenIDs := make(map[string]struct{}, len(fileInputs)+len(directoryInputs))
	for _, input := range fileInputs {
		id, err := validateManifestID(input.ID)
		if err != nil {
			return nil, fmt.Errorf("approval: file source identity: %w", err)
		}
		if _, exists := seenIDs[id]; exists {
			return nil, fmt.Errorf("approval: duplicate project source identity %q", id)
		}
		seenIDs[id] = struct{}{}
		relativePath, err := validateManifestPath(input.RelativePath, false)
		if err != nil {
			return nil, fmt.Errorf("approval: file source %q path: %w", id, err)
		}
		if !input.Present && len(input.Bytes) != 0 {
			return nil, fmt.Errorf("approval: absent file source %q contains bytes", id)
		}
		record := ProjectSourceRecord{ID: id, RelativePath: relativePath, Present: input.Present}
		var data []byte
		if input.Present {
			data = append([]byte(nil), input.Bytes...)
			digest := sha256.Sum256(data)
			record.Digest = hex.EncodeToString(digest[:])
		}
		snapshot.files[id] = projectSourceFile{record: record, bytes: data}
		snapshot.fileRecords = append(snapshot.fileRecords, record)
	}
	for _, input := range directoryInputs {
		id, err := validateManifestID(input.ID)
		if err != nil {
			return nil, fmt.Errorf("approval: directory source identity: %w", err)
		}
		if _, exists := seenIDs[id]; exists {
			return nil, fmt.Errorf("approval: duplicate project source identity %q", id)
		}
		seenIDs[id] = struct{}{}
		relativePath, err := validateManifestPath(input.RelativePath, true)
		if err != nil {
			return nil, fmt.Errorf("approval: directory source %q path: %w", id, err)
		}
		if !input.Present && len(input.Members) != 0 {
			return nil, fmt.Errorf("approval: absent directory source %q contains members", id)
		}
		members := make([]string, 0, len(input.Members))
		for _, member := range input.Members {
			cleanMember, err := validateManifestPath(member, false)
			if err != nil {
				return nil, fmt.Errorf("approval: directory source %q member: %w", id, err)
			}
			members = append(members, cleanMember)
		}
		sort.Strings(members)
		for i := 1; i < len(members); i++ {
			if members[i] == members[i-1] {
				return nil, fmt.Errorf("approval: duplicate member %q in directory source %q", members[i], id)
			}
		}
		snapshot.directories = append(snapshot.directories, ProjectSourceDirectoryRecord{
			ID:           id,
			RelativePath: relativePath,
			Present:      input.Present,
			Members:      members,
		})
	}
	sort.Slice(snapshot.fileRecords, func(i, j int) bool { return snapshot.fileRecords[i].ID < snapshot.fileRecords[j].ID })
	sort.Slice(snapshot.directories, func(i, j int) bool { return snapshot.directories[i].ID < snapshot.directories[j].ID })
	snapshot.manifestDigest = digestProjectSourceManifest(snapshot)
	snapshot.valid = true
	return snapshot, nil
}

// CanonicalRoot returns the resolved workspace root bound to the snapshot.
func (s *ProjectSourceSnapshot) CanonicalRoot() string {
	if s == nil || !s.valid {
		return ""
	}
	return s.root
}

// ManifestVersion returns the version of the encoded source manifest.
func (s *ProjectSourceSnapshot) ManifestVersion() int {
	if s == nil || !s.valid {
		return 0
	}
	return s.manifestVersion
}

// ManifestDigest returns the SHA-256 digest of all source and directory
// identity data captured in the snapshot.
func (s *ProjectSourceSnapshot) ManifestDigest() string {
	if s == nil || !s.valid {
		return ""
	}
	return s.manifestDigest
}

// SourceRecords returns a defensive copy of the sorted file records.
func (s *ProjectSourceSnapshot) SourceRecords() []ProjectSourceRecord {
	if s == nil || !s.valid {
		return nil
	}
	return append([]ProjectSourceRecord(nil), s.fileRecords...)
}

// DirectoryRecords returns a defensive copy of the sorted directory records.
func (s *ProjectSourceSnapshot) DirectoryRecords() []ProjectSourceDirectoryRecord {
	if s == nil || !s.valid {
		return nil
	}
	return cloneDirectoryRecords(s.directories)
}

// SourceBytes returns a defensive copy of the captured bytes for a present
// source. The bool reports whether the source ID exists and is present.
func (s *ProjectSourceSnapshot) SourceBytes(id string) ([]byte, bool) {
	if s == nil || !s.valid {
		return nil, false
	}
	file, ok := s.files[id]
	if !ok || !file.record.Present {
		return nil, false
	}
	return append([]byte(nil), file.bytes...), true
}

// DirectoryMembers returns a defensive copy of a directory's sorted members.
// The bool reports whether the directory ID exists; an existing but absent
// directory returns an empty or nil member list with ok=true.
func (s *ProjectSourceSnapshot) DirectoryMembers(id string) ([]string, bool) {
	if s == nil || !s.valid {
		return nil, false
	}
	for _, directory := range s.directories {
		if directory.ID == id {
			return append([]string(nil), directory.Members...), true
		}
	}
	return nil, false
}

// VerifiedProjectSources is an opaque capability minted only after a
// FrozenProjectSourceTrust store comparison succeeds.
type VerifiedProjectSources struct {
	snapshot        *ProjectSourceSnapshot
	trustGeneration uint64
	manager         *DefaultTrustManager
	seal            [32]byte
}

// Valid reports whether this value carries a manager-minted source proof.
func (v *VerifiedProjectSources) Valid() bool {
	if v == nil || v.snapshot == nil || !v.snapshot.valid || v.snapshot.manifestDigest == "" ||
		v.trustGeneration == 0 || v.manager == nil {
		return false
	}
	return v.seal == verifiedProjectSourceSeal(v.snapshot, v.trustGeneration)
}

// TrustGeneration returns the persisted project trust epoch captured by this
// verified source capability.
func (v *VerifiedProjectSources) TrustGeneration() uint64 {
	if !v.Valid() {
		return 0
	}
	return v.trustGeneration
}

// CommitIfCurrent holds the trust-store transaction lock while running the
// short in-memory dispatch commit callback. Revocation that wins the lock first
// rejects the callback; a later revocation observes an already-committed action.
func (v *VerifiedProjectSources) CommitIfCurrent(ctx context.Context, commit func() error) error {
	if ctx == nil {
		return errors.New("approval: project source commit context is nil")
	}
	if commit == nil {
		return errors.New("approval: project source commit callback is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !v.Valid() {
		return ErrProjectSourceSnapshotInvalid
	}
	return v.manager.commitProjectSourceCapability(ctx, v, commit)
}

// CanonicalRoot returns the workspace root bound to this verified capability.
func (v *VerifiedProjectSources) CanonicalRoot() string {
	if !v.Valid() {
		return ""
	}
	return v.snapshot.root
}

// ManifestDigest returns the verified source manifest digest.
func (v *VerifiedProjectSources) ManifestDigest() string {
	if !v.Valid() {
		return ""
	}
	return v.snapshot.manifestDigest
}

// ManifestVersion returns the verified source manifest version.
func (v *VerifiedProjectSources) ManifestVersion() int {
	if !v.Valid() {
		return 0
	}
	return v.snapshot.manifestVersion
}

// SourceRecords returns a defensive copy of the verified file records.
func (v *VerifiedProjectSources) SourceRecords() []ProjectSourceRecord {
	if !v.Valid() {
		return nil
	}
	return v.snapshot.SourceRecords()
}

// DirectoryRecords returns a defensive copy of the verified directory records.
func (v *VerifiedProjectSources) DirectoryRecords() []ProjectSourceDirectoryRecord {
	if !v.Valid() {
		return nil
	}
	return v.snapshot.DirectoryRecords()
}

// SourceBytes returns a defensive copy of one verified file's captured bytes.
func (v *VerifiedProjectSources) SourceBytes(id string) ([]byte, bool) {
	if !v.Valid() {
		return nil, false
	}
	return v.snapshot.SourceBytes(id)
}

// DirectoryMembers returns a defensive copy of one verified directory's
// captured sorted membership.
func (v *VerifiedProjectSources) DirectoryMembers(id string) ([]string, bool) {
	if !v.Valid() {
		return nil, false
	}
	return v.snapshot.DirectoryMembers(id)
}

// FrozenProjectSourceTrust is the optional TrustManager capability for
// persistent executable-source manifest comparison and grants.
type FrozenProjectSourceTrust interface {
	CheckProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) (*VerifiedProjectSources, error)
	TrustProjectSourceSnapshot(context.Context, *ProjectSourceSnapshot) error
}

var _ FrozenProjectSourceTrust = (*DefaultTrustManager)(nil)

// CheckProjectSourceSnapshot returns a capability only when the persisted
// manifest exactly matches the captured project sources and remains active.
func (m *DefaultTrustManager) CheckProjectSourceSnapshot(
	ctx context.Context,
	snapshot *ProjectSourceSnapshot,
) (*VerifiedProjectSources, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || m.store == nil {
		return nil, ErrProjectSourceManagerUnavailable
	}
	if snapshot == nil || !snapshot.valid || snapshot.manifestDigest == "" {
		return nil, ErrProjectSourceSnapshotInvalid
	}
	var verified *VerifiedProjectSources
	err := m.update(ctx, func(entries map[string]TrustEntry) (bool, error) {
		entry, ok := entries[snapshot.root]
		if !ok || entry.Revoked {
			return false, ErrProjectSourceUntrusted
		}
		if entry.ManifestVersion == 0 {
			return false, ErrProjectSourceLegacy
		}
		if entry.ManifestVersion != snapshot.manifestVersion || entry.CanonicalRoot != snapshot.root ||
			entry.Path != snapshot.root || entry.ManifestDigest != snapshot.manifestDigest ||
			!equalProjectSourceRecords(entry.ProjectSources, snapshot.fileRecords) ||
			!equalProjectSourceDirectoryRecords(entry.ProjectSourceDirectories, snapshot.directories) {
			return false, ErrProjectSourceManifestMismatch
		}
		if expired(entry, time.Now()) {
			return false, ErrProjectSourceExpired
		}
		generation := effectiveTrustGeneration(entry)
		if generation == 0 {
			return false, ErrProjectSourceTrustStale
		}
		frozen := cloneProjectSourceSnapshot(snapshot)
		verified = &VerifiedProjectSources{
			snapshot:        frozen,
			trustGeneration: generation,
			manager:         m,
			seal:            verifiedProjectSourceSeal(frozen, generation),
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("approval: check project source trust: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if verified == nil || !verified.Valid() {
		return nil, ErrProjectSourceTrustStale
	}
	return verified, nil
}

// TrustProjectSourceSnapshot persists the raw capture as a future manifest
// grant. It does not mint a capability for the current invocation.
func (m *DefaultTrustManager) TrustProjectSourceSnapshot(
	ctx context.Context,
	snapshot *ProjectSourceSnapshot,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.store == nil {
		return ErrProjectSourceManagerUnavailable
	}
	if snapshot == nil || !snapshot.valid || snapshot.manifestDigest == "" {
		return ErrProjectSourceSnapshotInvalid
	}
	return m.update(ctx, func(entries map[string]TrustEntry) (bool, error) {
		maxGeneration := uint64(0)
		for key, existing := range entries {
			if trustEntryIdentifiesRoot(key, existing, snapshot.root) {
				maxGeneration = maxTrustGeneration(maxGeneration, effectiveTrustGeneration(existing))
			}
		}
		if maxGeneration == ^uint64(0) {
			return false, errTrustGenerationOverflow
		}
		entries[snapshot.root] = TrustEntry{
			Path:                     snapshot.root,
			TrustedAt:                time.Now().UTC().Format(time.RFC3339),
			ManifestVersion:          snapshot.manifestVersion,
			CanonicalRoot:            snapshot.root,
			ProjectSources:           snapshot.SourceRecords(),
			ProjectSourceDirectories: snapshot.DirectoryRecords(),
			ManifestDigest:           snapshot.manifestDigest,
			TrustGeneration:          maxGeneration + 1,
		}
		return true, nil
	})
}

func (m *DefaultTrustManager) commitProjectSourceCapability(
	ctx context.Context,
	verified *VerifiedProjectSources,
	commit func() error,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if m == nil || m.store == nil || verified == nil || !verified.Valid() || verified.manager != m {
		return ErrProjectSourceManagerUnavailable
	}
	return m.update(ctx, func(entries map[string]TrustEntry) (bool, error) {
		entry, ok := entries[verified.snapshot.root]
		if !ok || entry.Revoked {
			return false, ErrProjectSourceTrustStale
		}
		generation := effectiveTrustGeneration(entry)
		if generation != verified.trustGeneration || entry.ManifestVersion != verified.snapshot.manifestVersion ||
			entry.CanonicalRoot != verified.snapshot.root || entry.Path != verified.snapshot.root ||
			entry.ManifestDigest != verified.snapshot.manifestDigest ||
			!equalProjectSourceRecords(entry.ProjectSources, verified.snapshot.fileRecords) ||
			!equalProjectSourceDirectoryRecords(entry.ProjectSourceDirectories, verified.snapshot.directories) {
			return false, ErrProjectSourceTrustStale
		}
		if expired(entry, time.Now()) {
			return false, ErrProjectSourceExpired
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return false, commit()
	})
}

func canonicalProjectRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("empty root")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("root is not a directory")
	}
	return filepath.Clean(canonical), nil
}

func validateManifestID(id string) (string, error) {
	if id == "" || strings.ContainsRune(id, '\x00') || strings.Contains(id, "\\") || filepath.IsAbs(id) {
		return "", fmt.Errorf("invalid ID %q", id)
	}
	clean := pathSlashClean(id)
	if clean != id || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("invalid ID %q", id)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." || part == "." || part == "" {
			return "", fmt.Errorf("invalid ID %q", id)
		}
	}
	return clean, nil
}

func validateManifestPath(path string, allowRoot bool) (string, error) {
	if strings.ContainsRune(path, '\x00') || strings.Contains(path, "\\") || filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q must be root-relative", path)
	}
	clean := pathSlashClean(path)
	if clean == "." && allowRoot {
		return clean, nil
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q escapes the project root", path)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." || part == "." || part == "" {
			return "", fmt.Errorf("path %q escapes the project root", path)
		}
	}
	return clean, nil
}

func pathSlashClean(value string) string {
	return filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
}

func digestProjectSourceManifest(snapshot *ProjectSourceSnapshot) string {
	h := sha256.New()
	writeManifestString(h, "go-cli-project-source-manifest-v1")
	writeManifestUint64(h, uint64(snapshot.manifestVersion))
	writeManifestString(h, snapshot.root)
	writeManifestUint64(h, uint64(len(snapshot.fileRecords)))
	for _, record := range snapshot.fileRecords {
		writeManifestString(h, record.ID)
		writeManifestString(h, record.RelativePath)
		writeManifestBool(h, record.Present)
		if record.Present {
			file := snapshot.files[record.ID]
			writeManifestUint64(h, uint64(len(file.bytes)))
			_, _ = h.Write(file.bytes)
		}
	}
	writeManifestUint64(h, uint64(len(snapshot.directories)))
	for _, record := range snapshot.directories {
		writeManifestString(h, record.ID)
		writeManifestString(h, record.RelativePath)
		writeManifestBool(h, record.Present)
		writeManifestUint64(h, uint64(len(record.Members)))
		for _, member := range record.Members {
			writeManifestString(h, member)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeManifestString(h hash.Hash, value string) {
	writeManifestUint64(h, uint64(len(value)))
	_, _ = h.Write([]byte(value))
}

func writeManifestUint64(h hash.Hash, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = h.Write(encoded[:])
}

func writeManifestBool(h hash.Hash, value bool) {
	if value {
		_, _ = h.Write([]byte{1})
		return
	}
	_, _ = h.Write([]byte{0})
}

func verifiedProjectSourceSeal(snapshot *ProjectSourceSnapshot, generation uint64) [32]byte {
	if snapshot == nil {
		return [32]byte{}
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], generation)
	material := append([]byte("go-cli-verified-project-source-v2\x00"+snapshot.root+"\x00"+snapshot.manifestDigest+"\x00"), encoded[:]...)
	return sha256.Sum256(material)
}

func cloneProjectSourceSnapshot(snapshot *ProjectSourceSnapshot) *ProjectSourceSnapshot {
	if snapshot == nil || !snapshot.valid {
		return nil
	}
	clone := &ProjectSourceSnapshot{
		root:            snapshot.root,
		manifestVersion: snapshot.manifestVersion,
		files:           make(map[string]projectSourceFile, len(snapshot.files)),
		fileRecords:     append([]ProjectSourceRecord(nil), snapshot.fileRecords...),
		directories:     cloneDirectoryRecords(snapshot.directories),
		manifestDigest:  snapshot.manifestDigest,
		valid:           true,
	}
	for id, file := range snapshot.files {
		file.bytes = append([]byte(nil), file.bytes...)
		clone.files[id] = file
	}
	return clone
}

func cloneDirectoryRecords(records []ProjectSourceDirectoryRecord) []ProjectSourceDirectoryRecord {
	if records == nil {
		return nil
	}
	clone := make([]ProjectSourceDirectoryRecord, len(records))
	for i, record := range records {
		clone[i] = record
		clone[i].Members = append([]string(nil), record.Members...)
	}
	return clone
}

func equalProjectSourceRecords(left, right []ProjectSourceRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func equalProjectSourceDirectoryRecords(left, right []ProjectSourceDirectoryRecord) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].ID != right[i].ID || left[i].RelativePath != right[i].RelativePath || left[i].Present != right[i].Present ||
			len(left[i].Members) != len(right[i].Members) {
			return false
		}
		for j := range left[i].Members {
			if left[i].Members[j] != right[i].Members[j] {
				return false
			}
		}
	}
	return true
}
