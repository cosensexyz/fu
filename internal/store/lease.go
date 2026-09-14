package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/sys/unix"
)

// LeasePrefix names the evidence files this package writes beside the
// temporary objects they cover. A lease and its object share one token --
// `.fu-lease-<token>` beside `.fu-src-<token>` -- so a person reading the
// directory can see which record covers which object without opening either.
const LeasePrefix = ".fu-lease-"

// errLeaseHeld means another descriptor holds the lease, which is the only
// evidence fu accepts that the object it covers is still in use.
var errLeaseHeld = errors.New("lease is held by a live holder")

// errLeaseNotRegular means something that is not a regular file stands at a
// lease name. fu only ever creates a regular file there, so anything else was
// put there by someone else and is not evidence this code may act on.
var errLeaseNotRegular = errors.New("lease name is not a regular file")

// LeaseKind says what a lease covers, so a reader and a collector can tell a
// source checkout from a half-built clone without inspecting the object.
type LeaseKind string

const (
	LeaseSourceScratch LeaseKind = "source-scratch"
	LeaseCloneScratch  LeaseKind = "clone-scratch"
	LeaseStagedRoot    LeaseKind = "staged-root"
)

// leaseRecord is what a lease file holds.
//
// Identity is absent until the object exists: a lease is written before the
// object it covers, so that a crash between the two leaves evidence rather
// than an unexplained directory. Pid and Created are for a person reading the
// file and are never consulted by any decision -- a pid can be reused by an
// unrelated process, so it cannot answer whether the holder lives, and the
// answer to that question is the flock and nothing else.
type leaseRecord struct {
	Version int       `json:"version"`
	Kind    LeaseKind `json:"kind"`
	// Token is what the lease and its object share, and what a collector
	// matches on. A scratch is renamed twice over its life -- to a quarantine
	// name at close, to an orphan name when its constructor fails -- and every
	// one of those names ends in this same token. Matching the token rather
	// than a spelling is what lets a rename cost nothing: there is no window
	// in which the record has to be rewritten to stay true, because it never
	// claimed one spelling in the first place.
	Token string `json:"token"`
	// Name is the spelling at the time of writing, kept for a person reading
	// the file. Decisions go through Token.
	Name     string        `json:"name"`
	Identity *FileIdentity `json:"identity,omitempty"`
	Pid      int           `json:"pid"`
	Created  string        `json:"created"`
}

const leaseVersion = 1

// atFDCWD is unix.AT_FDCWD under a name that reads as what it is at call
// sites that resolve an absolute path rather than one relative to a directory.
const atFDCWD = unix.AT_FDCWD

