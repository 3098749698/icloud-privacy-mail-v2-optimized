package store

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"icloud-privacy-mail-v2/internal/domain"
)

func (s *Store) SchedulerTasks() []domain.SchedulerTask {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tasks := cloneSchedulerTasks(s.state.SchedulerTasks)
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	return tasks
}

func (s *Store) SchedulerTask(accountID string) (domain.SchedulerTask, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	accountID = strings.TrimSpace(accountID)
	for _, task := range s.state.SchedulerTasks {
		if task.AccountID == accountID {
			return cloneSchedulerTask(task), true
		}
	}
	return domain.SchedulerTask{}, false
}

// SaveSchedulerTask inserts or replaces the unique task for an Apple account.
func (s *Store) SaveSchedulerTask(task domain.SchedulerTask) (domain.SchedulerTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task.AccountID = strings.TrimSpace(task.AccountID)
	if task.AccountID == "" {
		return domain.SchedulerTask{}, errors.New("调度任务缺少 Apple 账号")
	}
	previousTasks := cloneSchedulerTasks(s.state.SchedulerTasks)
	previousNextID := s.state.NextID
	previousUpdatedAt := s.state.UpdatedAt
	rollback := func() {
		s.state.SchedulerTasks = previousTasks
		s.state.NextID = previousNextID
		s.state.UpdatedAt = previousUpdatedAt
	}
	now := time.Now()
	for i, existing := range s.state.SchedulerTasks {
		if existing.AccountID != task.AccountID {
			continue
		}
		if strings.TrimSpace(task.ID) == "" {
			task.ID = existing.ID
		}
		if task.CreatedAt.IsZero() {
			task.CreatedAt = existing.CreatedAt
		}
		task.UpdatedAt = now
		s.state.SchedulerTasks[i] = cloneSchedulerTask(task)
		if err := s.saveLocked(); err != nil {
			rollback()
			return cloneSchedulerTask(task), err
		}
		return cloneSchedulerTask(task), nil
	}
	if strings.TrimSpace(task.ID) == "" {
		task.ID = s.nextIDLocked("schedule")
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = now
	}
	task.UpdatedAt = now
	s.state.SchedulerTasks = append(s.state.SchedulerTasks, cloneSchedulerTask(task))
	if err := s.saveLocked(); err != nil {
		rollback()
		return cloneSchedulerTask(task), err
	}
	return cloneSchedulerTask(task), nil
}

func (s *Store) UpdateSchedulerTask(accountID string, update func(*domain.SchedulerTask) error) (domain.SchedulerTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	for i := range s.state.SchedulerTasks {
		if s.state.SchedulerTasks[i].AccountID != accountID {
			continue
		}
		next := cloneSchedulerTask(s.state.SchedulerTasks[i])
		if err := update(&next); err != nil {
			return domain.SchedulerTask{}, err
		}
		next.AccountID = accountID
		next.UpdatedAt = time.Now()
		previous := cloneSchedulerTask(s.state.SchedulerTasks[i])
		previousUpdatedAt := s.state.UpdatedAt
		s.state.SchedulerTasks[i] = cloneSchedulerTask(next)
		if err := s.saveLocked(); err != nil {
			s.state.SchedulerTasks[i] = previous
			s.state.UpdatedAt = previousUpdatedAt
			return cloneSchedulerTask(next), err
		}
		return cloneSchedulerTask(next), nil
	}
	return domain.SchedulerTask{}, errors.New("账号创建任务不存在")
}

func (s *Store) DeleteSchedulerTask(accountID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	accountID = strings.TrimSpace(accountID)
	for i, task := range s.state.SchedulerTasks {
		if task.AccountID != accountID {
			continue
		}
		previousTasks := cloneSchedulerTasks(s.state.SchedulerTasks)
		previousUpdatedAt := s.state.UpdatedAt
		s.state.SchedulerTasks = append(s.state.SchedulerTasks[:i], s.state.SchedulerTasks[i+1:]...)
		if err := s.saveLocked(); err != nil {
			s.state.SchedulerTasks = previousTasks
			s.state.UpdatedAt = previousUpdatedAt
			return err
		}
		return nil
	}
	return errors.New("账号创建任务不存在")
}

func cloneSchedulerTasks(tasks []domain.SchedulerTask) []domain.SchedulerTask {
	data, _ := json.Marshal(tasks)
	var out []domain.SchedulerTask
	_ = json.Unmarshal(data, &out)
	return out
}

func cloneSchedulerTask(task domain.SchedulerTask) domain.SchedulerTask {
	task.Events = append([]domain.SchedulerTaskEvent(nil), task.Events...)
	return task
}
