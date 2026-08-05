// internal/store/config.go
package store

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/cosensexyz/fu/internal/skill"
)

// SupportedVersion is the highest fu.yaml version this build understands.
const SupportedVersion = 1

// MaxConfigBytes bounds every fu.yaml snapshot. The limit leaves ample room
// inside the larger transaction-record envelope after JSON base64 encoding
// while preventing a hand-edited config from consuming unbounded memory.
const MaxConfigBytes int64 = 8 << 20

// MinSupportedVersion is the lowest one it understands. There has only ever
// been one schema, so this equals SupportedVersion today; it exists so that
// "a version this build can work with" is a stated range rather than an
// open-ended "not too new" (round 8 finding). Version 0 was previously
// accepted and writable despite never having been a schema fu defined.
const MinSupportedVersion = 1

// ErrVersionTooNew is returned by Save when the loaded fu.yaml version
// exceeds SupportedVersion: reads of such a file are best-effort, but
// writes are refused so an older fu never truncates a newer schema.
var ErrVersionTooNew = errors.New(
	"fu.yaml version is newer than this fu supports; refusing to write")

// ErrMalformedConfig is wrapped into every error LoadConfig returns
// because a parsed fu.yaml does not have the shape every mutator in
// this file assumes (skills must be a mapping of mappings; enabled and
// override values must be boolean scalars). fu.yaml lives inside a git
// repository the user may hand-edit -- such edits are committed as
// "external modifications" -- so a malformed file is a realistic
// input, not a theoretical one, and callers can test for it with
// errors.Is.
var ErrMalformedConfig = errors.New("fu.yaml: malformed config")

// Config wraps fu.yaml as a yaml.Node tree so unknown fields survive
// load-modify-save round trips untouched (DESIGN §3).
type Config struct {
	doc      *yaml.Node
	path     string
	fsRoot   *os.Root
	rootPath string
	version  int
	invalid  []InvalidName // skill entries LoadConfig found under a name that fails validation; see InvalidNames
}

// InvalidName pairs a fu.yaml skill name that fails skill.ValidateName with
// why, so a caller can report it (round 4 finding 2).
type InvalidName struct {
	Name   string
	Reason string
}

func defaultDoc() *yaml.Node {
	var doc yaml.Node
	_ = yaml.Unmarshal([]byte("version: 1\nskills: {}\n"), &doc)
	return &doc
}

// NewConfig builds an empty in-memory config for path, at SupportedVersion
// with no skills. It writes nothing; Save is what puts it on disk.
//
// This is the half of the old LoadConfig that legitimately produced a fresh
// config, split out so that loading can be strict (round 6 finding; see
// missingConfigErr). Init writes the store's first fu.yaml as raw bytes and
// does not go through here; the callers that do are tests constructing a
// config directly, without a store behind it.
func NewConfig(path string) *Config {
	return &Config{path: path, version: SupportedVersion, doc: defaultDoc()}
}

// ErrConfigMissing is returned by LoadConfig when fu.yaml is absent or
// blank. Callers can test for it with errors.Is; the message itself carries
// the path and the recovery route.
var ErrConfigMissing = errors.New("fu.yaml is missing or empty")

// missingConfigErr refuses to invent a config for an initialized store.
//
// LoadConfig used to answer "missing or blank fu.yaml" with a fresh, empty
// config. That is the right answer to "there is no store yet" and a
// catastrophic one to "the store exists and its config was destroyed" --
// and by the time anything calls LoadConfig, Store.Open has already proven
// the second reading is the only possible one (round 6 finding). The next
// ordinary write then rebuilt fu.yaml from nothing: Sweep committed the
// deletion into history, Save wrote back a file holding only that command's
// own mutation, and Reconcile removed every delivered link whose skill was
// missing from the reconstruction. A single `fu new beta` discarded every
// registration, switch, override and source record the user had, and
// recorded the loss in git as an ordinary operation.
//
// The store is a git repository, which is exactly what makes this
// recoverable -- provided fu stops before committing over it. The message
// says so, because nothing else will.
func missingConfigErr(path, what string) error {
	return fmt.Errorf("%s %s: %w; the store is a git repository, so a previous version is "+
		"recoverable with `git -C %s checkout -- fu.yaml` (inspect `git -C %s log -- fu.yaml` first)",
		path, what, ErrConfigMissing, filepath.Dir(path), filepath.Dir(path))
}

// LoadConfig reads path into a Config. It is strict: a missing or blank
// file is an error wrapping ErrConfigMissing (round 6 finding -- see
// missingConfigErr for what leniency here used to cost), and a file that
// parses as YAML but does not have the shape every mutator in this package
// assumes (see validateConfigTree) is rejected with an error wrapping
// ErrMalformedConfig, rather than being accepted and silently losing writes
// later. Use NewConfig to construct a fresh, empty config.
//
// One exception (round 4 finding 2, softening round 3 finding 2's own
// bdf2882): a skill *name* that fails skill.ValidateName does not reject
// the file. It used to -- wrapped in the same ErrMalformedConfig as every
// other structural violation here -- on the reasoning that such a name is
// a trust boundary (it becomes a path component wherever a skill is
// looked up on disk) and LoadConfig is the one place every caller loads
// its config through. That much still holds, but rejecting the *whole*
// file over *one* bad entry cost more than it closed: every command,
// including read-only ones like `fu list` and `fu show <anything>`,
// started failing the moment fu.yaml carried a single invalid name,
// anywhere in it -- and it made a sibling fix (engine.Desired's own
// name-filtering, so a stray fu-owned link recorded under such a name can
// still be reclaimed) unreachable in production, since no command ever
// got as far as calling Desired with a *Config LoadConfig had already
// refused to build. The offending entry is now excluded from the config's
// skill set (SkillNames, HasSkill, and therefore every accessor gated on
// either) and collected into invalid instead, leaving the rest of fu.yaml
// -- including any other skill's own overrides, digest, etc. -- fully
// loadable and writable. The underlying document is left untouched (the
// entry is filtered at the accessor boundary, not deleted from c.doc), so
// Save round-trips it unchanged rather than silently erasing it from
// fu.yaml as a side effect of an unrelated write -- only a human
// hand-editing fu.yaml removes it (this plan ships no `fu rm`). See
// InvalidNames for how a caller reports these.
func LoadConfig(path string) (*Config, error) {
	return loadConfigWithHooks(path, regularFileReadHooks{})
}