// randomLeaseName is prefix plus 128 bits from crypto/rand. The randomness is
// load-bearing twice over: it makes a token collision impossible in practice,
// and it is half the authority for removing an object whose identity was never
// captured (see removeLeasedObject).
func randomLeaseName(prefix string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate lease name: %w", err)
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

// Lease is a held lease: the open, locked descriptor plus the record it
// carries. The lock lives exactly as long as this value's descriptor, which is
// what makes releasing it on process death the kernel's job rather than fu's.
type Lease struct {
	file   *os.File
	path   string
	record leaseRecord
}

// Name is the object this lease covers.
func (l *Lease) Name() string { return l.record.Name }

// Path is the lease file itself.
func (l *Lease) Path() string { return l.path }

// Token is the tail every name this lease's object may wear must end in.
// A producer renaming the object builds the new name from this, which is what
// keeps the record true without rewriting it.
func (l *Lease) Token() string { return l.record.Token }

// LeasedObjectPrefixes are the prefixes a leased object's name may carry. Every
// such name ends in its lease's token, so a collector resolves a lease to
// whichever of these currently exists.
//
// Adding a producer means adding its prefix here: a leased object under a
// prefix this list omits is invisible to the collector, which is the failure
// this whole mechanism exists to end. What stops that being a matter of memory
// is that the prefixes below are constants a producer must name, so a
// misspelling is a build error; and that each producer's own spelling is
// exercised end to end -- TestEveryProducerLeavesAnObjectTheCollectorCanFind
// for the three made in this package, TestScratchQuarantineNameKeepsTheLeaseToken
// and TestOrphanedScratchNameStaysMatchedToItsLease (internal/source) for the
// two the source scratch renames to.
var LeasedObjectPrefixes = []string{
	SourceScratchCleanPrefix,
	SourceScratchOrphanPrefix,
	SourceScratchPrefix,
	cloneScratchPrefix,
	stagedRootPrefix,
	RetiredLeasedPayloadPrefix,
}

// The prefixes a leased object's name may carry, named so that producers
// reference them rather than retyping the text.
//
// This is deliberately a compile-time constraint rather than a tested one. Two
// successive guards for it could not fail: one read the AcquireLease call sites
// for string literals and found none, because they pass variables; the next
// scanned producer sources for a list of known-good prefixes, so a mistyped one
// was invisible by construction -- it simply was not in the list being looked
// for. Both enumerate what they expect to find, which is exactly why neither
// noticed what it did not expect. A constant the producer must name turns the
// same mistake into a build error.
const (
	SourceScratchPrefix       = ".fu-src-"
	SourceScratchCleanPrefix  = ".fu-src-clean-"
	SourceScratchOrphanPrefix = ".fu-src-orphan-"
)

// RetiredLeasedPayloadPrefix names a leased object mid-removal: reclamation
// retires it under this before deleting it, so that what gets deleted is no
// longer reachable under a name anyone else is using. It is resolvable like
// any other spelling, which is what lets an interrupted removal be finished by
// the next run instead of stranding the object under a name only the
// interrupted run knew.
const RetiredLeasedPayloadPrefix = ".fu-retired-payload-"

// afterLeaseCreateHook fires once a lease file exists and before its lock is
// taken. That is the window in which the file is listable and unlocked, so
// anyone scanning staging can lock it first -- which is why the acquisition
// below blocks rather than failing. Nil outside tests.
var afterLeaseCreateHook func(path string)

// keepLeaseAlive stops a lease's descriptor being finalised while the object it
// covers is still in use. A garbage-collected *os.File closes its descriptor,
// and closing the descriptor drops the flock -- which would tell every other
// process the holder had died while it was still working.
func keepLeaseAlive(values ...any) {
	for _, value := range values {
		runtime.KeepAlive(value)
	}
}

// acquireLeaseFile opens path and takes the lock, waiting if another holder
// has it. Used by the holder and by tests; collectors use tryAcquireLeaseFile,
// which must never wait.
func acquireLeaseFile(path string) (*os.File, error) {
	return openLeaseFile(path, unix.LOCK_EX)
}

// tryAcquireLeaseFile takes the lock or reports errLeaseHeld at once. A
// collector must never block here: a lease held by a live holder is an answer
// ("still in use, leave it alone"), not something to wait for.
func tryAcquireLeaseFile(path string) (*os.File, error) {
	return openLeaseFile(path, unix.LOCK_EX|unix.LOCK_NB)
}

// openLeaseDescriptor opens a lease for reading, and refuses anything that is
// not fu's own evidence file.
//
// O_NONBLOCK and the regular-file check are one guard, not two. Without them a
// FIFO named .fu-lease-<32 chars> -- which fu can never create, and which any
// process able to write to staging can -- parks the open forever waiting for a
// writer. That wedged `fu status` and `fu gc`, and because the sweep now holds
// fu.lock across its whole run, it wedged every write command behind it too.
// One unopenable entry must cost that entry its verdict, not the tool its
// ability to run. O_NONBLOCK gets the open back; the S_IFREG check is what
// then refuses the thing, since a non-blocking open of a FIFO succeeds.
func openLeaseDescriptor(path string) (int, error) {
	return openLeaseDescriptorAt(unix.AT_FDCWD, path, path)
}

// openLeaseDescriptorAt is openLeaseDescriptor relative to a pinned directory.
// path is carried only so errors name something a reader can find.
func openLeaseDescriptorAt(parentFD int, name, path string) (int, error) {
	// O_NOFOLLOW here for the same reason openLeaseFile states below: this
	// descriptor's contents decide which object a lease covers, so following a
	// symlink would let whatever it resolved to answer for a name under
	// staging.
	fd, err := unix.Openat(parentFD, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return -1, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("%w: %s", errLeaseNotRegular, path)
	}
	return fd, nil
}

func openLeaseFile(path string, how int) (*os.File, error) {
	return openLeaseFileAt(unix.AT_FDCWD, path, path, how)
}

func openLeaseFileAt(parentFD int, name, path string, how int) (*os.File, error) {
	// O_NOFOLLOW, via openLeaseDescriptor: following a symlink planted at a
	// lease name would lock whatever the link resolved to, so two processes
	// resolving differently would both believe they held it.
	// No O_CREAT: a lease file is created exactly once, by AcquireLease with
	// O_EXCL. Every other open must find one that already exists -- creating
	// one here would invent the very evidence these decisions rest on, and
	// would also make the read-only scan a writer.
	// O_RDONLY: flock needs only an open descriptor, and the scan behind
	// `fu status` must keep working where the tree is not writable. A holder
	// that has to write its record opens its own descriptor for that.
	fd, err := openLeaseDescriptorAt(parentFD, name, path)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, how); err != nil {
		_ = unix.Close(fd)
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", errLeaseHeld, path)
		}
		return nil, &os.PathError{Op: "flock", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open lease %s: invalid descriptor", path)
	}
	return file, nil
}

