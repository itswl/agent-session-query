package fixture

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Damage overwrites the first page of a table in the index file at path, which no process
// may have open: what opening the index reads is whole, and the first question that reads
// the table meets damage. words is the table a search reads, sessions the one every sync
// does.
func Damage(t testing.TB, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	var root, size int64
	err = db.QueryRow(`SELECT rootpage, page_size FROM sqlite_master, pragma_page_size() WHERE name = ?`, table).Scan(&root, &size)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt([]byte(strings.Repeat("\xff", int(size))), (root-1)*size); err != nil {
		t.Fatal(err)
	}
}
