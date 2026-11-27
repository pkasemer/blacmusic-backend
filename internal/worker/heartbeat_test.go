package worker

import (
	"context"
	"testing"
	"time"

	"github.com/blacmusic/backend/internal/upload"
)

// held makes a job last as long as a test needs: the download waits until release is closed.
type held struct {
	Blobs
	started, release chan struct{}
}

func (h held) Download(ctx context.Context, key, dst string) error {
	close(h.started)
	select {
	case <-h.release:
		return h.Blobs.Download(ctx, key, dst)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// hold starts handling job and returns once it is in the middle of it.
func (e *env) hold(job *upload.Upload) (h held, done chan struct{}) {
	e.t.Helper()
	h = held{Blobs: e.blobs, started: make(chan struct{}), release: make(chan struct{})}
	e.w.blobs, e.w.cfg.HeartbeatEvery = h, 50*time.Millisecond
	done = make(chan struct{})
	go func() {
		defer close(done)
		e.w.handle(context.Background(), job)
	}()
	select {
	case <-h.started:
	case <-time.After(10 * time.Second):
		e.t.Fatal("the job never started")
	}
	return h, done
}

func wait(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("the worker did not let go of the job")
	}
}

func (e *env) restart(id string) {
	e.t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		e.t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	if err := upload.Restart(ctx, tx, id); err != nil {
		e.t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		e.t.Fatal(err)
	}
}

func TestARunningJobKeepsSayingSo(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	job := e.submit("", tone(t, 2, 2))
	h, done := e.hold(job)

	// make it look long silent: the worker must put that right by itself
	if _, err := e.pool.Exec(ctx, `UPDATE uploads SET locked_at = now() - interval '1 hour' WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}
	fresh := false
	for deadline := time.Now().Add(5 * time.Second); !fresh && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if err := e.pool.QueryRow(ctx, `SELECT locked_at > now() - interval '10 seconds' FROM uploads WHERE id = $1`, job.ID).Scan(&fresh); err != nil {
			t.Fatal(err)
		}
	}
	if !fresh {
		t.Fatal("no still-working signal while the job was running")
	}
	close(h.release)
	wait(t, done)
	if st, _ := e.store.Get(ctx, job.ID); st.Status != upload.StatusReady {
		t.Fatalf("status = %s, want ready", st.Status)
	}
}

func TestAWorkerDropsAJobThatWasRestarted(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	job := e.submit("", tone(t, 2, 2))
	job.Attempts = e.w.cfg.MaxAttempts // the worst case: a failure of the old try would fail the job and delete its file
	_, done := e.hold(job)

	e.restart(job.ID)
	wait(t, done) // the next signal tells the worker the job is no longer its own

	st, err := e.store.Get(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != upload.StatusQueued || st.Attempts != 0 || st.Error != "" {
		t.Fatalf("status=%s attempts=%d error=%q, want it still waiting", st.Status, st.Attempts, st.Error)
	}
	if _, ok := e.blobs.Get(job.OriginalKey); !ok {
		t.Fatal("the old try deleted the restarted job's file")
	}
}

func TestOnlyTheLatestClaimCanFinish(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	old := e.submit("", []byte("x"))
	e.restart(old.ID)
	again := e.claim(old.ID)
	if again.Attempts != 1 {
		t.Fatalf("the restarted job was not claimed with all its tries: %+v", again)
	}
	if ok, err := e.store.Heartbeat(ctx, old); ok || err != nil {
		t.Fatalf("old claim's signal = %v %v, want refused", ok, err)
	}
	if ok, err := e.store.Heartbeat(ctx, again); !ok || err != nil {
		t.Fatalf("new claim's signal = %v %v", ok, err)
	}
	res := upload.Result{DurationMs: 1000, Renditions: []upload.Rendition{{Quality: "medium", MediaKey: "tracks/x/medium.m4a", BitrateKbps: 128, SizeBytes: 1}}}
	if err := e.store.Finish(ctx, old, res); err == nil {
		t.Fatal("the old claim must not finish the job")
	}
	if over, _ := e.store.TakenOver(ctx, old); !over {
		t.Fatal("the old claim should know the job was taken over")
	}
	if over, _ := e.store.TakenOver(ctx, again); over {
		t.Fatal("the new claim still has the job")
	}
}
