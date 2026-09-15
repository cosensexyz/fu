package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cosensexyz/fu/internal/store"
)

func archiveRecordFixture(identity store.FileIdentity) adoptLinkArchiveRecord {
	return newAdoptLinkArchiveRecord(
		adoptLinkArchiveEntry, "claude", "alpha",
		"/home/someone/.claude/skills/alpha", "../../store/skills/alpha",
		uint32(0o777|(1<<27)), identity, // symlink mode bits
	)
}

// A new archive keeps the strongest identity the platform gave it.
//
// The handle was stripped so the bytes stayed reproducible by binaries that
// predate handles. The cost was that every archive fell back to device+inode,
// which a deleted file's replacement can reuse -- the failure batch 4 and the
// Linux CI tripwire were both about.
func TestNewArchivesKeepTheHandle(t *testing.T) {
	identity := store.FileIdentity{Device: 7, Inode: 11, Handle: "1:abcd"}
	raw, name, err := marshalAdoptLinkArchive(archiveRecordFixture(identity))
	if err != nil {
		t.Fatal(err)
	}
	var written adoptLinkArchiveRecord
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatal(err)
	}
	if written.Version != adoptLinkArchiveVersion {
		t.Fatalf("version = %d, want %d", written.Version, adoptLinkArchiveVersion)
	}
	if written.Identity.Handle != "1:abcd" {
		t.Fatalf("identity = %+v, want the handle kept", written.Identity)
	}
	if !validAdoptLinkArchiveName(name) {
		t.Fatalf("name %q is not a valid archive name", name)
	}
}

// An archive written before handles must still be reproducible byte for byte,
// from a record whose identity now carries one.
//
// This is what makes an interrupted adopt resumable across the format change.
// The record handed to validation is rebuilt from the journal, and since batch
// 4 that journal identity has a handle -- so marshalling it at the current
// version would give a different digest, a different name, and a conflict on
// an archive that is perfectly intact.
func TestAnArchiveWrittenBeforeHandlesStillReproduces(t *testing.T) {
	legacy, legacyName, err := marshalAdoptLinkArchiveAt(
		archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11}), adoptLinkArchiveVersionLegacy)
	if err != nil {
		t.Fatal(err)
	}
	// The same object, seen today: same device and inode, and now a handle.
	rebuilt := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11, Handle: "1:abcd"})
	again, againName, err := marshalAdoptLinkArchiveAt(rebuilt, adoptLinkArchiveVersionLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if againName != legacyName || string(again) != string(legacy) {
		t.Fatalf("a v1 archive must reproduce from today's record:\n got %s %s\nwant %s %s",
			againName, again, legacyName, legacy)
	}
	// And the current version genuinely differs, or the test above proves nothing.
	if _, currentName, err := marshalAdoptLinkArchive(rebuilt); err != nil {
		t.Fatal(err)
	} else if currentName == legacyName {
		t.Fatal("v1 and v2 produce the same name; the version is not reaching the digest")
	}
}

// What a record proves must be a typed answer, not a boolean from Same.
//
// FileIdentity.Same degrades to device+inode whenever either side lacks a
// handle, so a handle-less record comparing "equal" is not proof of anything --
// and nothing in the type says so. The distinction that matters is between a v1
// record, which never recorded a handle whatever the platform could do, and a
// v2 record whose platform could not supply one.
func TestArchiveEvidenceDoesNotUpgradeOldRecords(t *testing.T) {
	for _, tc := range []struct {
		name    string
		version int
		handle  string
		want    adoptLinkEvidence
	}{
		{"v1 says nothing about handles", adoptLinkArchiveVersionLegacy, "", adoptLinkEvidenceUnknown},
		{"v2 without a handle names the platform's limit", adoptLinkArchiveVersion, "", adoptLinkEvidenceDeviceInode},
		{"v2 with a handle is the strong case", adoptLinkArchiveVersion, "1:abcd", adoptLinkEvidenceHandle},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11, Handle: tc.handle})
			record.Version = tc.version
			if got := record.evidence(); got != tc.want {
				t.Fatalf("evidence = %v, want %v", got, tc.want)
			}
		})
	}
	// The zero value must be the weakest answer, not a confident one: a
	// strength read out of an unset field is exactly how batch 8 twice turned
	// "nothing was decided" into a verdict.
	var unset adoptLinkEvidence
	if unset != adoptLinkEvidenceUnknown {
		t.Fatal("the zero evidence value must mean unknown")
	}
}

