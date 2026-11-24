package fs

import (
	"fmt"
	"os"
	"path/filepath"
)

// OS call hooks that tests may replace (white-box tests only; production code does
// not use them)
var (
	osSyncFile  = func(f *os.File) error { return f.Sync() }
	osCloseFile = func(f *os.File) error { return f.Close() }
	osChmod     = func(name string, mode os.FileMode) error { return os.Chmod(name, mode) }
	osRename    = func(oldpath, newpath string) error { return os.Rename(oldpath, newpath) }
	osOpenDir   = func(dir string) (*os.File, error) { return os.Open(dir) } // #nosec G304 -- caller-validated directory path for fsync
)

// AtomicWriteFile atomically writes data to path with permission perm.
//
// Steps:
//  1. Create a temp file in path's parent directory (.<name>.tmp.*);
//  2. Write the data and call f.Sync() to flush it to disk;
//  3. After Close, replace the target path atomically with os.Rename;
//  4. fsync the parent directory once more - rename only guarantees data atomicity;
//     if the parent directory entry is not flushed yet, the new file may disappear
//     from the directory after a power loss, and the dir fsync makes the directory
//     entry itself durable too.
//
// Any failing step cleans up the temp file, leaving no garbage behind.
// CreateTemp defaults to mode 0600, so only an explicit Chmod makes perm take effect.
func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	name := filepath.Base(path)

	// 1. Create a temp file in the same directory, prefixed like ".<name>.tmp."
	tmp, err := os.CreateTemp(dir, "."+name+".tmp.")
	if err != nil {
		return fmt.Errorf("fs.AtomicWriteFile: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	// cleanup removes the temp file on any failure path
	cleanup := func() { _ = os.Remove(tmpPath) }

	// 2. Write the data
	if _, err = tmp.Write(data); err != nil {
		_ = osCloseFile(tmp)
		cleanup()
		return fmt.Errorf("fs.AtomicWriteFile: write: %w", err)
	}

	// 3. Sync: flush the data from the OS buffer to the device
	if err = osSyncFile(tmp); err != nil {
		_ = osCloseFile(tmp)
		cleanup()
		return fmt.Errorf("fs.AtomicWriteFile: sync: %w", err)
	}

	if err = osCloseFile(tmp); err != nil {
		cleanup()
		return fmt.Errorf("fs.AtomicWriteFile: close: %w", err)
	}

	// 4. Set the real permission (CreateTemp is fixed at 0600)
	if err = osChmod(tmpPath, perm); err != nil {
		cleanup()
		return fmt.Errorf("fs.AtomicWriteFile: chmod: %w", err)
	}

	// 5. Atomic rename
	if err = osRename(tmpPath, path); err != nil {
		cleanup()
		return fmt.Errorf("fs.AtomicWriteFile: rename: %w", err)
	}

	// 6. fsync the parent directory: rename itself is atomic, but if the parent
	//    directory entry is not durable yet, the new file may disappear from the
	//    directory after a system crash (i.e. the directory entry is lost). fsyncing
	//    the directory makes sure the directory entry change reaches the disk,
	//    completing the persistence of the rename.
	df, err := osOpenDir(dir)
	if err != nil {
		// The rename already succeeded; a dir-fsync failure is only reported, not rolled back
		return fmt.Errorf("fs.AtomicWriteFile: open dir for fsync: %w", err)
	}
	syncErr := osSyncFile(df)
	_ = osCloseFile(df)
	if syncErr != nil {
		return fmt.Errorf("fs.AtomicWriteFile: dir fsync: %w", syncErr)
	}

	return nil
}

// CreateExclusive atomically reserves a name under dir, with filenames like base+ext,
// base+" (1)"+ext ... base+" (999)"+ext.
//
// O_CREATE|O_EXCL lets the kernel guarantee atomicity, with no Stat/Exists pre-check at
// all (which prevents TOCTOU races).
// The caller gets an opened (O_WRONLY) file handle and is responsible for writing to it
// and closing it.
// When all 1000 candidate names already exist, a clear error is returned.
func CreateExclusive(dir, base, ext string) (path string, f *os.File, err error) {
	// Candidate 0: base+ext
	// Candidate n (n>=1): base+" (n)"+ext
	for i := range 1000 {
		var candidate string
		if i == 0 {
			candidate = filepath.Join(dir, base+ext)
		} else {
			candidate = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		}

		f, err = os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) // #nosec G304 -- candidate path constructed under caller-validated dir
		if err == nil {
			return candidate, f, nil
		}
		// Keep looping only on EEXIST (file already exists); return any other error immediately
		if !os.IsExist(err) {
			return "", nil, fmt.Errorf("fs.CreateExclusive: open %q: %w", candidate, err)
		}
	}
	return "", nil, fmt.Errorf("fs.CreateExclusive: all 1000 candidates exist in %q (base=%q ext=%q)", dir, base, ext)
}
