//go:build !solution

package storage

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"gitlab.com/slon/shad-go/Exam-1-QuizBot/quizbot/internal/quiz"
)

var ErrNotFound = errors.New("not found")

type MemoryStorage struct {
	mu sync.RWMutex

	quizzes map[string]*quiz.Quiz
	runs    map[string]*quiz.QuizRun

	quizzesByOwner map[int64]map[string]struct{}
	runsByQuiz     map[string]map[string]struct{}
}

func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		quizzes:        make(map[string]*quiz.Quiz),
		runs:           make(map[string]*quiz.QuizRun),
		quizzesByOwner: make(map[int64]map[string]struct{}),
		runsByQuiz:     make(map[string]map[string]struct{}),
	}
}

func (s *MemoryStorage) SaveQuiz(ctx context.Context, q *quiz.Quiz) error {
	_ = ctx

	if q == nil {
		return errors.New("nil quiz")
	}

	if q.ID == "" {
		return errors.New("quiz id is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cp := cloneQuiz(q)
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = time.Now()
	}

	s.quizzes[cp.ID] = cp

	if _, ok := s.quizzesByOwner[cp.OwnerID]; !ok {
		s.quizzesByOwner[cp.OwnerID] = make(map[string]struct{})
	}

	s.quizzesByOwner[cp.OwnerID][cp.ID] = struct{}{}

	return nil
}

func (s *MemoryStorage) GetQuiz(ctx context.Context, id string) (*quiz.Quiz, error) {
	_ = ctx

	s.mu.RLock()
	q := s.quizzes[id]
	s.mu.RUnlock()

	if q == nil {
		return nil, ErrNotFound
	}

	return cloneQuiz(q), nil
}

func (s *MemoryStorage) ListQuizzes(ctx context.Context, ownerID int64) ([]*quiz.Quiz, error) {
	_ = ctx

	s.mu.RLock()
	set := s.quizzesByOwner[ownerID]
	s.mu.RUnlock()

	if len(set) == 0 {
		return nil, nil
	}

	out := make([]*quiz.Quiz, 0, len(set))

	s.mu.RLock()

	for id := range set {
		if q := s.quizzes[id]; q != nil {
			out = append(out, cloneQuiz(q))
		}
	}

	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})

	return out, nil
}

func (s *MemoryStorage) DeleteQuiz(ctx context.Context, id string) error {
	_ = ctx

	s.mu.Lock()
	defer s.mu.Unlock()

	q := s.quizzes[id]
	if q == nil {
		return ErrNotFound
	}

	delete(s.quizzes, id)

	if set := s.quizzesByOwner[q.OwnerID]; set != nil {
		delete(set, id)

		if len(set) == 0 {
			delete(s.quizzesByOwner, q.OwnerID)
		}
	}

	if runSet := s.runsByQuiz[id]; runSet != nil {
		for runID := range runSet {
			delete(s.runs, runID)
		}

		delete(s.runsByQuiz, id)
	}

	return nil
}

func (s *MemoryStorage) SaveRun(ctx context.Context, run *quiz.QuizRun) error {
	_ = ctx

	if run == nil {
		return errors.New("nil run")
	}

	if run.ID == "" {
		return errors.New("run id is empty")
	}

	if run.QuizID == "" {
		return errors.New("quiz id is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	cp := cloneRun(run)
	s.runs[cp.ID] = cp

	if _, ok := s.runsByQuiz[cp.QuizID]; !ok {
		s.runsByQuiz[cp.QuizID] = make(map[string]struct{})
	}

	s.runsByQuiz[cp.QuizID][cp.ID] = struct{}{}

	return nil
}

func (s *MemoryStorage) GetRun(ctx context.Context, id string) (*quiz.QuizRun, error) {
	_ = ctx

	s.mu.RLock()
	run := s.runs[id]
	s.mu.RUnlock()

	if run == nil {
		return nil, ErrNotFound
	}

	return cloneRun(run), nil
}

func (s *MemoryStorage) ListRuns(ctx context.Context, quizID string) ([]*quiz.QuizRun, error) {
	_ = ctx

	s.mu.RLock()
	set := s.runsByQuiz[quizID]
	s.mu.RUnlock()

	if len(set) == 0 {
		return nil, nil
	}

	out := make([]*quiz.QuizRun, 0, len(set))

	s.mu.RLock()

	for id := range set {
		if r := s.runs[id]; r != nil {
			out = append(out, cloneRun(r))
		}
	}

	s.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		ai := out[i].StartedAt

		aj := out[j].StartedAt
		if ai.IsZero() && !aj.IsZero() {
			return false
		}

		if !ai.IsZero() && aj.IsZero() {
			return true
		}

		return ai.After(aj)
	})

	return out, nil
}

func (s *MemoryStorage) UpdateRun(ctx context.Context, run *quiz.QuizRun) error {
	_ = ctx

	if run == nil {
		return errors.New("nil run")
	}

	if run.ID == "" {
		return errors.New("run id is empty")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.runs[run.ID] == nil {
		return ErrNotFound
	}

	cp := cloneRun(run)
	s.runs[cp.ID] = cp

	if _, ok := s.runsByQuiz[cp.QuizID]; !ok {
		s.runsByQuiz[cp.QuizID] = make(map[string]struct{})
	}

	s.runsByQuiz[cp.QuizID][cp.ID] = struct{}{}

	return nil
}

func cloneQuiz(q *quiz.Quiz) *quiz.Quiz {
	if q == nil {
		return nil
	}

	cp := *q
	cp.Questions = make([]quiz.Question, len(q.Questions))
	copy(cp.Questions, q.Questions)

	return &cp
}

func cloneParticipant(p *quiz.Participant) *quiz.Participant {
	if p == nil {
		return nil
	}

	cp := *p
	if p.RegData != nil {
		cp.RegData = make(map[string]string, len(p.RegData))
		for k, v := range p.RegData {
			cp.RegData[k] = v
		}
	} else {
		cp.RegData = make(map[string]string)
	}

	return &cp
}

func cloneRun(r *quiz.QuizRun) *quiz.QuizRun {
	if r == nil {
		return nil
	}

	cp := *r

	cp.Participants = make(map[int64]*quiz.Participant, len(r.Participants))
	for id, p := range r.Participants {
		cp.Participants[id] = cloneParticipant(p)
	}

	cp.Answers = make(map[int64][]quiz.Answer, len(r.Answers))
	for id, ans := range r.Answers {
		dst := make([]quiz.Answer, len(ans))
		copy(dst, ans)
		cp.Answers[id] = dst
	}

	return &cp
}
