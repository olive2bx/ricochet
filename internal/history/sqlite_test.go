//go:build sqlite

package history

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/olive2bx/ricochet/internal/collector"
)

func TestPersistAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	db, err := Open(path, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	col := collector.New(10)
	col.SetSink(db)
	for i := 0; i < 2; i++ {
		e := col.Start(collector.Event{Host: "a.com", Scheme: collector.SchemeHTTP})
		e.Done = true
		e.Total = 42 * time.Millisecond
		e.RespBody = []byte("body")
		col.Update(e)
	}
	col.Start(collector.Event{Host: "pending.com"}) // not done: not persisted
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db.Save(collector.Event{ID: 99, Done: true}) // after close: ignored, no panic
	db.Close()

	db, err = Open(path, 3, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	evs, err := db.Load(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 2 || evs[0].ID != 1 || evs[1].ID != 2 || evs[1].Total != 42*time.Millisecond || string(evs[1].RespBody) != "body" {
		t.Fatalf("loaded = %+v", evs)
	}

	col2 := collector.New(10)
	col2.SetSink(db)
	col2.Load(evs)
	if e := col2.Start(collector.Event{}); e.ID != 4 && e.ID != 3 {
		t.Errorf("next ID = %d", e.ID)
	}
	if err := col2.Clear(); err != nil {
		t.Fatal(err)
	}
	if evs, _ := db.Load(100); len(evs) != 0 {
		t.Errorf("%d rows after clear", len(evs))
	}
}

func TestPrune(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "h.db"), 5, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var batch []collector.Event
	for i := 1; i <= 12; i++ {
		batch = append(batch, collector.Event{ID: uint64(i), Done: true})
	}
	if err := db.write(batch); err != nil {
		t.Fatal(err)
	}
	db.prune()
	evs, err := db.Load(100)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 5 || evs[0].ID != 8 {
		t.Errorf("after prune: %d events, first %d", len(evs), evs[0].ID)
	}
}
