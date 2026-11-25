package store

import (
	"errors"
	"fmt"
	"io/fs"
	"syscall"
)

var (
	// ErrNotDatabase means the file exists but is not a SQLite database.
	ErrNotDatabase = errors.New("not a SQLite database")
	// ErrNewerSchema means the database was written by a newer build.
	ErrNewerSchema = errors.New("schema is newer than this build supports")
)

// SQLite primary result codes (the low byte of an extended code).
const (
	sqlitePerm     = 3
	sqliteBusy     = 5
	sqliteLocked   = 6
	sqliteReadOnly = 8
	sqliteCorrupt  = 11
	sqliteFull     = 13
	sqliteAuth     = 23
	sqliteNotADB   = 26
)

// Reason maps a store error to a short reason for the status line: "permission
// denied", "disk full", "locked", "newer schema", "not a database", "corrupt", or
// "error". SQLite's CORRUPT and NOTADB codes at runtime mean a damaged file
// ("corrupt"); a file that is no database at open is ErrNotDatabase (see atOpen).
// It looks only at typed errors (never at message text); nil gives "".
func Reason(err error) string {
	if err == nil {
		return ""
	}
	switch {
	case errors.Is(err, ErrNewerSchema):
		return "newer schema"
	case errors.Is(err, ErrNotDatabase):
		return "not a database"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	case errors.Is(err, syscall.ENOSPC):
		return "disk full"
	}
	var coded interface{ Code() int } // the driver's *sqlite.Error
	if errors.As(err, &coded) {
		switch coded.Code() & 0xff {
		case sqliteBusy, sqliteLocked:
			return "locked"
		case sqliteFull:
			return "disk full"
		case sqliteCorrupt, sqliteNotADB:
			return "corrupt"
		case sqlitePerm, sqliteReadOnly, sqliteAuth:
			return "permission denied"
		}
	}
	return "error"
}

// atOpen marks SQLite's NOTADB while opening as ErrNotDatabase: the file was never a
// usable database (at runtime the same code means it was damaged: "corrupt").
func atOpen(err error) error {
	var coded interface{ Code() int }
	if errors.As(err, &coded) && coded.Code()&0xff == sqliteNotADB {
		return fmt.Errorf("%w: %w", ErrNotDatabase, err)
	}
	return err
}