func loadConfigWithHooks(path string, hooks regularFileReadHooks) (*Config, error) {
	c := &Config{path: path, version: SupportedVersion}
	raw, err := readRegularFileWithHooks(path, MaxConfigBytes, hooks)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, missingConfigErr(path, "does not exist")
		}
		return nil, err
	}
	return decodeConfig(c, raw)
}

// LoadConfigRoot reads and later saves a config through an already checked
// root. displayPath is retained for diagnostics and recovery instructions.
func LoadConfigRoot(root *os.Root, rootPath, displayPath string) (*Config, error) {
	cfg, _, err := LoadConfigRootBytes(root, rootPath, displayPath)
	return cfg, err
}

// LoadConfigRootBytes returns the parsed config together with the exact bytes
// it was parsed from, so a caller can later prove the file still holds the
// snapshot its in-memory model describes. Reading twice would not prove that:
// the two reads can straddle an external edit, leaving a model of one version
// and a baseline of another.
func LoadConfigRootBytes(root *os.Root, rootPath, displayPath string) (*Config, []byte, error) {
	c := &Config{path: displayPath, fsRoot: root, rootPath: rootPath, version: SupportedVersion}
	raw, err := ReadConfigFileRoot(root, rootPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, missingConfigErr(displayPath, "does not exist")
		}
		return nil, nil, err
	}
	cfg, err := decodeConfig(c, raw)
	if err != nil {
		return nil, nil, err
	}
	return cfg, raw, nil
}

// LoadConfigBytes parses a durable config snapshot without reading a path.
// displayPath is used only in diagnostics.
func LoadConfigBytes(raw []byte, displayPath string) (*Config, error) {
	return decodeConfig(&Config{path: displayPath, version: SupportedVersion}, raw)
}

// ReadConfigFile reads one stable, bounded fu.yaml snapshot without parsing
// it. Pipeline comparisons use this so validation, rollback, and recovery
// observe the same filesystem boundary as LoadConfig.
func ReadConfigFile(path string) ([]byte, error) {
	return ReadRegularFile(path, MaxConfigBytes)
}

// ReadConfigFileRoot is ReadConfigFile relative to an already checked root.
func ReadConfigFileRoot(root *os.Root, path string) ([]byte, error) {
	return ReadRegularFileRoot(root, path, MaxConfigBytes)
}

func decodeConfig(c *Config, raw []byte) (*Config, error) {
	if err := validateConfigSize(raw); err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse fu.yaml: %w", err)
	}
	if docRoot(&doc).Kind == 0 {
		// Blank file: empty, or holding nothing but whitespace. Round 6
		// finding -- see missingConfigErr for why this is refused rather
		// than treated as a fresh config.
		return nil, missingConfigErr(c.path, "is empty")
	}
	invalid, err := validateConfigTree(docRoot(&doc))
	if err != nil {
		return nil, err
	}
	c.doc = &doc
	c.invalid = invalid
	if v := mapGet(c.root(), "version"); v != nil {
		// validateConfigTree above already confirmed this parses cleanly.
		version, _ := parseVersion(v)
		c.version = version
	}
	return c, nil
}

// InvalidNames returns, in file order, every skill entry LoadConfig found
// under a name that fails skill.ValidateName and excluded from the
// config's skill set (round 4 finding 2). Empty for a config with no such
// entries. A Config built purely in memory via AddSkill -- as this
// package's own and engine's tests deliberately do, to exercise
// ValidateName's callers as defence in depth -- never populates this:
// only LoadConfig's file-parsing path does, since AddSkill itself does not
// validate (see validateConfigTree's doc comment for why that division of
// labor is intentional).
func (c *Config) InvalidNames() []InvalidName { return c.invalid }

// CheckWritable reports ErrVersionTooNew without writing anything, so a
// write command can refuse a config it must not touch *before* running
// its mutation (finding I1). Save's own guard below runs too late for
// that purpose: by the time Save returns ErrVersionTooNew, the
// pipeline's op.Mutate has already written into the store directory --
// on a version:99 store, `fu new alpha` left
// store/skills/alpha/SKILL.md on disk even though the command as a
// whole reported failure, and the next write command's Sweep committed
// that residue as "external: manual modifications".
func (c *Config) CheckWritable() error {
	if c.version > SupportedVersion {
		return ErrVersionTooNew
	}
	if c.version < MinSupportedVersion {
		// Reachable only for a Config built in memory: LoadConfig refuses
		// such a version outright (validateConfigTree). Kept as the other
		// half of the stated range, so "writable" means "inside a schema
		// this build defines" rather than merely "not from the future".
		return fmt.Errorf("fu.yaml version %d is below the minimum this build supports (%d); "+
			"refusing to write", c.version, MinSupportedVersion)
	}
	return nil
}

// VersionTooNew reports whether the loaded fu.yaml version exceeds what
// this build supports (DESIGN §3) -- the same condition CheckWritable
// reports as an error. A write command refuses outright and never needs
// to ask; a read-only command (list, show) has no mutation to refuse and
// proceeds best-effort regardless, so it calls this instead to decide
// whether to warn the user that some content may not be understood.
func (c *Config) VersionTooNew() bool {
	return c.version > SupportedVersion
}

// Save refuses to write when the loaded version exceeds support:
// read best-effort, write never (forward-compat guard, DESIGN §3).
//
// Marshaling goes through an Encoder with a 2-space indent (yaml.Marshal
// itself always uses 4) and clearStyle strips any flow style picked up
// from parsing before every write (finding I2): both defaultDoc's seed
// and Init's own bootstrap fu.yaml write "skills: {}", and yaml.v3 marks
// a mapping node parsed from that "{}" syntax as flow-style permanently
// -- once set, that style is not just kept on the node itself but forces
// every descendant added later (each skill entry, its overrides) to
// render in flow style too, since block style cannot be nested inside an
// already-open flow collection. Left unfixed, real stores degrade to a
// single unreadable, unmergeable line the moment a second skill is added.
func (c *Config) Save() error {
	if err := c.CheckWritable(); err != nil {
		return err
	}
	out, err := c.Bytes()
	if err != nil {
		return err
	}
	if c.fsRoot != nil {
		return WriteFileAtomicRoot(c.fsRoot, c.rootPath, out, 0o644)
	}
	return WriteFileAtomic(c.path, out, 0o644)
}

