package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"pmt/internal/store"
)

// TestBackupRestore dumps the test database through age and restores it into a
// scratch database. pg_dump and pg_restore run inside the dev database
// container (PMT_TEST_PGCONTAINER, default pmt-dev-db), so their version
// matches the server's.
func TestBackupRestore(t *testing.T) {
	dbURL := os.Getenv("PMT_TEST_DB")
	if dbURL == "" {
		t.Skip("PMT_TEST_DB not set")
	}
	container := os.Getenv("PMT_TEST_PGCONTAINER")
	if container == "" {
		container = "pmt-dev-db"
	}
	if err := exec.Command("docker", "inspect", container).Run(); err != nil {
		t.Skipf("container %s not available", container)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := store.Open(ctx, dbURL, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	c, err := ParseConn(dbURL)
	if err != nil {
		t.Fatal(err)
	}
	inContainer := func(c Conn) Conn { c.Host, c.Port = "127.0.0.1", "5432"; return c }
	tools := Tools{
		Dump:    []string{"docker", "exec", "-i", "-e", "PGPASSWORD", container, "pg_dump"},
		Restore: []string{"docker", "exec", "-i", "-e", "PGPASSWORD", container, "pg_restore"},
		Reach:   inContainer,
	}
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// An old dump that pruning should remove.
	old := filepath.Join(dir, "pmt-20200101T000000Z.dump.age")
	os.WriteFile(old, []byte("x"), 0o640)
	os.Chtimes(old, time.Now().AddDate(0, 0, -30), time.Now().AddDate(0, 0, -30))

	res, err := Run(ctx, pool, c, tools, id.Recipient().String(), dir, 14, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 1 {
		t.Errorf("pruned %d", res.Pruned)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("old dump kept")
	}
	if latest, _ := Latest(dir); latest != res.Path {
		t.Errorf("latest %s, want %s", latest, res.Path)
	}
	raw, _ := os.ReadFile(res.Path)
	if !strings.HasPrefix(string(raw), "age-encryption.org/v1") {
		t.Fatal("dump is not age-encrypted")
	}

	// The wrong key fails cleanly.
	other, _ := age.GenerateX25519Identity()
	if _, err := Restore(ctx, pool, c, tools, other.String(), res.Path, "pmt_restore_unit_bad", false); err == nil {
		t.Error("restore with the wrong identity succeeded")
	}

	rr, err := Restore(ctx, pool, c, tools, id.String(), res.Path, "pmt_restore_unit", false)
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rr.String())
	if len(rr.Counts) < 5 {
		t.Errorf("only %d tables restored", len(rr.Counts))
	}
	var exists bool
	pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = 'pmt_restore_unit')`).Scan(&exists)
	if exists {
		t.Error("scratch database not dropped")
	}
}

func TestParseConn(t *testing.T) {
	c, err := ParseConn("postgres://u:p%40ss@db:6543/pmt?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	if c.Host != "db" || c.Port != "6543" || c.User != "u" || c.Password != "p@ss" || c.Database != "pmt" || c.SSLMode != "disable" {
		t.Errorf("%+v", c)
	}
	for _, a := range c.args("") {
		if strings.Contains(a, "p@ss") {
			t.Error("password on the command line")
		}
	}
}
