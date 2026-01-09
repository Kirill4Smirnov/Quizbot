//go:build !solution

package quiz

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Engine реализует QuizEngine.
type Engine struct {
	mu sync.RWMutex

	quizzes map[string]*Quiz
	runs    map[string]*runState

	quizSeq atomic.Uint64
	runSeq  atomic.Uint64

	rndMu sync.Mutex
	rnd   *rand.Rand
}

// NewEngine создаёт новый QuizEngine.
func NewEngine() *Engine {
	return &Engine{
		quizzes: make(map[string]*Quiz),
		runs:    make(map[string]*runState),
		rnd:     rand.New(rand.NewSource(time.Now().UnixNano())), //nolint:gosec
	}
}

var (
	errInvalidQuiz       = errors.New("invalid quiz")
	errRunNotFound       = errors.New("run not found")
	errRunNotJoinable    = errors.New("run is not joinable")
	errAlreadyJoined     = errors.New("participant already joined")
	errParticipantsLimit = errors.New("max participants reached")
	errQuizNotInLobby    = errors.New("quiz run is not in lobby")
	errQuizNotRunning    = errors.New("quiz run is not running")
	errQuizNotFinished   = errors.New("quiz run is not finished")
	errInvalidAnswer     = errors.New("invalid answer")
)

type quizJSON struct {
	Title     string     `json:"title"`
	Settings  Settings   `json:"settings"`
	Questions []Question `json:"questions"`
}

type runState struct {
	mu sync.Mutex

	quiz *Quiz
	run  *QuizRun

	questions []Question // prepared questions for this run (order + answer shuffle applied)
	durations []time.Duration
	points    []int

	startTimes []time.Time

	currentQuestion int // -1 not started/finished

	events   chan QuizEvent
	answerCh chan int64
	skipCh   chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
}

// LoadQuiz парсит JSON и создаёт квиз.
func (e *Engine) LoadQuiz(data []byte) (*Quiz, error) {
	var in quizJSON
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, err
	}

	if strings.TrimSpace(in.Title) == "" {
		return nil, fmt.Errorf("%w: missing title", errInvalidQuiz)
	}

	if in.Settings.TimePerQuestion <= 0 {
		return nil, fmt.Errorf("%w: time_per_question must be > 0", errInvalidQuiz)
	}

	if in.Settings.MaxParticipants < 0 {
		return nil, fmt.Errorf("%w: max_participants must be >= 0", errInvalidQuiz)
	}

	if len(in.Questions) == 0 {
		return nil, fmt.Errorf("%w: no questions", errInvalidQuiz)
	}

	for i := range in.Questions {
		q := &in.Questions[i]

		if strings.TrimSpace(q.Text) == "" {
			return nil, fmt.Errorf("%w: question[%d] missing text", errInvalidQuiz, i)
		}

		if len(q.Options) < 2 || len(q.Options) > len(AnswerLetters) {
			return nil, fmt.Errorf("%w: question[%d] options must be 2..6", errInvalidQuiz, i)
		}

		for j := range q.Options {
			if strings.TrimSpace(q.Options[j]) == "" {
				return nil, fmt.Errorf("%w: question[%d] option[%d] is empty", errInvalidQuiz, i, j)
			}
		}

		if q.Correct < 0 || q.Correct >= len(q.Options) {
			return nil, fmt.Errorf("%w: question[%d] correct index out of range", errInvalidQuiz, i)
		}

		if q.Points < 0 {
			return nil, fmt.Errorf("%w: question[%d] points must be >= 0", errInvalidQuiz, i)
		}

		if q.Time < 0 {
			return nil, fmt.Errorf("%w: question[%d] time must be >= 0", errInvalidQuiz, i)
		}
	}

	now := time.Now()
	id := fmt.Sprintf("quiz-%d", e.quizSeq.Add(1))
	out := &Quiz{
		ID:        id,
		OwnerID:   0,
		Title:     in.Title,
		Settings:  in.Settings,
		Questions: in.Questions,
		CreatedAt: now,
	}

	e.mu.Lock()
	e.quizzes[out.ID] = out
	e.mu.Unlock()

	return cloneQuiz(out), nil
}