// ErrConfigChangedExternally means fu.yaml no longer held the bytes the
// operation started from when it came time to install the operation's own.
var ErrConfigChangedExternally = errors.New("fu.yaml changed outside the operation after the initial sweep")

// configSwapName is the one active scratch entry a conditional config install
// uses. A uniquely named candidate is filled and durably recorded before it is
// moved here, so an active entry left by a crash has identity-bound recovery
// authority rather than being an unclassified file the next process must
// reject forever. Completed entries are retained under configArchivePrefix.
//
// It is created exclusively for every exchange. Reusing even an empty regular
// file would adopt an inode fu does not own: that file may be a hard link to a
// path outside the store, and filling it would mutate the external path too.
//
// A surviving active entry without a matching record remains a namespace
// conflict. The next install preserves and reports it rather than guessing
// that its inode belongs to fu.
const configSwapName = ".fu-config-swap"

// Completed config-exchange objects are retained rather than unlinked or
// truncated. POSIX has no portable identity-conditioned unlink, and ftruncate
// would change every hard link and open descriptor referring to the inode.
const configArchivePrefix = ".fu-config-archive-"

// SaveConfigExpecting installs cfg's bytes at fu.yaml only while the file
// still holds expect, and preserves whatever is there instead if it does not.
//
// Save alone cannot offer that. It ends in a replace-rename, which destroys
// the destination whatever it currently is, so an edit arriving in the window
// between the operation's sweep and its own write was overwritten and the
// command committed as if nothing had happened -- the frozen candidate only
// ever sees fu's bytes, so nothing downstream noticed either.
//
// Reading, comparing and then renaming would leave the same window, only
// narrower. The exchange closes it: after one atomic swap the name holds fu's
// bytes and the scratch entry holds whatever was displaced, so the object that
// was canonical at the instant of the swap is in fu's hands and can be judged.
// Exchange rather than move-aside-then-install because there is no instant
// where fu.yaml is absent: every point in this sequence, including a crash,
// leaves one of the two complete versions in place.
//
// The scratch entry lives in staging, never beside fu.yaml. staging is a
// sibling of the repository under the same $FU_HOME, so the exchange is still
// a same-filesystem operation, but a snapshot left by a crash lands outside
// version control. Beside fu.yaml it was ordinary untracked store content, and
// the next command's sweep committed it into history under "external: manual
// modifications".
func (s *Store) SaveConfigExpecting(cfg *Config, expect []byte) error {
	if err := cfg.CheckWritable(); err != nil {
		return err
	}
	out, err := cfg.Bytes()
	if err != nil {
		return err
	}
	return s.InstallConfigExpecting(expect, out)
}

// InstallConfigExpecting is the same conditional install for callers that
// already hold the bytes -- rollback and recovery, which would otherwise
// compare and then replace, leaving the window the comparison was meant to
// close.
func (s *Store) InstallConfigExpecting(expect, data []byte) error {
	return s.installConfigExpecting(expect, data, configExchangeHooks{})
}

// RecoverConfigExchanges finishes or withdraws every identity-bound config
// exchange left by a process exit. It is called by the unified write recovery
// entry before command-level WAL handling, and again by conditional installs
// so direct Store callers receive the same guarantee.
func (s *Store) RecoverConfigExchanges() error {
	if s.writeRoots == nil || s.writeRoots.store == nil || s.writeRoots.store.dir == nil ||
		s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil ||
		s.writeRoots.recovery == nil || s.writeRoots.recovery.dir == nil {
		return errors.New("store is not attached to checked store, staging, and recovery roots")
	}
	return recoverPendingConfigExchanges(s.writeRoots.store, s.writeRoots.staging, s.writeRoots.recovery)
}

// configExchangeHooks is a test-only seam at the two boundaries a concurrent
// writer can be observed: with fu's bytes staged but not yet published, and
// with the displaced object parked but not yet judged.
type configExchangeHooks struct {
	afterRecord    func()
	beforeExchange func()
	afterExchange  func()
}

func (s *Store) installConfigExpecting(expect, data []byte, hooks configExchangeHooks) error {
	if s.writeRoots == nil || s.writeRoots.store == nil || s.writeRoots.store.dir == nil ||
		s.writeRoots.staging == nil || s.writeRoots.staging.dir == nil ||
		s.writeRoots.recovery == nil || s.writeRoots.recovery.dir == nil {
		return errors.New("store is not attached to checked store, staging, and recovery roots")
	}
	if err := s.RecoverConfigExchanges(); err != nil {
		return fmt.Errorf("recover interrupted config exchange: %w", err)
	}
	return exchangeCheckedFile(s.writeRoots.store, "fu.yaml", s.writeRoots.staging, s.writeRoots.recovery, expect, data, 0o644, hooks)
}

// openConfigCandidate creates a fresh private inode under a unique name. It is
// not moved to the fixed active name until its identity and byte digests have
// been durably recorded, eliminating an unowned crash window.
func openConfigCandidate(scratch *checkedRoot) (*os.File, unix.Stat_t, string, error) {
	file, name, err := createTempAt(scratch.dir, configCandidatePrefix)
	if err != nil {
		return nil, unix.Stat_t{}, "", err
	}
	var opened unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &opened); err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, "", err
	}
	if err := requireRegularStat(name, &opened); err != nil {
		_ = file.Close()
		return nil, unix.Stat_t{}, "", err
	}
	if opened.Size != 0 {
		_ = file.Close()
		return nil, unix.Stat_t{}, "", fmt.Errorf(
			"%s/%s was nonempty immediately after exclusive creation", scratch.display, name)
	}
	return file, opened, name, nil
}