// releaseLeaseFile drops the lock by closing the descriptor. Closing is the
// release: an explicit LOCK_UN first would leave a window in which the file is
// open and unlocked, which is the one state no holder should ever be in.
func releaseLeaseFile(file *os.File) {
	if file == nil {
		return
	}
	_ = file.Close()
}

// AcquireLease writes a lease for an object about to be created under
// stagingDir, and returns it locked.
//
// The lease is written and fsynced before the object exists, which is the
// whole point: a crash in the window between them leaves a record saying what
// fu was about to do, and a record is what makes the leftover collectable
// instead of a directory nobody can account for.
func AcquireLease(stagingDir string, kind LeaseKind, objectName string) (_ *Lease, retErr error) {
	if objectName == "" {
		return nil, errors.New("a lease must name the object it covers")
	}
	token := leaseTokenOf(objectName)
	// leaseTokenOf is deliberately total: a name whose prefix is not registered
	// comes back whole, so the lease would be named for the object rather than
	// derived from it. Such a lease writes and fsyncs cleanly and is then
	// refused by every later classification, forever -- a payload carrying a
	// record nobody can act on, which is the exact failure this mechanism was
	// built to end. Refusing here puts a forgotten LeasedObjectPrefixes entry
	// in front of whoever forgot it.
	if !isLeaseToken(token) {
		return nil, fmt.Errorf("lease object name %q carries no registered prefix, so it yields no token", objectName)
	}
	path := filepath.Join(stagingDir, LeasePrefix+token)
	// O_EXCL through the create below: a lease whose name is already taken
	// means the token collided, which crypto/rand makes impossible in
	// practice and which must still not silently adopt somebody else's file.
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, &os.PathError{Op: "create lease", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("create lease %s: invalid descriptor", path)
	}
	defer func() {
		if retErr != nil {
			_ = file.Close()
			_ = unix.Unlink(path)
		}
	}()
	if afterLeaseCreateHook != nil {
		afterLeaseCreateHook(path)
	}
	// Blocking, unlike every other acquisition here. Between the create above
	// and this line the file exists, is listable and is unlocked, so a
	// concurrent read-only scan -- which takes no fu.lock and may run at any
	// moment -- can take its shared probe first and make a non-blocking
	// attempt fail. That surfaced as `fu add <url>` refusing to start because
	// somebody ran `fu status`.
	//
	// Deadlock is not reachable, and the reason is the O_EXCL above rather than
	// anything about how long other holders keep their locks. A collector does
	// keep one for a long time -- classifyLeases releases only after its action
	// has run, which spans removeLeasedTreeWhole's whole RemoveAll -- so "they
	// all release promptly" would be false if it were the argument.
	//
	// What holds instead: this file did not exist a moment ago, so no holder
	// can predate it. The only processes that can have opened it since are a
	// scan, which releases as soon as it has its verdict, and a collector --
	// and a collector that wins this race finds a zero-length record, which
	// readLeaseRecord rejects, so it classifies the lease unaccountable and
	// returns without acting on anything. Neither can be the one holding a
	// long lock here.
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		return nil, &os.PathError{Op: "flock", Path: path, Err: err}
	}
	lease := &Lease{
		file: file,
		path: path,
		record: leaseRecord{
			Version: leaseVersion,
			Kind:    kind,
			Token:   token,
			Name:    objectName,
			Pid:     os.Getpid(),
			Created: time.Now().UTC().Format(time.RFC3339),
		},
	}
	if err := lease.write(stagingDir); err != nil {
		return nil, err
	}
	return lease, nil
}

// AttachIdentity records the identity of the object once it exists, turning a
// lease that merely says what fu intended into one that can prove what it
// made.
func (l *Lease) AttachIdentity(stagingDir string, identity FileIdentity) error {
	if !identity.Valid() {
		return errors.New("a lease cannot record an invalid identity")
	}
	l.record.Identity = &identity
	return l.write(stagingDir)
}

