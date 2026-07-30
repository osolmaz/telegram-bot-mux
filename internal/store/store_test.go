package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/osolmaz/telegram-bot-mux/internal/routing"
)

func TestIngestFanoutIndependentOffsetsAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store := openTestStore(t, path, []string{"openclaw", "unyolo"}, 100, 10)
	updates := []routing.Update{testUpdate(t, 10, "message"), testUpdate(t, 11, "callback_query")}
	if err := store.Ingest(ctx, updates, func(routing.Update) []string { return []string{"openclaw", "unyolo"} }); err != nil {
		t.Fatal(err)
	}
	if offset, err := store.UpstreamOffset(ctx); err != nil || offset != 12 {
		t.Fatalf("offset = %d, err = %v", offset, err)
	}
	assertDeliveryIDs(t, mustGet(t, store, "openclaw", 0, 100), 10, 11)
	assertDeliveryIDs(t, mustGet(t, store, "unyolo", 0, 100), 10, 11)
	assertDeliveryIDs(t, mustGet(t, store, "openclaw", 11, 100), 11)
	if pending, _ := store.PendingCount(ctx, "openclaw"); pending != 1 {
		t.Fatalf("openclaw pending = %d", pending)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, []string{"openclaw", "unyolo"}, 100, 10)
	defer store.Close()
	assertDeliveryIDs(t, mustGet(t, store, "unyolo", 0, 100), 10, 11)
	if err := store.Ingest(ctx, updates, func(routing.Update) []string { return []string{"openclaw", "unyolo"} }); err != nil {
		t.Fatal(err)
	}
	assertDeliveryIDs(t, mustGet(t, store, "unyolo", 0, 100), 10, 11)
	if _, err := store.GetUpdates(ctx, "unyolo", 12, 100); err != nil {
		t.Fatal(err)
	}
	if pending, _ := store.PendingCount(ctx, "unyolo"); pending != 0 {
		t.Fatalf("unyolo pending = %d", pending)
	}
}

func TestIngestBacklogLimitRollsBackOffsetAndUpdates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), []string{"client"}, 1, 0)
	defer store.Close()
	err := store.Ingest(ctx, []routing.Update{testUpdate(t, 1, "message"), testUpdate(t, 2, "message")}, func(routing.Update) []string { return []string{"client"} })
	if !errors.Is(err, ErrBacklogFull) {
		t.Fatalf("Ingest error = %v", err)
	}
	if offset, _ := store.UpstreamOffset(ctx); offset != 0 {
		t.Fatalf("offset after rollback = %d", offset)
	}
	if got := mustGet(t, store, "client", 0, 100); len(got) != 0 {
		t.Fatalf("updates after rollback = %v", got)
	}
}

func TestNegativeOffsetForgetsEarlierUpdates(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), []string{"client"}, 10, 0)
	defer store.Close()
	updates := []routing.Update{testUpdate(t, 1, "message"), testUpdate(t, 2, "message"), testUpdate(t, 3, "message")}
	if err := store.Ingest(ctx, updates, func(routing.Update) []string { return []string{"client"} }); err != nil {
		t.Fatal(err)
	}
	assertDeliveryIDs(t, mustGet(t, store, "client", -2, 100), 2, 3)
	assertDeliveryIDs(t, mustGet(t, store, "client", 0, 100), 2, 3)
}

func TestRoutingCanLeaveUpdateWithoutDeliveries(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), []string{"client"}, 10, 0)
	defer store.Close()
	if err := store.Ingest(ctx, []routing.Update{testUpdate(t, 5, "message")}, func(routing.Update) []string { return nil }); err != nil {
		t.Fatal(err)
	}
	if offset, _ := store.UpstreamOffset(ctx); offset != 6 {
		t.Fatalf("offset = %d", offset)
	}
	if got := mustGet(t, store, "client", 0, 100); len(got) != 0 {
		t.Fatalf("unexpected deliveries: %v", got)
	}
}

func TestChangesSignalsAfterCommit(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), []string{"client"}, 10, 0)
	defer store.Close()
	changes := store.Changes()
	if err := store.Ingest(ctx, []routing.Update{testUpdate(t, 1, "message")}, func(routing.Update) []string { return []string{"client"} }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(time.Second):
		t.Fatal("changes channel was not signaled")
	}
}

func TestBackupIntegrityAndStaleClientCleanup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store := openTestStore(t, path, []string{"old", "kept"}, 10, 0)
	if err := store.Ingest(ctx, []routing.Update{testUpdate(t, 1, "message")}, func(routing.Update) []string { return []string{"old", "kept"} }); err != nil {
		t.Fatal(err)
	}
	if err := store.IntegrityCheck(ctx); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(dir, "backup.db")
	if err := store.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(backup); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("backup info = %v, err = %v", info, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store = openTestStore(t, path, []string{"kept", "new"}, 10, 0)
	defer store.Close()
	if _, err := store.GetUpdates(ctx, "old", 0, 100); err == nil {
		t.Fatal("removed client remained available")
	}
	assertDeliveryIDs(t, mustGet(t, store, "kept", 0, 100), 1)
	if got := mustGet(t, store, "new", 0, 100); len(got) != 0 {
		t.Fatalf("new client inherited history: %v", got)
	}
	if err := store.Backup(ctx, backup); err == nil {
		t.Fatal("backup overwrote an existing destination")
	}
}

func TestPrunesAcknowledgedUpdatesOutsideSafetyWindow(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "state.db"), []string{"client"}, 10, 1)
	defer store.Close()
	updates := []routing.Update{testUpdate(t, 1, "message"), testUpdate(t, 2, "message"), testUpdate(t, 3, "message")}
	if err := store.Ingest(ctx, updates, func(routing.Update) []string { return []string{"client"} }); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetUpdates(ctx, "client", 4, 100); err != nil {
		t.Fatal(err)
	}
	var ids []int64
	rows, err := store.db.QueryContext(ctx, `SELECT update_id FROM updates ORDER BY update_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if !reflect.DeepEqual(ids, []int64{2, 3}) {
		t.Fatalf("retained update ids = %v", ids)
	}
}

func openTestStore(t *testing.T, path string, clients []string, maxPending, safetyWindow int) *Store {
	t.Helper()
	store, err := Open(context.Background(), path, clients, maxPending, safetyWindow)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func testUpdate(t *testing.T, id int64, updateType string) routing.Update {
	t.Helper()
	var raw string
	if updateType == "callback_query" {
		raw = `{"update_id":` + strconv.FormatInt(id, 10) + `,"callback_query":{"data":"bk:test"}}`
	} else {
		raw = `{"update_id":` + strconv.FormatInt(id, 10) + `,"` + updateType + `":{"text":"test"}}`
	}
	update, err := routing.ParseUpdate(json.RawMessage(raw))
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func mustGet(t *testing.T, store *Store, client string, offset int64, limit int) []Delivery {
	t.Helper()
	updates, err := store.GetUpdates(context.Background(), client, offset, limit)
	if err != nil {
		t.Fatal(err)
	}
	return updates
}

func assertDeliveryIDs(t *testing.T, deliveries []Delivery, want ...int64) {
	t.Helper()
	got := make([]int64, 0, len(deliveries))
	for _, delivery := range deliveries {
		got = append(got, delivery.UpdateID)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery ids = %v, want %v", got, want)
	}
}