// fu never wrote a v1 record carrying a handle, so meeting one means the file
// was edited. It is refused rather than read as a stronger record than any v1
// can be.
func TestAV1RecordCarryingAHandleIsRefused(t *testing.T) {
	record := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11, Handle: "1:abcd"})
	record.Version = adoptLinkArchiveVersionLegacy
	if err := record.validate(); err == nil {
		t.Fatal("a v1 record with a handle must be refused")
	} else if !strings.Contains(err.Error(), "handle") {
		t.Fatalf("the refusal must say what is wrong: %v", err)
	}
}

// A version from a newer build is refused, not guessed at.
//
// This is *this* build meeting a future version 3 -- not an older fu meeting
// one of today's archives, which is a different thing and was described wrongly
// here at first. An older fu never decodes the archive file at all; it compares
// the bytes it would have written and reports a name/content mismatch, the same
// conflict it reports for a tampered archive. DESIGN carries that correction.
func TestAnUnknownArchiveVersionIsRefused(t *testing.T) {
	record := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11})
	record.Version = adoptLinkArchiveVersion + 1
	if err := record.validate(); err == nil {
		t.Fatal("a version this build does not understand must be refused")
	}
}

// The resume path, end to end: an archive on disk from before handles, and a
// record rebuilt today whose identity carries one. Validation must recognise
// the file for what it is rather than demanding it look like something this
// build would write.
//
// Without this, upgrading fu would strand every interrupted adopt: the archive
// is intact, the store is fine, and the only thing wrong is that a newer binary
// encodes the same facts differently.
func TestAnOldArchiveOnDiskStillValidatesAfterTheFormatChange(t *testing.T) {
	s, _ := setupStore(t)
	// What an older fu left behind.
	legacyRaw, legacyName, err := marshalAdoptLinkArchiveAt(
		archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11}), adoptLinkArchiveVersionLegacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeTxnFileNoReplace(s, legacyName, legacyRaw); err != nil {
		t.Fatal(err)
	}

	// What this build rebuilds from the journal: the same object, now with a
	// handle, because batch 4 made identity capture record one.
	rebuilt := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11, Handle: "1:abcd"})
	if err := validateAdoptLinkArchive(s, legacyName, rebuilt); err != nil {
		t.Fatalf("an intact archive from an older build must still validate: %v", err)
	}

	// And a genuinely different object must still be refused, or the above is
	// just a validation that gave up.
	other := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 12, Handle: "1:abcd"})
	if err := validateAdoptLinkArchive(s, legacyName, other); err == nil {
		t.Fatal("an archive describing a different object must be refused")
	}
}

// A current archive round-trips through the same path, handle included.
func TestACurrentArchiveValidatesWithItsHandle(t *testing.T) {
	s, _ := setupStore(t)
	record := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11, Handle: "1:abcd"})
	name, err := ensureAdoptLinkArchive(s, record)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateAdoptLinkArchive(s, name, record); err != nil {
		t.Fatalf("a freshly written archive must validate: %v", err)
	}
	// Two records agreeing on device and inode but not on the handle are now
	// distinguishable, where at version 1 they encoded identically.
	//
	// This is not the inode-reuse check itself: validateAdoptLinkArchive never
	// looks at the live symlink, and a resume's protection against reuse comes
	// from comparing a fresh capture against the WAL. What the handle buys here
	// is a durable record precise enough for a future restore to trust -- and
	// that a WAL and an archive describing two different objects no longer
	// reconcile.
	reused := archiveRecordFixture(store.FileIdentity{Device: 7, Inode: 11, Handle: "1:ffff"})
	if err := validateAdoptLinkArchive(s, name, reused); err == nil {
		t.Fatal("a record differing only in its handle must not validate against this archive")
	}
}
