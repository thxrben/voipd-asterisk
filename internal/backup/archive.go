package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Manifest is the first file of a full backup.
type Manifest struct {
	Format     string    `json:"format"` // "voipd-backup"
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"created_at"`
	Schema     int64     `json:"schema"`
	Calls      int64     `json:"calls"`
	Voicemails int64     `json:"voicemails"`
	Files      int       `json:"files"` // voicemail directory files
}

const (
	manifestName = "manifest.json"
	dbName       = "voipd.db"
	vmPrefix     = "voicemail/"
	archiveVer   = 1

	// Restore staging, see Stage and ApplyPending.
	pendingDir   = "restore-pending"
	readyMarker  = "READY"
	preRestore   = "pre-restore"
	vmPending    = ".restore-pending"
	vmPreRestore = ".pre-restore"
)

// skipVM reports voicemail paths that are not part of a backup.
func skipVM(rel string) bool {
	first, _, _ := strings.Cut(rel, "/")
	return first == vmPending || first == vmPreRestore || strings.HasSuffix(rel, ".upload")
}

// Create writes an encrypted full backup of the database and the voicemail
// directory to w. tmpDir holds the database snapshot while it is written.
func Create(ctx context.Context, db *sql.DB, vmDir, tmpDir, passphrase string, w io.Writer) (Manifest, error) {
	if passphrase == "" {
		return Manifest{}, ErrPassphrase
	}
	snap, err := os.CreateTemp(tmpDir, ".snapshot-*.db")
	if err != nil {
		return Manifest{}, err
	}
	snapPath := snap.Name()
	snap.Close()
	os.Remove(snapPath) // VACUUM INTO wants to create the file
	defer os.Remove(snapPath)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, snapPath); err != nil {
		return Manifest{}, fmt.Errorf("database snapshot: %w", err)
	}

	m := Manifest{Format: "voipd-backup", Version: archiveVer, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if m.Schema, err = schemaVersion(ctx, db); err != nil {
		return Manifest{}, err
	}
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM calls`).Scan(&m.Calls)
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM voicemails`).Scan(&m.Voicemails)

	var vmFiles []string
	if vmDir != "" {
		err := filepath.WalkDir(vmDir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			rel, _ := filepath.Rel(vmDir, p)
			rel = filepath.ToSlash(rel)
			if rel == "." {
				return nil
			}
			if skipVM(rel) {
				if e.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			vmFiles = append(vmFiles, rel)
			return nil
		})
		if err != nil {
			return Manifest{}, fmt.Errorf("voicemail: %w", err)
		}
	}
	for _, f := range vmFiles {
		if fi, err := os.Stat(filepath.Join(vmDir, f)); err == nil && fi.Mode().IsRegular() {
			m.Files++
		}
	}

	enc, err := Encrypt(w, passphrase)
	if err != nil {
		return Manifest{}, err
	}
	gz := gzip.NewWriter(enc)
	tw := tar.NewWriter(gz)
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: manifestName, Mode: 0o644, Size: int64(len(mb)), ModTime: m.CreatedAt}); err != nil {
		return Manifest{}, err
	}
	if _, err := tw.Write(mb); err != nil {
		return Manifest{}, err
	}
	if err := addFile(tw, snapPath, dbName); err != nil {
		return Manifest{}, err
	}
	for _, rel := range vmFiles {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if err := addFile(tw, filepath.Join(vmDir, rel), vmPrefix+rel); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // deleted meanwhile
			}
			return Manifest{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return Manifest{}, err
	}
	if err := gz.Close(); err != nil {
		return Manifest{}, err
	}
	return m, enc.Close()
}

func addFile(tw *tar.Writer, src, name string) error {
	fi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case fi.IsDir():
		return tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: name + "/", Mode: int64(fi.Mode().Perm()), ModTime: fi.ModTime()})
	case !fi.Mode().IsRegular():
		return nil // no links or devices
	}
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: int64(fi.Mode().Perm()), Size: fi.Size(), ModTime: fi.ModTime()}); err != nil {
		return err
	}
	_, err = io.CopyN(tw, f, fi.Size())
	return err
}

// Stage decrypts and checks a full backup and unpacks it next to the live
// data: the database into <dataDir>/restore-pending, the voicemail into
// <vmDir>/.restore-pending. ApplyPending swaps them in at the next start.
// maxSchema is the newest schema this voipd understands.
func Stage(ctx context.Context, r io.Reader, passphrase, dataDir, vmDir string, maxSchema int64) (Manifest, error) {
	plain, err := Decrypt(r, passphrase)
	if err != nil {
		return Manifest{}, err
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	stageDB := filepath.Join(dataDir, pendingDir)
	stageVM := filepath.Join(vmDir, vmPending)
	for _, d := range []string{stageDB, stageVM} {
		if err := os.RemoveAll(d); err != nil {
			return Manifest{}, err
		}
	}
	ok := false
	defer func() {
		if !ok {
			os.RemoveAll(stageDB)
			os.RemoveAll(stageVM)
		}
	}()
	if err := os.MkdirAll(stageDB, 0o750); err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(stageVM, 0o2775); err != nil {
		return Manifest{}, err
	}

	tr := tar.NewReader(gz)
	var m Manifest
	haveDB := false
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("%w: %v", ErrFormat, err)
		}
		if i == 0 {
			if h.Name != manifestName {
				return Manifest{}, ErrFormat
			}
			if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&m); err != nil || m.Format != "voipd-backup" {
				return Manifest{}, ErrFormat
			}
			if m.Version > archiveVer || m.Schema > maxSchema {
				return Manifest{}, fmt.Errorf("the backup was made by a newer voipd (schema %d); update voipd first", m.Schema)
			}
			continue
		}
		switch {
		case h.Name == dbName:
			if err := writeFile(filepath.Join(stageDB, dbName), tr, 0o640); err != nil {
				return Manifest{}, err
			}
			haveDB = true
		case strings.HasPrefix(h.Name, vmPrefix):
			rel := strings.TrimPrefix(h.Name, vmPrefix)
			clean := path.Clean("/" + rel)[1:]
			if clean == "" || clean != strings.TrimSuffix(rel, "/") || skipVM(clean) {
				return Manifest{}, fmt.Errorf("%w: bad path %q", ErrFormat, h.Name)
			}
			dst := filepath.Join(stageVM, filepath.FromSlash(clean))
			switch h.Typeflag {
			case tar.TypeDir:
				if err := os.MkdirAll(dst, 0o2775); err != nil {
					return Manifest{}, err
				}
			case tar.TypeReg:
				if err := os.MkdirAll(filepath.Dir(dst), 0o2775); err != nil {
					return Manifest{}, err
				}
				if err := writeFile(dst, tr, fs.FileMode(h.Mode).Perm()|0o640); err != nil {
					return Manifest{}, err
				}
			}
		default:
			return Manifest{}, fmt.Errorf("%w: unexpected file %q", ErrFormat, h.Name)
		}
	}
	if !haveDB {
		return Manifest{}, fmt.Errorf("%w: no database in the backup", ErrFormat)
	}
	if err := checkDB(ctx, filepath.Join(stageDB, dbName)); err != nil {
		return Manifest{}, err
	}
	if err := os.WriteFile(filepath.Join(stageDB, readyMarker), nil, 0o640); err != nil {
		return Manifest{}, err
	}
	ok = true
	return m, nil
}

func writeFile(dst string, r io.Reader, mode fs.FileMode) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func checkDB(ctx context.Context, p string) error {
	db, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		return fmt.Errorf("%w: the database in the backup is damaged (%v %s)", ErrFormat, err, res)
	}
	if _, err := schemaVersion(ctx, db); err != nil {
		return fmt.Errorf("%w: not a voipd database", ErrFormat)
	}
	return nil
}

// Pending reports whether a staged restore waits for the next start.
func Pending(dataDir string) bool {
	_, err := os.Stat(filepath.Join(dataDir, pendingDir, readyMarker))
	return err == nil
}

// ApplyPending swaps a staged restore in; call it before opening the
// database. The replaced database and voicemail are kept in
// <dataDir>/pre-restore and <vmDir>/.pre-restore until the next restore.
func ApplyPending(dbPath, vmDir string) (bool, error) {
	dataDir := filepath.Dir(dbPath)
	if !Pending(dataDir) {
		return false, nil
	}
	stageDB := filepath.Join(dataDir, pendingDir)
	old := filepath.Join(dataDir, preRestore)
	if err := os.RemoveAll(old); err != nil {
		return false, err
	}
	if err := os.MkdirAll(old, 0o750); err != nil {
		return false, err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		err := os.Rename(dbPath+suffix, filepath.Join(old, filepath.Base(dbPath)+suffix))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	if err := os.Rename(filepath.Join(stageDB, dbName), dbPath); err != nil {
		return false, err
	}

	if vmDir != "" {
		stageVM := filepath.Join(vmDir, vmPending)
		oldVM := filepath.Join(vmDir, vmPreRestore)
		if err := os.RemoveAll(oldVM); err != nil {
			return false, err
		}
		if err := os.MkdirAll(oldVM, 0o2775); err != nil {
			return false, err
		}
		entries, err := os.ReadDir(vmDir)
		if err != nil {
			return false, err
		}
		for _, e := range entries {
			if e.Name() == vmPending || e.Name() == vmPreRestore {
				continue
			}
			if err := os.Rename(filepath.Join(vmDir, e.Name()), filepath.Join(oldVM, e.Name())); err != nil {
				return false, err
			}
		}
		staged, err := os.ReadDir(stageVM)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		for _, e := range staged {
			if err := os.Rename(filepath.Join(stageVM, e.Name()), filepath.Join(vmDir, e.Name())); err != nil {
				return false, err
			}
		}
		if err := os.RemoveAll(stageVM); err != nil {
			return false, err
		}
	}
	return true, os.RemoveAll(stageDB)
}