// StartRun создаёт новый запуск квиза.
func (e *Engine) StartRun(ctx context.Context, quiz *Quiz) (*QuizRun, error) {
	if quiz == nil {
		return nil, fmt.Errorf("%w: nil quiz", errInvalidQuiz)
	}

	if strings.TrimSpace(quiz.Title) == "" || quiz.Settings.TimePerQuestion <= 0 ||
		len(quiz.Questions) == 0 {
		return nil, fmt.Errorf("%w: invalid quiz", errInvalidQuiz)
	}

	runID := fmt.Sprintf("run-%d", e.runSeq.Add(1))
	now := time.Now()

	rsCtx, cancel := context.WithCancel(ctx)

	rs := &runState{
		quiz: cloneQuiz(quiz),
		run: &QuizRun{
			ID:           runID,
			QuizID:       quiz.ID,
			Status:       RunStatusLobby,
			Participants: make(map[int64]*Participant),
			Answers:      make(map[int64][]Answer),
		},
		currentQuestion: -1,
		// Buffered so quiz loop never blocks on sending events if receiver is slow or stops early in tests.
		events:   make(chan QuizEvent, len(quiz.Questions)*2+2),
		answerCh: make(chan int64, 1024),
		skipCh:   make(chan struct{}, 1),
		ctx:      rsCtx,
		cancel:   cancel,
	}
	rs.run.StartedAt = time.Time{}
	rs.run.FinishedAt = time.Time{}
	_ = now

	// Prepare per-run question order and (optionally) shuffled answers.
	rs.questions, rs.durations, rs.points = e.prepareRunQuestions(rs.quiz)
	rs.startTimes = make([]time.Time, len(rs.questions))

	e.mu.Lock()
	e.runs[runID] = rs
	e.mu.Unlock()

	return cloneRun(rs.run), nil
}