func (l *Lease) write(stagingDir string) error {
	body, err := json.Marshal(l.record)
	if err != nil {
		return fmt.Errorf("encode lease %s: %w", l.path, err)
	}
	body = append(body, '\n')
	// No Truncate. It was there to shorten the file when a later record is
	// smaller, and a record only ever grows -- the identity is added and
	// nothing is removed -- so it shortened nothing and cost a window: the
	// truncation is visible to every reader the instant it returns, and a
	// death before the write below left a zero-length lease that no later run
	// can parse and therefore never collects. SIGKILL reaches that window; it
	// is not a machine-crash-only state, as was first supposed.
	//
	// Padding to a fixed width would close the machine-crash half as well, but
	// costs a rewrite of the format for a window no process death can enter.
	// What remains is a torn record from a crash mid-write, reported as
	// unaccountable and recorded in DESIGN §6.
	if _, err := l.file.WriteAt(body, 0); err != nil {
		return fmt.Errorf("write lease %s: %w", l.path, err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync lease %s: %w", l.path, err)
	}
	// The directory entry too: without it a crash can lose the lease itself
	// and leave the object unaccounted for, which is the state this exists to
	// prevent.
	return syncDir(stagingDir)
}

// EntryIdentityAtPath is EntryIdentityAt for an absolute path, which producers
// outside a pinned directory descriptor need when attaching an identity.
func EntryIdentityAtPath(path string) (FileIdentity, unix.Stat_t, error) {
	return EntryIdentityAt(atFDCWD, path)
}

// Abandon drops the descriptor and leaves the record in place -- what a death
// looks like from outside, which is otherwise only producible by killing a
// process. It exists for tests; production releases or dies, never this.
func (l *Lease) Abandon() {
	if l == nil {
		return
	}
	releaseLeaseFile(l.file)
	l.file = nil
}

// ReleaseIfObjectGone is Release when nothing is left to account for, and a
// plain unlock when something is.
//
// It is what an unwinding producer wants. A failed cleanup leaves the object on
// disk, and dropping the record there would manufacture exactly the residue
// with no evidence that this mechanism exists to end -- so the record stays,
// the lock is dropped, and a later reclamation deals with it. Evidence must
// neither outlive its object nor predecease it.
func (l *Lease) ReleaseIfObjectGone(stagingDir string) error {
	if l == nil || l.file == nil {
		return nil
	}
	present, err := resolveLeasedObject(stagingDir, l.record.Token)
	if err != nil {
		// Whether anything survives could not be established, so the record
		// stays: keeping evidence that turns out to be unnecessary costs a
		// file, and dropping evidence that turns out to be needed costs the
		// object's only explanation.
		l.Abandon()
		return err
	}
	if present != "" {
		l.Abandon()
		return nil
	}
	return l.Release(stagingDir)
}

// Release removes the lease, and with it the claim. Called once the object it
// covers is gone; the object is removed first, so a crash between the two
// leaves a lease naming an absent object, which a collector settles by
// removing the lease alone.
func (l *Lease) Release(stagingDir string) error {
	defer keepLeaseAlive(l)
	if l == nil || l.file == nil {
		return nil
	}
	unlinkErr := unix.Unlink(l.path)
	if unlinkErr != nil && !errors.Is(unlinkErr, unix.ENOENT) {
		unlinkErr = &os.PathError{Op: "remove lease", Path: l.path, Err: unlinkErr}
	} else {
		unlinkErr = nil
	}
	releaseLeaseFile(l.file)
	l.file = nil
	if unlinkErr != nil {
		return unlinkErr
	}
	return syncDir(stagingDir)
}

func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", path, err)
	}
	return nil
}

// leaseTokenOf is the random tail an object name carries, which its lease
// reuses so the pair is visible in a directory listing. A name with no
// recognisable prefix is used whole, which keeps this total rather than
// letting an unexpected caller produce a lease named for nothing.
// LeaseTokenOf is leaseTokenOf for producers outside this package, which need
// it to build a rename target that keeps the object matched to its lease.
func LeaseTokenOf(objectName string) string { return leaseTokenOf(objectName) }

func leaseTokenOf(objectName string) string {
	for _, prefix := range LeasedObjectPrefixes {
		if len(objectName) > len(prefix) && objectName[:len(prefix)] == prefix {
			return objectName[len(prefix):]
		}
	}
	return objectName
}