func configArchiveName(identity FileIdentity) string {
	return fmt.Sprintf("%s%016x-%016x", configArchivePrefix, identity.Device, identity.Inode)
}

// archiveNamedConfigEntry moves a recorded candidate or active scratch name to
// a no-replace terminal archive and proves the same inode arrived there. Fu
// never mutates or deletes the object resolved by the terminal namespace
// operation; if a writer wins a race, that object remains preserved.
func archiveNamedConfigEntry(source *checkedRoot, sourceName string, archive *checkedRoot, expected FileIdentity) error {
	if !validLogicalEntry(sourceName) {
		return fmt.Errorf("archive config entry: invalid source name %q", sourceName)
	}
	sourceFD := int(source.dir.Fd())
	archiveFD := int(archive.dir.Fd())
	current, err := statAt(sourceFD, sourceName)
	if err != nil {
		return fmt.Errorf("inspect config exchange entry before archiving: %w", err)
	}
	if identityFromStat(&current) != expected {
		return fmt.Errorf("config exchange entry changed identity before it could be archived")
	}
	name := configArchiveName(expected)
	if err := renameNoReplace(sourceFD, sourceName, archiveFD, name); err != nil {
		return fmt.Errorf("archive config exchange entry as %s/%s: %w", archive.display, name, err)
	}
	archived, err := statAt(archiveFD, name)
	if err != nil {
		return fmt.Errorf("inspect archived config exchange entry %s/%s: %w", archive.display, name, err)
	}
	if identityFromStat(&archived) != expected {
		return fmt.Errorf("archived config exchange entry %s/%s has an unexpected identity; it is preserved", archive.display, name)
	}
	return nil
}

func exchangeCheckedFile(target *checkedRoot, name string, scratch, archive *checkedRoot, expect, data []byte, perm os.FileMode, hooks configExchangeHooks) error {
	targetFD := int(target.dir.Fd())
	scratchFD := int(scratch.dir.Fd())
	targetPath := filepath.Join(target.display, name)
	scratchPath := filepath.Join(scratch.display, configSwapName)

	if _, err := statAt(scratchFD, configSwapName); err == nil {
		return fmt.Errorf(
			"%s already exists without a recoverable completed exchange; inspect it before writing fu.yaml again",
			scratchPath)
	} else if !errors.Is(err, unix.ENOENT) {
		return &os.PathError{Op: "fstatat", Path: scratchPath, Err: err}
	}

	staged, stagedStat, candidateName, err := openConfigCandidate(scratch)
	if err != nil {
		return err
	}
	defer staged.Close()
	// Held open from here to the end. The descriptor binds validation to the
	// generated inode; terminal retirement uses a no-replace rename followed by
	// identity revalidation and never mutates the inode itself.
	stagedIdentity := identityFromStat(&stagedStat)
	if err := fillRegularFile(staged, data, perm); err != nil {
		return err
	}
	// What fu.yaml is about to become is not enough; the exchange also has to
	// know what it currently is, because the object it displaces is the only
	// evidence the file held the expected bytes at the instant of the swap.
	previousStat, err := statAt(targetFD, name)
	if err != nil {
		return &os.PathError{Op: "fstatat", Path: targetPath, Err: err}
	}
	previousIdentity := identityFromStat(&previousStat)
	record := configExchangeRecord{
		Version:      configExchangeRecordVersion,
		Candidate:    candidateName,
		Previous:     previousIdentity,
		Staged:       stagedIdentity,
		ExpectDigest: digestConfigExchangeBytes(expect),
		DataDigest:   digestConfigExchangeBytes(data),
	}
	recordRaw, err := writeConfigExchangeRecord(archive, record)
	if err != nil {
		return err
	}
	if hooks.afterRecord != nil {
		hooks.afterRecord()
	}
	if current, statErr := statAt(scratchFD, candidateName); statErr != nil || identityFromStat(&current) != stagedIdentity {
		return fmt.Errorf("%w (%s/%s changed before its recorded candidate could be published; it is preserved)",
			ErrConfigChangedExternally, scratch.display, candidateName)
	}
	if err := renameNoReplace(scratchFD, candidateName, scratchFD, configSwapName); err != nil {
		publishErr := fmt.Errorf("publish recorded config candidate at %s: %w", scratchPath, err)
		if archiveErr := archiveNamedConfigEntry(scratch, candidateName, archive, stagedIdentity); archiveErr != nil {
			return errors.Join(publishErr, fmt.Errorf("preserve unpublished config candidate: %w", archiveErr))
		}
		if completeErr := completeConfigExchange(archive, record, recordRaw, "withdrawn-before-publication"); completeErr != nil {
			return errors.Join(publishErr, completeErr)
		}
		return publishErr
	}
	if hooks.beforeExchange != nil {
		hooks.beforeExchange()
	}
	// The scratch name is unproven on the way in too: fu wrote its bytes
	// through a descriptor, but the exchange addresses a name, and a
	// replacement arriving in between would be installed as fu.yaml -- fu
	// publishing content it never generated.
	if current, statErr := statAt(scratchFD, configSwapName); statErr != nil || identityFromStat(&current) != stagedIdentity {
		return fmt.Errorf("%w (%s was replaced before it could be published; it is preserved and %s is untouched)",
			ErrConfigChangedExternally, scratchPath, targetPath)
	}
	if err := renameExchange(targetFD, name, scratchFD, configSwapName); err != nil {
		return fmt.Errorf("exchange %s with its replacement: %w", targetPath, err)
	}
	if hooks.afterExchange != nil {
		hooks.afterExchange()
	}

	// The one and only resolution of the scratch name after the swap. From here
	// the displaced object is the descriptor, not the name.
	displaced, displacedStat, err := openRegularFileAt(scratchFD, configSwapName)
	if err != nil {
		return fmt.Errorf("%w (the displaced %s is parked at %s but could not be inspected: %v)",
			ErrConfigChangedExternally, targetPath, scratchPath, err)
	}
	defer displaced.Close()
	previousBytes, err := readAllRegularFile(displaced, configSwapName, displacedStat, MaxConfigBytes)
	if err == nil && identityFromStat(&displacedStat) == previousIdentity && bytes.Equal(previousBytes, expect) {
		// The precondition held. Retain the superseded inode unchanged: a path
		// outside fu may be a hard link to it, and an external process may still
		// hold a descriptor for it after the exchange.
		if archiveErr := archiveNamedConfigEntry(scratch, configSwapName, archive, previousIdentity); archiveErr != nil {
			return fmt.Errorf("archive the displaced %s parked at %s without modifying it: %w", targetPath, scratchPath, archiveErr)
		}
		if err := completeConfigExchange(archive, record, recordRaw, "installed"); err != nil {
			return err
		}
		return nil
	}

	// The precondition did not hold. The parked object is what was canonical at
	// the instant of the swap, so it goes back: an external writer who replaced
	// fu.yaml owns it, and DESIGN §6 keeps their version there for the next
	// external commit rather than demoting it into staging.
	//
	// A scratch entry substituted between the exchange and the open above is
	// indistinguishable from that, and would be promoted here. It stays the
	// right trade: reaching that window needs a same-user process, and such a
	// process can write fu.yaml directly anyway, so it gains nothing -- while
	// refusing to restore would demote an ordinary concurrent writer's config
	// on every occurrence.
	current, statErr := statAt(targetFD, name)
	installed, readCurrentErr := readRegularFileAt(targetFD, name, MaxConfigBytes)
	// Identity and content both, because they catch different writers: an
	// atomic replace leaves a new inode, while a plain rewrite truncates fu's
	// own file in place and leaves the identity untouched. When either says the
	// name no longer holds what fu installed, that newer version stays where it
	// is -- restoring over it would be the same overwrite this whole protocol
	// exists to prevent -- and the displaced version stays parked for the next
	// install to report.
	if statErr != nil || readCurrentErr != nil || identityFromStat(&current) != stagedIdentity || !bytes.Equal(installed, data) {
		return fmt.Errorf("%w (a third version was installed at %s while the exchange was in progress; it is left in place and the displaced version is parked at %s)",
			ErrConfigChangedExternally, targetPath, scratchPath)
	}
	if swapErr := renameExchange(scratchFD, configSwapName, targetFD, name); swapErr != nil {
		return fmt.Errorf("%w (the displaced %s is parked at %s because restoring it failed: %v)",
			ErrConfigChangedExternally, targetPath, scratchPath, swapErr)
	}
	// fu's generated inode is back under the scratch name. Retain it by the same
	// no-replace protocol: a same-user process may have linked to the visible
	// scratch name while the exchange was in progress, so even this inode is not
	// safe to truncate.
	if archiveErr := archiveNamedConfigEntry(scratch, configSwapName, archive, stagedIdentity); archiveErr != nil {
		return fmt.Errorf("%w (and fu's withdrawn config could not be archived from %s without modifying it: %v)",
			ErrConfigChangedExternally, scratchPath, archiveErr)
	}
	if completeErr := completeConfigExchange(archive, record, recordRaw, "withdrawn-after-precondition-mismatch"); completeErr != nil {
		return errors.Join(ErrConfigChangedExternally, completeErr)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", ErrConfigChangedExternally, err)
	}
	return ErrConfigChangedExternally
}

