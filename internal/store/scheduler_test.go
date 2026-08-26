package store

import (
	"path/filepath"
	"testing"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func TestSaveSchedulerTaskRollsBackOnPersistenceFailure(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	originalPath := state.path
	first, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "account_1", Status: domain.SchedulerTaskWaiting, Target: 750,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := state.Snapshot()
	state.path = t.TempDir() // Renaming a temporary state file over a directory must fail.
	if _, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "account_2", Status: domain.SchedulerTaskWaiting, Target: 750,
	}); err == nil {
		t.Fatal("expected scheduler task persistence failure")
	}
	state.path = originalPath
	after := state.Snapshot()
	if len(after.SchedulerTasks) != 1 || after.SchedulerTasks[0].ID != first.ID || after.NextID != before.NextID || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("failed insert changed in-memory scheduler state: before=%+v after=%+v", before, after)
	}
}

func TestUpdateSchedulerTaskRollsBackOnPersistenceFailure(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "account_1", Status: domain.SchedulerTaskWaiting, Target: 750,
		Events: []domain.SchedulerTaskEvent{{ID: 1, Type: "started", Message: "started"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := state.SchedulerTask("account_1")
	originalPath := state.path
	state.path = t.TempDir()
	if _, err := state.UpdateSchedulerTask("account_1", func(task *domain.SchedulerTask) error {
		task.Status = domain.SchedulerTaskRunning
		task.Events = append(task.Events, domain.SchedulerTaskEvent{ID: 2, Type: "round_started"})
		return nil
	}); err == nil {
		t.Fatal("expected scheduler task persistence failure")
	}
	state.path = originalPath
	after, _ := state.SchedulerTask("account_1")
	if after.Status != before.Status || len(after.Events) != len(before.Events) || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("failed update changed in-memory scheduler task: before=%+v after=%+v", before, after)
	}
}

func TestDeleteSchedulerTaskRollsBackOnPersistenceFailure(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	created, err := state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "account_1", Status: domain.SchedulerTaskPaused, Target: 750,
	})
	if err != nil {
		t.Fatal(err)
	}
	before := state.Snapshot()
	originalPath := state.path
	state.path = t.TempDir()
	if err := state.DeleteSchedulerTask("account_1"); err == nil {
		t.Fatal("expected scheduler task persistence failure")
	}
	state.path = originalPath
	after, exists := state.SchedulerTask("account_1")
	if !exists || after.ID != created.ID || !state.Snapshot().UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("failed delete changed in-memory scheduler state: task=%+v exists=%v", after, exists)
	}
}

func TestUpdateSchedulerTaskCallbackFailureDoesNotMutateEvents(t *testing.T) {
	state, err := Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().Add(-time.Minute)
	_, err = state.SaveSchedulerTask(domain.SchedulerTask{
		AccountID: "account_1", Status: domain.SchedulerTaskWaiting, Target: 750,
		CreatedAt: createdAt, Events: []domain.SchedulerTaskEvent{{ID: 1, Type: "started"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := state.SchedulerTask("account_1")
	sentinel := errSchedulerFixture
	if _, err := state.UpdateSchedulerTask("account_1", func(task *domain.SchedulerTask) error {
		task.Events[0].Type = "mutated"
		return sentinel
	}); err != sentinel {
		t.Fatalf("expected callback error, got %v", err)
	}
	after, _ := state.SchedulerTask("account_1")
	if after.Events[0].Type != before.Events[0].Type {
		t.Fatalf("callback error mutated stored events: before=%+v after=%+v", before.Events, after.Events)
	}
}

var errSchedulerFixture = schedulerFixtureError("scheduler fixture failure")

type schedulerFixtureError string

func (err schedulerFixtureError) Error() string { return string(err) }
