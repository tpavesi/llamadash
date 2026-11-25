package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The comment lines that head the lines llamadash appends to a settings file;
// the installer writes the same lines.
const (
	scalesComment = "# Chart scales learned by llamadash (it raises these itself)."
	fansComment   = "# Fans llamadash has seen spinning (it adds to this itself; delete an entry to forget a fan)."
)

// confVal is one setting written back to a settings file: its value as written,
// and the comment line it is appended under when the file does not have it yet.
type confVal struct {
	key, val, comment string
}

// numVal is a learned chart top.
func numVal(key string, v float64) confVal { return confVal{key, fmtConfNum(v), scalesComment} }

// fansVal is the known fans line.
func fansVal(ids []string) confVal {
	return confVal{"LLAMADASH_KNOWN_FANS", strings.Join(ids, ","), fansComment}
}

// fmtConfNum writes a number plainly, without trailing zeros: 150, 87.5.
func fmtConfNum(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// confLineKey is the setting a line of a settings file sets, as parseConfig reads
// it; "" for a blank, comment or malformed line.
func confLineKey(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	k, _, ok := strings.Cut(line, "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(k)
}

// rewriteConf sets vals in the settings file data. Every other line stays byte for
// byte. A key's line (the last one, which is the one the parser uses) is replaced
// in place; missing keys are appended at the end, each under its comment line
// (written once, and not when the file has it already), with the file's own line
// ending (CRLF when its first line ends so).
func rewriteConf(data []byte, vals []confVal) []byte {
	type line struct{ text, eol string }
	var lines []line
	for s := string(data); s != ""; {
		i := strings.IndexByte(s, '\n')
		if i < 0 {
			lines = append(lines, line{s, ""})
			break
		}
		l := line{s[:i], "\n"}
		if strings.HasSuffix(l.text, "\r") {
			l = line{l.text[:len(l.text)-1], "\r\n"}
		}
		lines = append(lines, l)
		s = s[i+1:]
	}
	eol := "\n"
	if len(lines) > 0 && lines[0].eol != "" {
		eol = lines[0].eol
	}
	last := map[string]int{}
	hasLine := map[string]bool{}
	for i, l := range lines {
		if k := confLineKey(l.text); k != "" {
			last[k] = i
		}
		hasLine[strings.TrimSpace(l.text)] = true
	}
	var missing []confVal
	for _, v := range vals {
		if i, ok := last[v.key]; ok {
			lines[i].text = v.key + "=" + v.val
		} else {
			missing = append(missing, v)
		}
	}
	var b bytes.Buffer
	for _, l := range lines {
		b.WriteString(l.text + l.eol)
	}
	if len(missing) > 0 {
		if n := len(lines); n > 0 && lines[n-1].eol == "" {
			b.WriteString(eol)
		}
		for _, v := range missing {
			if v.comment != "" && !hasLine[v.comment] {
				b.WriteString(v.comment + eol)
				hasLine[v.comment] = true
			}
			b.WriteString(v.key + "=" + v.val + eol)
		}
	}
	return b.Bytes()
}

// Replaced in tests.
var (
	createTemp = os.CreateTemp
	renameFile = os.Rename
)

// saveConf sets vals in the settings file at path, which must exist: it reads the
// file again (so an edit made meanwhile is kept), writes the result to a temporary
// file in the same directory with the same permission bits (and, as root, the
// same owner), syncs it and renames it over the original, so the file is never
// missing or partly written. On any error the original is left as it was.
func saveConf(path string, vals []confVal) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	data, err := readFile(path)
	if err != nil {
		return err
	}
	out := rewriteConf(data, vals)
	if bytes.Equal(out, data) {
		return nil
	}
	f, err := createTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp, done := f.Name(), false
	defer func() {
		if !done {
			f.Close()
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(out); err != nil {
		return err
	}
	if err := keepOwner(f, fi); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return err
	}
	if err := renameFile(tmp, path); err != nil {
		return err
	}
	done = true
	syncDir(filepath.Dir(path))
	return nil
}