// JoinRun добавляет участника в запуск квиза.
func (e *Engine) JoinRun(ctx context.Context, runID string, participant *Participant) error {
	_ = ctx

	if participant == nil {
		return errors.New("nil participant")
	}

	rs, err := e.getRunState(runID)
	if err != nil {
		return err
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	switch rs.run.Status {
	case RunStatusLobby, RunStatusRunning:
		// ok
	default:
		return errRunNotJoinable
	}

	if _, exists := rs.run.Participants[participant.TelegramID]; exists {
		return errAlreadyJoined
	}

	if maxPrt := rs.quiz.Settings.MaxParticipants; maxPrt > 0 &&
		len(rs.run.Participants) >= maxPrt {
		return errParticipantsLimit
	}

	cp := *participant
	if cp.RegData == nil {
		cp.RegData = make(map[string]string)
	}

	cp.JoinedAt = time.Now()
	rs.run.Participants[cp.TelegramID] = &cp
	rs.run.Answers[cp.TelegramID] = nil

	return nil
}

// GetParticipantCount возвращает текущее количество участников.
func (e *Engine) GetParticipantCount(runID string) int {
	rs, err := e.getRunState(runID)
	if err != nil {
		return 0
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	return len(rs.run.Participants)
}

// StartQuiz запускает квиз.
func (e *Engine) StartQuiz(ctx context.Context, runID string) (<-chan QuizEvent, error) {
	rs, err := e.getRunState(runID)
	if err != nil {
		return nil, err
	}

	rs.mu.Lock()

	if rs.run.Status != RunStatusLobby {
		rs.mu.Unlock()
		return nil, errQuizNotInLobby
	}

	rs.run.Status = RunStatusRunning
	rs.run.StartedAt = time.Now()
	rs.ctx, rs.cancel = context.WithCancel(ctx)
	rs.mu.Unlock()

	go e.runQuizLoop(rs)

	return rs.events, nil
}

// SubmitAnswer регистрирует ответ участника.
func (e *Engine) SubmitAnswer(
	ctx context.Context,
	runID string,
	participantID int64,
	questionIdx int,
	answerIdx int,
) error {
	_ = ctx

	rs, err := e.getRunState(runID)
	if err != nil {
		return err
	}

	now := time.Now()

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.run.Status != RunStatusRunning {
		return errQuizNotRunning
	}

	if questionIdx < 0 || questionIdx >= len(rs.questions) {
		return fmt.Errorf("%w: bad question idx", errInvalidAnswer)
	}

	if answerIdx < 0 || answerIdx >= len(rs.questions[questionIdx].Options) {
		return fmt.Errorf("%w: bad answer idx", errInvalidAnswer)
	}

	if rs.currentQuestion != questionIdx {
		return fmt.Errorf("%w: not current question", errInvalidAnswer)
	}

	p, ok := rs.run.Participants[participantID]
	if !ok || p == nil {
		return fmt.Errorf("%w: unknown participant", errInvalidAnswer)
	}

	// Check question is still open.
	start := rs.startTimes[questionIdx]
	if !start.IsZero() {
		deadline := start.Add(rs.durations[questionIdx])
		if now.After(deadline) {
			return fmt.Errorf("%w: question closed", errInvalidAnswer)
		}
	}

	// Enforce single answer per participant per question.
	if hasAnswered(rs.run.Answers[participantID], questionIdx) {
		return fmt.Errorf("%w: already answered", errInvalidAnswer)
	}

	q := rs.questions[questionIdx]
	pts := 0

	isCorrect := answerIdx == q.Correct
	if isCorrect {
		pts = rs.points[questionIdx]
	}

	a := Answer{
		QuestionIdx: questionIdx,
		AnswerIdx:   answerIdx,
		IsCorrect:   isCorrect,
		Points:      pts,
		AnsweredAt:  now,
	}

	rs.run.Answers[participantID] = append(rs.run.Answers[participantID], a)

	// Notify quiz loop that a new answer arrived.
	select {
	case rs.answerCh <- participantID:
	default:
		// In extreme situations we may drop notifications; quiz loop will still time out correctly.
	}

	return nil
}

// SubmitAnswerByLetter регистрирует ответ участника по букве.
func (e *Engine) SubmitAnswerByLetter(
	ctx context.Context,
	runID string,
	participantID int64,
	letter string,
) error {
	letter = strings.TrimSpace(letter)
	letter = strings.ToUpper(letter)

	idx, ok := LetterToIndex(letter)
	if !ok {
		return fmt.Errorf("%w: unknown letter", errInvalidAnswer)
	}

	qIdx := e.GetCurrentQuestion(runID)
	if qIdx < 0 {
		return errQuizNotRunning
	}

	return e.SubmitAnswer(ctx, runID, participantID, qIdx, idx)
}

// GetCurrentQuestion возвращает текущий номер вопроса.
func (e *Engine) GetCurrentQuestion(runID string) int {
	rs, err := e.getRunState(runID)
	if err != nil {
		return -1
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.run.Status != RunStatusRunning {
		return -1
	}

	if rs.currentQuestion < 0 || rs.currentQuestion >= len(rs.questions) {
		return -1
	}

	return rs.currentQuestion
}

// GetResults возвращает результаты квиза.
func (e *Engine) GetResults(runID string) (*QuizResults, error) {
	rs, err := e.getRunState(runID)
	if err != nil {
		return nil, err
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.run.Status != RunStatusFinished {
		return nil, errQuizNotFinished
	}

	results := e.computeResultsLocked(rs)

	return results, nil
}

// ExportCSV экспортирует результаты в CSV.
func (e *Engine) ExportCSV(runID string) ([]byte, error) {
	results, err := e.GetResults(runID)
	if err != nil {
		return nil, err
	}

	var buf bytes.Buffer

	w := csv.NewWriter(&buf)

	header := []string{
		"Rank",
		"TelegramID",
		"Username",
		"FirstName",
		"LastName",
		"Score",
		"CorrectCount",
		"TotalTime",
	}
	if err := w.Write(header); err != nil {
		return nil, err
	}

	for _, entry := range results.Leaderboard {
		p := entry.Participant

		rec := []string{
			fmt.Sprintf("%d", entry.Rank),
			fmt.Sprintf("%d", p.TelegramID),
			p.Username,
			p.FirstName,
			p.LastName,
			fmt.Sprintf("%d", entry.Score),
			fmt.Sprintf("%d", entry.CorrectCount),
			entry.TotalTime.String(),
		}
		if err := w.Write(rec); err != nil {
			return nil, err
		}
	}

	w.Flush()

	if err := w.Error(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// GetRun возвращает запуск по ID.
func (e *Engine) GetRun(runID string) (*QuizRun, error) {
	rs, err := e.getRunState(runID)
	if err != nil {
		return nil, err
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	return cloneRun(rs.run), nil
}

// SkipQuestion forces the current question to end immediately (teacher action).
// This method is intentionally not part of the QuizEngine interface; bot calls it via type assertion.
func (e *Engine) SkipQuestion(runID string) error {
	rs, err := e.getRunState(runID)
	if err != nil {
		return err
	}

	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.run.Status != RunStatusRunning {
		return errQuizNotRunning
	}

	select {
	case rs.skipCh <- struct{}{}:
	default:
	}

	return nil
}

func (e *Engine) getRunState(runID string) (*runState, error) {
	e.mu.RLock()
	rs := e.runs[runID]
	e.mu.RUnlock()

	if rs == nil {
		return nil, errRunNotFound
	}

	return rs, nil
}

func (e *Engine) prepareRunQuestions(q *Quiz) ([]Question, []time.Duration, []int) {
	// Create order indices.
	order := make([]int, len(q.Questions))
	for i := range order {
		order[i] = i
	}

	if q.Settings.ShuffleQuestions {
		e.rndMu.Lock()
		e.rnd.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		e.rndMu.Unlock()
	}

	outQ := make([]Question, 0, len(order))
	outDur := make([]time.Duration, 0, len(order))
	outPts := make([]int, 0, len(order))

	for _, origIdx := range order {
		qq := q.Questions[origIdx] // copy

		pts := qq.Points
		if pts <= 0 {
			pts = 1
		}

		secs := qq.Time
		if secs <= 0 {
			secs = q.Settings.TimePerQuestion
		}

		// Shuffle answers if enabled (global or per-question override).
		shuffle := q.Settings.ShuffleAnswers
		if qq.Shuffle != nil {
			shuffle = *qq.Shuffle
		}

		if shuffle {
			qq = shuffleQuestionOptions(e, qq)
		}

		outQ = append(outQ, qq)
		outDur = append(outDur, time.Duration(secs)*time.Second)
		outPts = append(outPts, pts)
	}

	return outQ, outDur, outPts
}

func shuffleQuestionOptions(e *Engine, q Question) Question {
	type pair struct {
		txt   string
		isCor bool
	}

	pairs := make([]pair, 0, len(q.Options))
	for i := range q.Options {
		pairs = append(pairs, pair{txt: q.Options[i], isCor: i == q.Correct})
	}

	e.rndMu.Lock()
	e.rnd.Shuffle(len(pairs), func(i, j int) { pairs[i], pairs[j] = pairs[j], pairs[i] })
	e.rndMu.Unlock()

	opts := make([]string, 0, len(pairs))
	correct := 0

	for i := range pairs {
		opts = append(opts, pairs[i].txt)
		if pairs[i].isCor {
			correct = i
		}
	}

	q.Options = opts
	q.Correct = correct

	return q
}

func (e *Engine) runQuizLoop(rs *runState) {
	defer close(rs.events)

	for qIdx := 0; qIdx < len(rs.questions); qIdx++ {
		rs.mu.Lock()

		if rs.run.Status != RunStatusRunning {
			rs.mu.Unlock()
			return
		}

		rs.currentQuestion = qIdx
		start := time.Now()
		rs.startTimes[qIdx] = start
		dur := rs.durations[qIdx]

		eligible := make(map[int64]struct{})

		for pid, p := range rs.run.Participants {
			if p == nil {
				continue
			}
			// Eligible for early completion only if participant joined before this question started.
			if !p.JoinedAt.After(start) {
				eligible[pid] = struct{}{}
			}
		}

		questionCopy := rs.questions[qIdx] // local copy
		rs.mu.Unlock()

		// Notify about question.
		select {
		case rs.events <- QuizEvent{
			Type:        EventTypeQuestion,
			QuestionIdx: qIdx,
			Question:    &questionCopy,
			TimeLeft:    dur,
		}:
		case <-rs.ctx.Done():
			rs.finishEarly()
			return
		}

		timer := time.NewTimer(dur)

		for {
			if len(eligible) == 0 {
				// Nobody to wait for -> proceed immediately.
				if !timer.Stop() {
					<-timer.C
				}

				break
			}

			select {
			case <-rs.ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}

				rs.finishEarly()

				return

			case <-rs.skipCh:
				if !timer.Stop() {
					<-timer.C
				}
				// Notify listeners that question ended early.
				select {
				case rs.events <- QuizEvent{
					Type:        EventTypeTimeUp,
					QuestionIdx: qIdx,
					TimeLeft:    0,
				}:
				case <-rs.ctx.Done():
					rs.finishEarly()
					return
				}

				goto nextQuestion

			case <-timer.C:
				// Time up for this question.
				select {
				case rs.events <- QuizEvent{
					Type:        EventTypeTimeUp,
					QuestionIdx: qIdx,
					TimeLeft:    0,
				}:
				case <-rs.ctx.Done():
					rs.finishEarly()
					return
				}

				goto nextQuestion

			case pid := <-rs.answerCh:
				rs.mu.Lock()
				answered := hasAnswered(rs.run.Answers[pid], qIdx)
				rs.mu.Unlock()

				if !answered {
					// Could be a delayed notification or dropped answer; ignore.
					continue
				}

				delete(eligible, pid)
				// If all eligible answered, proceed immediately without time_up.
				if len(eligible) == 0 {
					if !timer.Stop() {
						<-timer.C
					}

					goto nextQuestion
				}
			}
		}

	nextQuestion:
		continue
	}

	// Finish.
	rs.mu.Lock()
	rs.run.Status = RunStatusFinished
	rs.run.FinishedAt = time.Now()
	rs.currentQuestion = -1
	results := e.computeResultsLocked(rs)
	rs.mu.Unlock()

	select {
	case rs.events <- QuizEvent{
		Type:    EventTypeFinished,
		Results: results,
	}:
	case <-rs.ctx.Done():
		return
	}
}

func (rs *runState) finishEarly() {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	if rs.run.Status == RunStatusFinished {
		return
	}

	rs.run.Status = RunStatusFinished
	rs.run.FinishedAt = time.Now()
	rs.currentQuestion = -1
}

func (e *Engine) computeResultsLocked(rs *runState) *QuizResults {
	totalQuizTime := time.Duration(0)
	for _, d := range rs.durations {
		totalQuizTime += d
	}

	entries := make([]LeaderboardEntry, 0, len(rs.run.Participants))
	for pid, p := range rs.run.Participants {
		if p == nil {
			continue
		}

		score := 0
		correct := 0
		totalTime := time.Duration(0)

		answers := rs.run.Answers[pid]

		for qIdx := 0; qIdx < len(rs.questions); qIdx++ {
			dur := rs.durations[qIdx]

			start := rs.startTimes[qIdx]
			if start.IsZero() {
				// Shouldn't happen for a finished run, but keep deterministic behavior.
				totalTime += dur
				continue
			}

			deadline := start.Add(dur)

			if p.JoinedAt.After(deadline) {
				totalTime += dur
				continue
			}

			a, ok := findAnswer(answers, qIdx)
			if !ok {
				totalTime += dur
				continue
			}

			dt := a.AnsweredAt.Sub(start)
			if dt < 0 {
				dt = 0
			}

			if dt > dur {
				dt = dur
			}

			totalTime += dt

			score += a.Points
			if a.IsCorrect {
				correct++
			}
		}

		entries = append(entries, LeaderboardEntry{
			Participant:  cloneParticipant(p),
			Score:        score,
			CorrectCount: correct,
			TotalTime:    totalTime,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Score != entries[j].Score {
			return entries[i].Score > entries[j].Score
		}

		if entries[i].TotalTime != entries[j].TotalTime {
			return entries[i].TotalTime < entries[j].TotalTime
		}

		return entries[i].Participant.TelegramID < entries[j].Participant.TelegramID
	})

	for i := range entries {
		entries[i].Rank = i + 1
	}

	return &QuizResults{
		RunID:       rs.run.ID,
		QuizTitle:   rs.quiz.Title,
		Leaderboard: entries,
		TotalTime:   totalQuizTime,
	}
}

func hasAnswered(answers []Answer, questionIdx int) bool {
	_, ok := findAnswer(answers, questionIdx)
	return ok
}

func findAnswer(answers []Answer, questionIdx int) (Answer, bool) {
	for i := range answers {
		if answers[i].QuestionIdx == questionIdx {
			return answers[i], true
		}
	}

	return Answer{}, false
}

func cloneParticipant(p *Participant) *Participant {
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

func cloneQuiz(q *Quiz) *Quiz {
	if q == nil {
		return nil
	}

	cp := *q
	cp.Questions = make([]Question, len(q.Questions))
	copy(cp.Questions, q.Questions)
	// Settings is a value type; ok.
	return &cp
}

func cloneRun(r *QuizRun) *QuizRun {
	if r == nil {
		return nil
	}

	cp := *r

	cp.Participants = make(map[int64]*Participant, len(r.Participants))
	for id, p := range r.Participants {
		cp.Participants[id] = cloneParticipant(p)
	}

	cp.Answers = make(map[int64][]Answer, len(r.Answers))
	for id, ans := range r.Answers {
		dst := make([]Answer, len(ans))
		copy(dst, ans)
		cp.Answers[id] = dst
	}

	return &cp
}
