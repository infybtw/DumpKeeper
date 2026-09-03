package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"dumpkeeper/internal/db"
	"dumpkeeper/internal/storage"
)

// TestPruneKeepsLastN: 5 completed backups with keep_last=2 must leave the
// 2 newest rows/files; older files must be gone from disk.
func TestPruneKeepsLastN(t *testing.T) {
	dir := t.TempDir()
	store, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	local := storage.NewLocal(filepath.Join(dir, "backups"))
	if err := os.MkdirAll(local.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	engine := New(store, local, nil)

	job, err := store.CreateJob(db.Job{
		Name: "prune-test", Host: "127.0.0.1", Port: 5432,
		Username: "u", Password: "p", DBName: "d", SSLMode: "prefer",
		DestLocal: true, KeepLast: 2,
	})
	if err != nil {
		t.Fatalf("create job: %v", err)
	}

	base := time.Now().UTC()
	for i := range 5 {
		filename := fmt.Sprintf("prune-test-%d.dump", i)
		if err := os.WriteFile(filepath.Join(local.Root, filename), []byte("dumpdata"), 0o644); err != nil {
			t.Fatal(err)
		}
		startedAt := db.FormatTime(base.Add(time.Duration(i) * time.Second))
		id, err := store.CreateBackup(job.ID, db.StatusCompleted, TriggerManual, startedAt, filename)
		if err != nil {
			t.Fatalf("create backup %d: %v", i, err)
		}
		// The engine flips stored_local via UpdateBackup once the file lands
		// in the local store; retention only deletes what is marked stored.
		if err := store.UpdateBackup(db.Backup{ID: id, Status: db.StatusCompleted, StoredLocal: true}); err != nil {
			t.Fatalf("mark backup %d stored: %v", i, err)
		}
	}

	engine.Prune(job)

	backups, err := store.ListBackups(job.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 2 {
		t.Fatalf("want 2 remaining backups, got %d", len(backups))
	}
	if backups[0].Filename != "prune-test-4.dump" || backups[1].Filename != "prune-test-3.dump" {
		t.Fatalf("wrong survivors: %q, %q", backups[0].Filename, backups[1].Filename)
	}
	for _, keep := range []string{"prune-test-3.dump", "prune-test-4.dump"} {
		if _, err := os.Stat(filepath.Join(local.Root, keep)); err != nil {
			t.Errorf("newest file %s should survive: %v", keep, err)
		}
	}
	for _, gone := range []string{"prune-test-0.dump", "prune-test-1.dump", "prune-test-2.dump"} {
		if _, err := os.Stat(filepath.Join(local.Root, gone)); !os.IsNotExist(err) {
			t.Errorf("old file %s should be deleted (stat err: %v)", gone, err)
		}
	}
}
