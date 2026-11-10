package naming

import (
	"os"

	"github.com/1368129224/tgshelf/internal/fs"
)

// UniquePath reserves a name under dir atomically: base+ext, base+" (1)"+ext ...
// base+" (999)"+ext.
//
// It reuses internal/fs.CreateExclusive directly (O_CREATE|O_EXCL, with the kernel
// guaranteeing atomicity), does not reimplement O_EXCL, and does no Stat pre-check
// (which avoids TOCTOU).
// The returned file is already open (O_WRONLY); the caller writes to it and closes it.
func UniquePath(dir, base, ext string) (string, *os.File, error) {
	return fs.CreateExclusive(dir, base, ext)
}