// fillRegularFile replaces an already-open file's contents without going back
// through its name.
func fillRegularFile(file *os.File, data []byte, perm os.FileMode) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	written, err := file.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Chmod(perm)
	}
	if err == nil {
		err = file.Sync()
	}
	return err
}

func readAllRegularFile(file *os.File, name string, stat unix.Stat_t, maxBytes int64) ([]byte, error) {
	if stat.Size < 0 || stat.Size > maxBytes {
		return nil, fmt.Errorf("regular file %q size %d exceeds limit %d", name, stat.Size, maxBytes)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("regular file %q exceeds limit %d while being read", name, maxBytes)
	}
	return raw, nil
}

func writeCompleteFile(file *os.File, data []byte, perm os.FileMode) error {
	written, err := file.Write(data)
	if err == nil && written != len(data) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = file.Chmod(perm)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// Bytes returns the same normalized YAML representation Save writes.
func (c *Config) Bytes() ([]byte, error) {
	clearStyle(c.doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c.doc); err != nil {
		_ = enc.Close()
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	out := buf.Bytes()
	if err := validateConfigSize(out); err != nil {
		return nil, err
	}
	return out, nil
}

func validateConfigSize(raw []byte) error {
	if int64(len(raw)) > MaxConfigBytes {
		return fmt.Errorf("fu.yaml size %d exceeds configuration limit %d", len(raw), MaxConfigBytes)
	}
	return nil
}

// clearStyle clears only the flow-style bit, and only on mapping/sequence
// nodes (round 2 finding 3): that is the one style finding I2 above needs
// undone (see Save's doc comment), and collections are the only nodes
// that can even carry it meaningfully picked up from stray "{}"/"[]"
// syntax. The previous version reset Style to 0 on every node, scalars
// included, which also erases DoubleQuotedStyle/SingleQuotedStyle,
// LiteralStyle, FoldedStyle, and the implicit "!!merge" tagging a "<<"
// merge key relies on. yaml.v3 itself still round-trips correctly either
// way (it keeps the resolved !!str/!!bool/etc. tag independently of
// Style), but any YAML 1.1 reader -- PyYAML, Ruby Psych, yaml.v2, most
// non-Go libraries -- parses an unquoted yes/no/on/off as a boolean, so
// stripping quotes from a scalar changes what it *means* to every tool
// but fu itself, defeating the entire point of preserving unknown fields
// untouched (DESIGN §3). Reproduced against the compiled binary:
// hand-editing quoted `truthy: "yes"` (etc.), a quoted unicode string,
// and an anchored value into fu.yaml, then running any write command,
// silently unquoted every one of them (and rewrote `<<: *defaults` as
// `!!merge <<: *defaults`, and a folded scalar `>` as a literal `|`).
func clearStyle(n *yaml.Node) {
	if n == nil {
		return
	}
	if n.Kind == yaml.MappingNode || n.Kind == yaml.SequenceNode {
		n.Style &^= yaml.FlowStyle
	}
	for _, child := range n.Content {
		clearStyle(child)
	}
}

func (c *Config) root() *yaml.Node { return docRoot(c.doc) }

// docRoot unwraps the top-level DocumentNode yaml.Unmarshal produces,
// returning the actual content node (mapping, scalar, sequence, or a
// zero-Kind node for a blank document).
func docRoot(doc *yaml.Node) *yaml.Node {
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

// ---- structural validation ----

// validateConfigTree checks that root has the shape every mutator below
// assumes: skills, if present, is a mapping of mappings, and each entry's
// enabled/overrides fields, if present, are boolean scalars; it also
// collects, but does not reject the file over, any skill name that fails
// skill.ValidateName (round 4 finding 2 -- see the returned []InvalidName
// and the block below for why). It is called once by LoadConfig (root
// must not be the zero-Kind node produced by a blank document; LoadConfig
// handles that case earlier) so no mutator needs an error return to plumb
// a structural check through itself.
//
// Skipping the structural checks would let mapSet silently attach
// children to a non-mapping node -- e.g. a scalar "skills: oops" or
// "alpha: oops" -- and yaml.Marshal drops a scalar node's children
// entirely, so a write like SetEnabled would vanish on the next Save with
// no error, while Enabled would keep reporting a stale value forever
// after (mapGet on a non-mapping node returns nil). Those checks are
// still whole-file fatal, unchanged.
// checkUnambiguousKeys rejects duplicate and non-string keys in one mapping
// (round 7 finding). Every accessor in this file resolves a key by scanning
// for the *first* match, so a document carrying the same key twice means
// whatever its reader happens to look at first: `version: 1` followed by
// `version: 99` read as 1, passed the forward-compatibility write guard,
// and was saved back with both entries intact. YAML itself calls duplicate
// mapping keys an error; yaml.v3's node API does not enforce it, so the
// check has to live here.
//
// A non-string key is refused for the neighbouring reason: nothing in this
// package can address it, and what it denotes depends on the reader.
func checkUnambiguousKeys(m *yaml.Node, where string) error {
	seen := make(map[string]bool, len(m.Content)/2)
	for i := 0; i+1 < len(m.Content); i += 2 {
		k := m.Content[i]
		if k.Kind != yaml.ScalarNode || k.Tag != "!!str" {
			return malformedErr(where, fmt.Sprintf("has a non-string key %q", k.Value))
		}
		if seen[k.Value] {
			return malformedErr(where, fmt.Sprintf("has a duplicate key %q, so what the file means "+
				"depends on which reader interprets it", k.Value))
		}
		seen[k.Value] = true
	}
	return nil
}

func validateConfigTree(root *yaml.Node) ([]InvalidName, error) {
	if root.Kind != yaml.MappingNode {
		return nil, malformedErr("fu.yaml", "must be a mapping")
	}
	// Checked before any accessor runs, at every level this file addresses
	// by key -- including keys fu itself does not understand, which are
	// preserved verbatim across writes (DESIGN §3) and so must be
	// unambiguous for whoever does understand them.
	if err := checkUnambiguousKeys(root, "fu.yaml"); err != nil {
		return nil, err
	}
	// A persisted config must say which schema it is (round 8 finding).
	// LoadConfig used to seed the version with SupportedVersion and only
	// overwrite it when the key was present, so a file with no version at all
	// was silently read, mutated and written back under this build's
	// assumptions -- exactly the outcome the forward-compatibility guard
	// exists to prevent, arrived at from the other direction. Only NewConfig
	// assigns a version now, for a config fu is creating rather than reading.
	v := mapGet(root, "version")
	if v == nil {
		return nil, malformedErr("fu.yaml: version",
			"is required: a config with no declared schema cannot be assumed to match this build")
	}
	version, err := parseVersion(v)
	if err != nil {
		return nil, malformedErr("fu.yaml: version", "must be an integer")
	}
	if version < MinSupportedVersion {
		return nil, malformedErr("fu.yaml: version",
			fmt.Sprintf("is %d, below the minimum schema this build defines (%d)", version, MinSupportedVersion))
	}
	skills := mapGet(root, "skills")
	if skills == nil {
		return nil, nil
	}
	if skills.Kind != yaml.MappingNode {
		return nil, malformedErr("fu.yaml: skills", "must be a mapping")
	}
	if err := checkUnambiguousKeys(skills, "fu.yaml: skills"); err != nil {
		return nil, err
	}
	var invalid []InvalidName
	for i := 0; i+1 < len(skills.Content); i += 2 {
		skillName := skills.Content[i].Value
		base := "fu.yaml: skills." + skillName
		// Every key under `skills:` becomes a path component wherever a
		// skill is looked up on disk -- `fu show`'s SKILL.md read
		// (internal/cli/show.go) and the engine's own link materialization
		// (internal/engine/diff.go) both join it straight onto a directory
		// -- so it is checked here once, at the one place every caller
		// loads its config through, rather than trusting each call site to
		// validate it independently (round 3 finding 2: `fu show
		// '../../evilskill'`, with such a key hand-added to fu.yaml and
		// real content planted at the escaped location, printed that
		// content instead of refusing). fu.yaml is hand-editable today, and
		// a future clone/pull will populate it from a network source,
		// making this a genuine trust boundary, not a theoretical one.
		//
		// A failing name is collected into invalid and this entry is
		// skipped entirely -- not treated as a fatal, whole-file error
		// (round 4 finding 2, softening round 3 finding 2's own bdf2882,
		// which returned an error here instead): LoadConfig excludes every
		// name recorded in invalid from the config's skill set (SkillNames,
		// HasSkill), so it makes no difference to that exclusion whether
		// this entry's own internal shape (enabled/overrides) is well-formed
		// or not -- nothing will ever read it. See LoadConfig's doc comment
		// for the full reasoning and InvalidNames for how a caller reports
		// these.
		//
		// This still makes engine.Desired's and engine.Diff's own per-name
		// validation unreachable in production for any name arriving this
		// way -- store.Config's fields are unexported with no public
		// constructor other than LoadConfig, so a name excluded here never
		// reaches SkillNames() for the engine to iterate in the first
		// place. They stay in place as defence in depth: today's AddSkill
		// deliberately does not itself validate (NewSkill validates before
		// calling it, so a second check there would be redundant, and
		// store/config_test.go and internal/engine's own tests rely on
		// constructing a Config with a deliberately invalid name entirely
		// in memory, bypassing this file-parsing path, to exercise that
		// defence directly) -- so an invalid name can still reach
		// Diff/Desired if some future caller ever mutates a Config without
		// routing an untrusted name through skill.ValidateName first, the
		// way NewSkill already disciplines itself to do.
		if err := skill.ValidateName(skillName); err != nil {
			invalid = append(invalid, InvalidName{Name: skillName, Reason: err.Error()})
			continue
		}
		entry := skills.Content[i+1]
		if entry.Kind != yaml.MappingNode {
			return nil, malformedErr(base, "must be a mapping")
		}
		if err := checkUnambiguousKeys(entry, base); err != nil {
			return nil, err
		}
		if enabled := mapGet(entry, "enabled"); enabled != nil && !isBoolScalar(enabled) {
			return nil, malformedErr(base+".enabled", "must be a boolean")
		}
		overrides := mapGet(entry, "overrides")
		if overrides == nil {
			continue
		}
		if overrides.Kind != yaml.MappingNode {
			return nil, malformedErr(base+".overrides", "must be a mapping")
		}
		if err := checkUnambiguousKeys(overrides, base+".overrides"); err != nil {
			return nil, err
		}
		for j := 0; j+1 < len(overrides.Content); j += 2 {
			if !isBoolScalar(overrides.Content[j+1]) {
				return nil, malformedErr(base+".overrides."+overrides.Content[j].Value, "must be a boolean")
			}
		}
	}
	return invalid, nil
}

// isBoolScalar reports whether n is a genuine YAML boolean (the !!bool
// tag), as opposed to a plain string that merely looks like one, such
// as "yes"/"no" or a quoted "true".
func isBoolScalar(n *yaml.Node) bool {
	return n.Kind == yaml.ScalarNode && n.Tag == "!!bool"
}

// parseVersion decodes a version scalar as a non-negative integer,
// requiring the node to be a genuine YAML integer whose text is canonical.
// LoadConfig used to call fmt.Sscanf with its error discarded, so a value
// like "v2" left the version at SupportedVersion and let Save overwrite a
// config an older fu cannot necessarily read. Reporting the error instead
// makes such a value fail load like any other structural violation.
//
// Round 6 finding: decoding through yaml.v3's int conversion was still too
// permissive, because it silently truncates a float. "version: 1.5" became
// 1, so CheckWritable saw a supported version and let this build write a
// schema it has no reason to understand. The previous comment here noted
// the truncation and argued it "errs toward refusing to write" -- exactly
// backwards: truncation *lowers* the version, so it permits the write it
// was supposed to refuse. A quoted "1" is likewise refused: it is a string,
// and reading it as a version would mean guessing at the author's intent in
// the one place guessing is least affordable.
//
// The tag check is what rejects floats and strings (yaml.v3 resolves 1.5 to
// !!float and "1" to !!str); the text comparison additionally rejects
// noncanonical integer spellings such as "+1", so that what this build
// records and what it compares against are the same string.
func parseVersion(n *yaml.Node) (int, error) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!int" {
		return 0, fmt.Errorf("must be an integer, got %s", n.Tag)
	}
	var v int
	if err := n.Decode(&v); err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("must not be negative, got %d", v)
	}
	if strconv.Itoa(v) != n.Value {
		return 0, fmt.Errorf("must be written in canonical decimal form, got %q", n.Value)
	}
	return v, nil
}

// malformedErr names the offending path and wraps ErrMalformedConfig so
// callers can test with errors.Is regardless of the exact message text.
func malformedErr(path, reason string) error {
	return fmt.Errorf("%s %s: %w", path, reason, ErrMalformedConfig)
}

// ---- yaml mapping helpers ----

func mapGet(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mapSet(m *yaml.Node, key string, val *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = val
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key}, val)
}

func mapDel(m *yaml.Node, key string) {
	if m == nil {
		return
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

func scalar(v string) *yaml.Node { return &yaml.Node{Kind: yaml.ScalarNode, Value: v} }

func boolNode(b bool) *yaml.Node {
	n := scalar(fmt.Sprintf("%v", b))
	n.Tag = "!!bool"
	return n
}

// boolValue decodes a boolean scalar. LoadConfig's validateConfigTree
// guarantees, at load time, that every enabled/override value reaching
// here carries the !!bool tag, so Enabled, Override, and normalize all
// read through this single helper and can no longer disagree on how to
// interpret one: previously Enabled compared against the literal
// "false" while Override compared against the literal "true", so a
// hand-written case variant of a real YAML bool (e.g. "True"/"FALSE")
// was read inconsistently depending on which accessor was asked.
func boolValue(n *yaml.Node) bool {
	var b bool
	_ = n.Decode(&b)
	return b
}

func emptyMap() *yaml.Node { return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"} }

// ---- skill accessors ----

func (c *Config) skills() *yaml.Node {
	s := mapGet(c.root(), "skills")
	if s == nil {
		s = emptyMap()
		mapSet(c.root(), "skills", s)
	}
	return s
}

// SkillNames returns the names of every validly-named skill recorded in
// the config, in file order. A name LoadConfig found invalid (round 4
// finding 2; see InvalidNames) is excluded here rather than aborting the
// whole load.
func (c *Config) SkillNames() []string {
	s := c.skills()
	var names []string
	for i := 0; i+1 < len(s.Content); i += 2 {
		name := s.Content[i].Value
		if c.isInvalidName(name) {
			continue
		}
		names = append(names, name)
	}
	return names
}

// HasSkill reports whether name is already registered. A name LoadConfig
// found invalid (round 4 finding 2) is never reported present here, even
// though the underlying document still carries its raw entry untouched
// (see LoadConfig's doc comment) -- this is what keeps `fu show`'s own
// path-escape guard closed without show.go needing any code of its own:
// it already refuses any name HasSkill denies.
func (c *Config) HasSkill(name string) bool {
	return !c.isInvalidName(name) && mapGet(c.skills(), name) != nil
}

// isInvalidName reports whether name is one LoadConfig found invalid and
// excluded from the skill set (see InvalidNames). Always false for a
// Config with no such entries, in particular any Config built purely in
// memory via AddSkill.
func (c *Config) isInvalidName(name string) bool {
	for _, inv := range c.invalid {
		if inv.Name == name {
			return true
		}
	}
	return false
}

// AddSkill registers a skill with enabled=true and no overrides
// (install default, SPEC §4.1).
func (c *Config) AddSkill(name, digest string) error {
	if c.HasSkill(name) {
		return fmt.Errorf("skill %q already exists", name)
	}
	entry := emptyMap()
	mapSet(entry, "digest", scalar(digest))
	mapSet(entry, "enabled", boolNode(true))
	mapSet(c.skills(), name, entry)
	return nil
}

// RemoveSkill deletes a skill's entire entry, including any overrides.
func (c *Config) RemoveSkill(name string) { mapDel(c.skills(), name) }

// Digest returns the recorded content digest for name, or "" if the
// skill or its digest field is absent.
func (c *Config) Digest(name string) string {
	if d := mapGet(mapGet(c.skills(), name), "digest"); d != nil {
		return d.Value
	}
	return ""
}

// SetDigest updates the recorded content digest of an existing skill.
// It is a no-op if the skill is not registered.
func (c *Config) SetDigest(name, digest string) {
	if entry := mapGet(c.skills(), name); entry != nil {
		mapSet(entry, "digest", scalar(digest))
	}
}

// Enabled returns the global switch (absent means true).
func (c *Config) Enabled(name string) bool {
	e := mapGet(mapGet(c.skills(), name), "enabled")
	return e == nil || boolValue(e)
}

// SetEnabled sets the global switch for name (SPEC §4.1). It is a no-op
// if the skill is not registered.
//
// Existing overrides are left completely untouched, even one that becomes
// equal to the new global value as a result (round 3 finding 4, reversing
// this build's own prior behavior -- normalize used to be called from
// here too). SPEC §4.1 contains two rules that contradict each other for
// exactly this case: same-value normalization applies after "any" switch
// write, but a global toggle is also said to leave overrides alone. This
// build resolves the contradiction in favor of the second rule: an
// override is a user's explicit, one-agent decision, and a global toggle
// is a completely unrelated write -- letting it silently delete that
// decision just because the two values happened to end up equal is a
// destructive side effect the user never asked for and, on a machine
// where that agent is not even installed, cannot see coming or verify
// after the fact. Two ordinary commands (an override, then a global flip
// to the same value) used to erase it permanently and invisibly; now the
// only cost is that fu.yaml may carry an override equal to the current
// global value, and `fu list`'s override marker may appear on an agent
// that is, for now, in fact following the global -- both purely cosmetic,
// and the effective on/off matrix is decidable exactly the same way
// either way. See normalize's own doc comment for where normalization
// still happens.
func (c *Config) SetEnabled(name string, on bool) {
	entry := mapGet(c.skills(), name)
	if entry == nil {
		return
	}
	mapSet(entry, "enabled", boolNode(on))
}

// Override returns (value, present) for an agent-level override.
func (c *Config) Override(name, agent string) (bool, bool) {
	v := mapGet(mapGet(mapGet(c.skills(), name), "overrides"), agent)
	if v == nil {
		return false, false
	}
	return boolValue(v), true
}

// SetAgent sets one agent's effective switch. Same-value normalization: a
// value equal to global is stored as "no override" (SPEC §4.1). This is
// the only setter that still normalizes (round 3 finding 4): a global
// toggle (SetEnabled) no longer does, so an agent-level write remains the
// only way an override is ever cleared -- see SetEnabled's doc comment for
// why.
//
// Normalization here touches only the single (name, agent) key this call
// was asked to write, never any other agent's override on the same skill
// (round 4 finding 1, reversing this build's own prior code): a previous
// version routed this through a shared normalize(entry, global) helper
// that walked *every* override recorded on the skill and deleted each one
// found equal to global, on the theory that reusing SetEnabled's old
// helper was simpler than a direct delete. But by the time this build
// stopped SetEnabled from normalizing at all (round 3 finding 4), a
// redundant override -- one equal to the current global -- became a
// state that can legitimately persist in fu.yaml indefinitely (that is
// the whole point of that fix), so that walk was no longer bounded to
// entries this call itself just made redundant: it deleted *any* override
// on the skill that happened to already equal the global, including ones
// belonging to agents never mentioned in this call at all. Four ordinary
// commands reproduced it: `disable alpha --agent codex`, `disable alpha`
// (global, correctly preserving codex's override per round 3 finding 4),
// `disable alpha --agent claude` -- which silently deleted codex's
// override too, purely because claude's own new value happened to match
// the global -- then `enable alpha` materialized codex's link again, for
// an agent the user had explicitly, and by this point only apparently,
// disabled.
func (c *Config) SetAgent(name, agent string, on bool) {
	entry := mapGet(c.skills(), name)
	if entry == nil {
		return
	}
	if on == c.Enabled(name) {
		// Same-value normalization: delete only this agent's own override
		// key, then drop the overrides map itself if that was the last one
		// left, so an empty `overrides: {}` never lingers in fu.yaml.
		if ov := mapGet(entry, "overrides"); ov != nil {
			mapDel(ov, agent)
			if len(ov.Content) == 0 {
				mapDel(entry, "overrides")
			}
		}
		return
	}
	ov := mapGet(entry, "overrides")
	if ov == nil {
		ov = emptyMap()
		mapSet(entry, "overrides", ov)
	}
	mapSet(ov, agent, boolNode(on))
}

// Effective resolves one agent's switch: override wins, else global.
func (c *Config) Effective(name, agent string) bool {
	if v, ok := c.Override(name, agent); ok {
		return v
	}
	return c.Enabled(name)
}
