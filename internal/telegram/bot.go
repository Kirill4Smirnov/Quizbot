//go:build !solution

package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"gitlab.com/slon/shad-go/Exam-1-QuizBot/quizbot/internal/quiz"
	"gitlab.com/slon/shad-go/Exam-1-QuizBot/quizbot/internal/storage"
)

// Bot реализует Telegram бота для квизов.
type Bot struct {
	client      Client
	engine      quiz.QuizEngine
	botUsername string
	storage     storage.Storage
	debug       bool

	mu sync.Mutex

	teacherChatByRun map[string]int64

	lobbyMsgByRun    map[string]messageRef
	lobbyCancelByRun map[string]context.CancelFunc

	runEventsStarted map[string]struct{}

	teacherMsgByRun    map[string]messageRef
	teacherCancelByRun map[string]context.CancelFunc

	questionMsgByRun    map[string]map[int64]int // runID -> participantID -> messageID
	questionCancelByRun map[string]context.CancelFunc

	regPending map[int64]*pendingReg
}

type messageRef struct {
	chatID int64
	msgID  int
}

type pendingReg struct {
	runID       string
	quizID      string
	fields      []string
	idx         int
	participant *quiz.Participant
}

// NewBot создаёт нового бота.
// botUsername — username бота без @ (например, "my_quiz_bot").
// Используется для формирования ссылок: https://t.me/<botUsername>?start=join_<runID>
func NewBot(
	client Client,
	engine quiz.QuizEngine,
	botUsername string,
	st storage.Storage,
	debug bool,
) *Bot {
	return &Bot{
		client:      client,
		engine:      engine,
		botUsername: botUsername,
		storage:     st,
		debug:       debug,

		teacherChatByRun:    make(map[string]int64),
		lobbyMsgByRun:       make(map[string]messageRef),
		lobbyCancelByRun:    make(map[string]context.CancelFunc),
		runEventsStarted:    make(map[string]struct{}),
		teacherMsgByRun:     make(map[string]messageRef),
		teacherCancelByRun:  make(map[string]context.CancelFunc),
		questionMsgByRun:    make(map[string]map[int64]int),
		questionCancelByRun: make(map[string]context.CancelFunc),
		regPending:          make(map[int64]*pendingReg),
	}
}

func (b *Bot) Run(ctx context.Context) error {
	offset := 0

	log.Printf("quizbot: started (botUsername=%s)", b.botUsername)

	for {
		select {
		case <-ctx.Done():
			b.shutdown()
			return ctx.Err()
		default:
		}

		updates, err := b.client.GetUpdates(offset, 30)
		if err != nil {
			if b.debug {
				log.Printf("quizbot: getUpdates error: %v", err)
			}
			// network hiccups: backoff a bit and continue
			select {
			case <-ctx.Done():
				b.shutdown()
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
			}

			continue
		}

		for _, upd := range updates {
			if upd.UpdateID >= offset {
				offset = upd.UpdateID + 1
			}

			_ = b.HandleUpdate(upd)
		}
	}
}

// HandleUpdate обрабатывает одно обновление.
func (b *Bot) HandleUpdate(update Update) error {
	if update.CallbackQuery != nil {
		return b.handleCallback(update.CallbackQuery)
	}

	if update.Message != nil {
		return b.handleMessage(update.Message)
	}

	return nil
}

func (b *Bot) shutdown() {
	b.mu.Lock()
	// stop lobby updaters
	for runID, cancel := range b.lobbyCancelByRun {
		cancel()
		delete(b.lobbyCancelByRun, runID)
	}
	// stop teacher updaters
	for runID, cancel := range b.teacherCancelByRun {
		cancel()
		delete(b.teacherCancelByRun, runID)
	}
	// stop question updaters
	for runID, cancel := range b.questionCancelByRun {
		cancel()
		delete(b.questionCancelByRun, runID)
	}

	b.mu.Unlock()
}

func (b *Bot) handleMessage(msg *Message) error {
	if msg.Chat == nil || msg.From == nil {
		return nil
	}

	chatID := msg.Chat.ID
	user := msg.From

	txt := strings.TrimSpace(msg.Text)

	// Registration flow (text replies).
	if txt != "" {
		b.mu.Lock()
		p := b.regPending[user.ID]
		b.mu.Unlock()

		if p != nil {
			return b.handleRegText(chatID, user.ID, txt)
		}
	}

	// Commands.
	if strings.HasPrefix(txt, "/start") {
		return b.handleStart(chatID, msg.Text, user)
	}

	if strings.HasPrefix(txt, "/help") {
		_, _ = b.client.SendMessage(chatID, helpText(), nil)
		return nil
	}

	if strings.HasPrefix(txt, "/myquizzes") || strings.HasPrefix(txt, "/quizzes") {
		return b.showMyQuizzes(chatID, user.ID)
	}

	// Quiz answer letters during run.
	if txt != "" {
		// Try to find a running run where this user is a participant by scanning.
		// This is not the most efficient, but OK for in-memory + small groups.
		runID := b.findParticipantRun(user.ID)
		if runID != "" {
			err := b.engine.SubmitAnswerByLetter(context.Background(), runID, user.ID, txt)
			if err == nil {
				_, _ = b.client.SendMessage(chatID, "✅ Ответ принят!", nil)
			}

			return nil
		}
	}

	// Quiz upload (document).
	if msg.Document != nil {
		return b.handleQuizUpload(chatID, user.ID, msg.Document)
	}

	return nil
}

func (b *Bot) handleStart(chatID int64, text string, user *User) error {
	_ = user

	parts := strings.Fields(text)
	if len(parts) > 0 {
		cmd := parts[0]
		// Telegram can send /start@bot_username in group/supergroup contexts.
		if at := strings.IndexByte(cmd, '@'); at >= 0 {
			cmd = cmd[:at]
		}

		payload := ""
		if len(parts) >= 2 {
			payload = parts[1]
		}

		if cmd == "/start" && strings.HasPrefix(payload, "join_") {
			runID := strings.TrimPrefix(payload, "join_")
			return b.sendJoinPrompt(chatID, runID)
		}
	}

	_, _ = b.client.SendMessage(
		chatID,
		"Если вы преподаватель, то отправьте мне JSON файл с квизом (как документ). Я создам лобби и ссылку для студентов.\n\n"+
			"Если вы студент и у вас есть runID, отправьте: /start join_<runID>",
		nil,
	)

	return nil
}

func (b *Bot) sendJoinPrompt(chatID int64, runID string) error {
	btn := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Присоединиться", CallbackData: "join_run:" + runID}},
		},
	}
	_, _ = b.client.SendMessage(chatID,
		fmt.Sprintf("Квиз: присоединиться к запуску `%s`", runID),
		&SendOptions{ParseMode: "Markdown", ReplyMarkup: &btn},
	)

	return nil
}

func (b *Bot) handleQuizUpload(chatID int64, ownerID int64, doc *Document) error {
	if doc == nil {
		return nil
	}
	// Only accept .json-ish files.
	if doc.MimeType != "" && !strings.Contains(doc.MimeType, "json") &&
		!strings.HasSuffix(strings.ToLower(doc.FileName), ".json") {
		_, _ = b.client.SendMessage(chatID, "Пожалуйста, пришлите JSON файл.", nil)
		return nil
	}

	filePath, err := b.client.GetFile(doc.FileID)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог получить файл из Telegram.", nil)
		return err
	}

	data, err := b.client.DownloadFile(filePath)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог скачать файл из Telegram.", nil)
		return err
	}

	qz, err := b.engine.LoadQuiz(data)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, fmt.Sprintf("Ошибка в JSON: %v", err), nil)
		return nil
	}

	qz.OwnerID = ownerID

	// Save quiz (advanced part).
	_ = b.storage.SaveQuiz(context.Background(), qz)

	run, err := b.engine.StartRun(context.Background(), qz)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, fmt.Sprintf("Не смог создать лобби: %v", err), nil)
		return err
	}

	_ = b.storage.SaveRun(context.Background(), run)

	b.mu.Lock()
	b.teacherChatByRun[run.ID] = chatID
	b.mu.Unlock()

	link := buildDeepLink(b.botUsername, "join_"+run.ID)
	btn := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Начать квиз", CallbackData: "start_run:" + run.ID}},
			{{Text: "Ссылка для студентов", URL: link}},
		},
	}

	msg, _ := b.client.SendMessage(
		chatID,
		b.lobbyText(run.ID, qz.Title),
		&SendOptions{ReplyMarkup: &btn},
	)
	if msg != nil {
		b.mu.Lock()
		b.lobbyMsgByRun[run.ID] = messageRef{chatID: chatID, msgID: msg.MessageID}
		b.mu.Unlock()
		b.startLobbyTicker(run.ID, qz.Title, &btn)
	}

	return nil
}

func (b *Bot) lobbyText(runID string, title string) string {
	cnt := b.engine.GetParticipantCount(runID)
	link := buildDeepLink(b.botUsername, "join_"+runID)

	return fmt.Sprintf(
		"Лобби квиза: %s\nRunID: %s\nУчастников: %d\n\nСсылка для студентов:\n%s\n\nЕсли бот уже открыт, отправьте в чат с ботом:\n/start join_%s",
		title,
		runID,
		cnt,
		link,
		runID,
	)
}

func (b *Bot) startLobbyTicker(runID string, title string, opts *InlineKeyboardMarkup) {
	b.mu.Lock()

	if cancel, ok := b.lobbyCancelByRun[runID]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.lobbyCancelByRun[runID] = cancel
	lobby := b.lobbyMsgByRun[runID]
	b.mu.Unlock()

	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = b.client.EditMessage(
					lobby.chatID,
					lobby.msgID,
					b.lobbyText(runID, title),
					&SendOptions{ReplyMarkup: opts},
				)
			}
		}
	}()
}

func (b *Bot) handleCallback(cb *CallbackQuery) error {
	if cb == nil || cb.Message == nil || cb.From == nil {
		return nil
	}

	data := cb.Data
	if data == "" {
		return nil
	}

	switch {
	case strings.HasPrefix(data, "join_run:"):
		runID := strings.TrimPrefix(data, "join_run:")
		_ = b.client.AnswerCallback(cb.ID, "Ок")

		return b.joinRun(cb.Message.Chat.ID, cb.From, runID)
	case strings.HasPrefix(data, "start_run:"):
		runID := strings.TrimPrefix(data, "start_run:")
		_ = b.client.AnswerCallback(cb.ID, "Стартуем")

		return b.startRun(cb.Message.Chat.ID, runID)
	case strings.HasPrefix(data, "skip_run:"):
		runID := strings.TrimPrefix(data, "skip_run:")
		_ = b.client.AnswerCallback(cb.ID, "Пропускаю вопрос")

		return b.skipQuestion(cb.Message.Chat.ID, cb.From.ID, runID)
	case strings.HasPrefix(data, "quiz_show:"):
		quizID := strings.TrimPrefix(data, "quiz_show:")
		_ = b.client.AnswerCallback(cb.ID, "Ок")

		return b.showQuiz(cb.Message.Chat.ID, cb.From.ID, quizID)
	case strings.HasPrefix(data, "quiz_download:"):
		quizID := strings.TrimPrefix(data, "quiz_download:")
		_ = b.client.AnswerCallback(cb.ID, "Готовлю JSON")

		return b.downloadQuizJSON(cb.Message.Chat.ID, cb.From.ID, quizID)
	case strings.HasPrefix(data, "quiz_rerun:"):
		quizID := strings.TrimPrefix(data, "quiz_rerun:")
		_ = b.client.AnswerCallback(cb.ID, "Создаю лобби")

		return b.createLobbyFromSavedQuiz(cb.Message.Chat.ID, cb.From.ID, quizID)
	case strings.HasPrefix(data, "quiz_runs:"):
		quizID := strings.TrimPrefix(data, "quiz_runs:")
		_ = b.client.AnswerCallback(cb.ID, "Ок")

		return b.showQuizRuns(cb.Message.Chat.ID, cb.From.ID, quizID)
	case strings.HasPrefix(data, "run_show:"):
		runID := strings.TrimPrefix(data, "run_show:")
		_ = b.client.AnswerCallback(cb.ID, "Ок")

		return b.showRun(cb.Message.Chat.ID, cb.From.ID, runID)
	case strings.HasPrefix(data, "run_csv:"):
		runID := strings.TrimPrefix(data, "run_csv:")
		_ = b.client.AnswerCallback(cb.ID, "Готовлю CSV")

		return b.downloadRunCSV(cb.Message.Chat.ID, cb.From.ID, runID)
	default:
		_ = b.client.AnswerCallback(cb.ID, "Неизвестная команда")
		return nil
	}
}

func (b *Bot) joinRun(chatID int64, user *User, runID string) error {
	run, err := b.engine.GetRun(runID)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, "Запуск не найден.", nil)
		return nil
	}

	// Registration fields are stored on quiz, so fetch quiz from storage if possible.
	qz, _ := b.storage.GetQuiz(context.Background(), run.QuizID)

	regFields := []string(nil)
	if qz != nil {
		regFields = qz.Settings.Registration
	}

	p := &quiz.Participant{
		TelegramID: user.ID,
		Username:   user.Username,
		FirstName:  user.FirstName,
		LastName:   user.LastName,
		RegData:    make(map[string]string),
	}

	if len(regFields) > 0 {
		b.mu.Lock()
		b.regPending[user.ID] = &pendingReg{
			runID:       runID,
			quizID:      run.QuizID,
			fields:      append([]string(nil), regFields...),
			idx:         0,
			participant: p,
		}
		b.mu.Unlock()
		_, _ = b.client.SendMessage(
			chatID,
			fmt.Sprintf("Регистрация: введите `%s`", regFields[0]),
			&SendOptions{ParseMode: "Markdown"},
		)

		return nil
	}

	if err := b.engine.JoinRun(context.Background(), runID, p); err != nil {
		_, _ = b.client.SendMessage(chatID, fmt.Sprintf("Не удалось присоединиться: %v", err), nil)
		return nil
	}

	_, _ = b.client.SendMessage(chatID, "Вы в лобби. Ждите начала квиза.", nil)

	return nil
}

func (b *Bot) handleRegText(chatID int64, userID int64, value string) error {
	b.mu.Lock()
	p := b.regPending[userID]
	b.mu.Unlock()

	if p == nil {
		return nil
	}

	if p.idx < 0 || p.idx >= len(p.fields) {
		return nil
	}

	field := p.fields[p.idx]
	p.participant.RegData[field] = value
	p.idx++

	if p.idx < len(p.fields) {
		next := p.fields[p.idx]
		_, _ = b.client.SendMessage(
			chatID,
			fmt.Sprintf("Введите `%s`", next),
			&SendOptions{ParseMode: "Markdown"},
		)

		return nil
	}

	// Finish registration: join run.
	err := b.engine.JoinRun(context.Background(), p.runID, p.participant)
	b.mu.Lock()
	delete(b.regPending, userID)
	b.mu.Unlock()

	if err != nil {
		_, _ = b.client.SendMessage(chatID, fmt.Sprintf("Не удалось присоединиться: %v", err), nil)
		return nil
	}

	_, _ = b.client.SendMessage(chatID, "Регистрация завершена. Вы в лобби — ждите начала.", nil)

	return nil
}

func (b *Bot) startRun(chatID int64, runID string) error {
	// Stop lobby ticker.
	b.mu.Lock()

	if cancel, ok := b.lobbyCancelByRun[runID]; ok {
		cancel()
		delete(b.lobbyCancelByRun, runID)
	}

	_, started := b.runEventsStarted[runID]
	if !started {
		b.runEventsStarted[runID] = struct{}{}
	}

	b.mu.Unlock()

	if started {
		_, _ = b.client.SendMessage(chatID, "Квиз уже запущен.", nil)
		return nil
	}

	// UX: do not start quiz with zero participants (otherwise it finishes instantly).
	if b.engine.GetParticipantCount(runID) == 0 {
		b.mu.Lock()
		delete(b.runEventsStarted, runID)
		b.mu.Unlock()
		_, _ = b.client.SendMessage(chatID,
			"Сейчас в лобби 0 участников — если запустить квиз, он мгновенно завершится.\n"+
				"Дождитесь хотя бы одного студента (или перешлите ссылку/команду из лобби).",
			nil,
		)

		return nil
	}

	events, err := b.engine.StartQuiz(context.Background(), runID)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, fmt.Sprintf("Не удалось запустить квиз: %v", err), nil)
		return nil
	}

	// Create a teacher status message that we keep updating.
	status, _ := b.client.SendMessage(chatID, "Квиз запущен. Готовлю первый вопрос…", &SendOptions{
		ReplyMarkup: &InlineKeyboardMarkup{
			InlineKeyboard: [][]InlineKeyboardButton{
				{{Text: "Пропустить вопрос", CallbackData: "skip_run:" + runID}},
			},
		},
	})
	if status != nil {
		b.mu.Lock()
		b.teacherMsgByRun[runID] = messageRef{chatID: chatID, msgID: status.MessageID}
		b.mu.Unlock()
		b.startTeacherUpdater(runID)
	}

	go b.runEventForwarder(runID, events)

	return nil
}

func (b *Bot) startTeacherUpdater(runID string) {
	b.mu.Lock()

	if cancel, ok := b.teacherCancelByRun[runID]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.teacherCancelByRun[runID] = cancel
	target, ok := b.teacherMsgByRun[runID]
	b.mu.Unlock()

	if !ok {
		cancel()
		return
	}

	go func() {
		t := time.NewTicker(1 * time.Second)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				txt, markup := b.teacherStatus(runID)
				_ = b.client.EditMessage(target.chatID, target.msgID, txt, &SendOptions{
					ReplyMarkup: markup,
				})
			}
		}
	}()
}

func (b *Bot) teacherStatus(runID string) (string, *InlineKeyboardMarkup) {
	run, err := b.engine.GetRun(runID)
	if err != nil || run == nil {
		return "Run не найден.", nil
	}

	qIdx := b.engine.GetCurrentQuestion(runID)
	if qIdx < 0 {
		return fmt.Sprintf(
				"Квиз: %s\nСтатус: %s\nУчастников: %d",
				run.QuizID,
				run.Status,
				len(run.Participants),
			), &InlineKeyboardMarkup{
				InlineKeyboard: [][]InlineKeyboardButton{
					{{Text: "Пропустить вопрос", CallbackData: "skip_run:" + runID}},
				},
			}
	}

	total := len(run.Participants)
	answered := 0

	for pid := range run.Participants {
		ans := run.Answers[pid]
		for i := range ans {
			if ans[i].QuestionIdx == qIdx {
				answered++
				break
			}
		}
	}

	txt := fmt.Sprintf(
		"Run: %s\nСтатус: %s\nВопрос: %d\nОтветов: %d/%d",
		runID,
		run.Status,
		qIdx+1,
		answered,
		total,
	)

	return txt, &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Пропустить вопрос", CallbackData: "skip_run:" + runID}},
		},
	}
}

func (b *Bot) skipQuestion(chatID int64, userID int64, runID string) error {
	// Only teacher (creator chat) can skip.
	b.mu.Lock()
	teacherChat := b.teacherChatByRun[runID]
	b.mu.Unlock()

	if teacherChat == 0 || teacherChat != chatID {
		_, _ = b.client.SendMessage(
			chatID,
			"Пропускать вопросы может только преподаватель (чат, где создавалось лобби).",
			nil,
		)

		return nil
	}

	// Call concrete engine method via type assertion.
	type skipper interface {
		SkipQuestion(runID string) error
	}

	s, ok := b.engine.(skipper)
	if !ok {
		_, _ = b.client.SendMessage(chatID, "Текущий engine не поддерживает skip.", nil)
		return nil
	}

	if err := s.SkipQuestion(runID); err != nil {
		_, _ = b.client.SendMessage(
			chatID,
			fmt.Sprintf("Не удалось пропустить вопрос: %v", err),
			nil,
		)

		return nil
	}

	_ = userID
	_, _ = b.client.SendMessage(chatID, "Ок, вопрос пропущен.", nil)

	return nil
}

func (b *Bot) runEventForwarder(runID string, events <-chan quiz.QuizEvent) {
	for ev := range events {
		switch ev.Type {
		case quiz.EventTypeQuestion:
			b.stopQuestionUpdater(runID)
			b.broadcastQuestion(runID, ev.QuestionIdx, ev.Question, ev.TimeLeft)
			b.startQuestionUpdater(runID, ev.QuestionIdx, ev.Question, ev.TimeLeft)
		case quiz.EventTypeTimeUp:
			b.stopQuestionUpdater(runID)
			// optional: broadcast time up
		case quiz.EventTypeFinished:
			b.stopQuestionUpdater(runID)
			// Stop teacher updater.
			b.mu.Lock()

			if cancel, ok := b.teacherCancelByRun[runID]; ok {
				cancel()
				delete(b.teacherCancelByRun, runID)
			}

			b.mu.Unlock()
			b.finishRun(runID, ev.Results)
		}
	}
}

func (b *Bot) broadcastQuestion(runID string, qIdx int, q *quiz.Question, tl time.Duration) {
	run, err := b.engine.GetRun(runID)
	if err != nil || run == nil {
		return
	}

	text := formatQuestion(qIdx, q, tl)

	msgIDs := make(map[int64]int, len(run.Participants))
	for pid := range run.Participants {
		// send to participant chat (private chats: chatID == userID)
		msg, err := b.client.SendMessage(pid, text, &SendOptions{ParseMode: "Markdown"})
		if err == nil && msg != nil {
			msgIDs[pid] = msg.MessageID
		}
	}

	b.mu.Lock()
	b.questionMsgByRun[runID] = msgIDs
	b.mu.Unlock()
}

func (b *Bot) finishRun(runID string, res *quiz.QuizResults) {
	if res == nil {
		r, err := b.engine.GetResults(runID)
		if err == nil {
			res = r
		}
	}

	if res == nil {
		return
	}

	run, _ := b.engine.GetRun(runID)

	// Notify participants.
	if run != nil {
		for pid := range run.Participants {
			msg := formatResultsForParticipant(pid, res)
			_, _ = b.client.SendMessage(pid, msg, nil)
		}
	}

	// Teacher: CSV.
	b.mu.Lock()
	teacherChat := b.teacherChatByRun[runID]
	b.mu.Unlock()

	if teacherChat != 0 {
		csvData, err := b.engine.ExportCSV(runID)
		if err == nil {
			_ = sendDocumentIfPossible(
				b.client,
				teacherChat,
				fmt.Sprintf("%s_%s.csv", sanitizeFileName(res.QuizTitle), runID),
				csvData,
			)
		}

		_, _ = b.client.SendMessage(teacherChat, "Квиз завершён. Результаты отправлены.", nil)
	}

	// Save final run snapshot to storage.
	if run != nil {
		_ = b.storage.UpdateRun(context.Background(), run)
	}
}

func (b *Bot) stopQuestionUpdater(runID string) {
	b.mu.Lock()

	if cancel, ok := b.questionCancelByRun[runID]; ok {
		cancel()
		delete(b.questionCancelByRun, runID)
	}

	delete(b.questionMsgByRun, runID)
	b.mu.Unlock()
}

func (b *Bot) startQuestionUpdater(runID string, qIdx int, q *quiz.Question, dur time.Duration) {
	if q == nil {
		return
	}

	// Adaptive interval to reduce Telegram API load.
	b.mu.Lock()
	msgs := b.questionMsgByRun[runID]
	b.mu.Unlock()

	if len(msgs) == 0 {
		return
	}

	interval := 3 * time.Second
	if len(msgs) > 50 {
		interval = 10 * time.Second
	} else if len(msgs) > 20 {
		interval = 5 * time.Second
	}

	b.mu.Lock()

	if cancel, ok := b.questionCancelByRun[runID]; ok {
		cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	b.questionCancelByRun[runID] = cancel
	b.mu.Unlock()

	start := time.Now()

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				left := dur - time.Since(start)
				if left < 0 {
					left = 0
				}

				txt := formatQuestion(qIdx, q, left)

				b.mu.Lock()
				// copy map to avoid holding lock during HTTP calls
				cur := b.questionMsgByRun[runID]

				local := make(map[int64]int, len(cur))
				for pid, mid := range cur {
					local[pid] = mid
				}

				b.mu.Unlock()

				for pid, mid := range local {
					_ = b.client.EditMessage(pid, mid, txt, &SendOptions{ParseMode: "Markdown"})
				}
			}
		}
	}()
}

func helpText() string {
	return strings.Join([]string{
		"Команды:",
		"- /start — начать (или deep-link для присоединения)",
		"- /myquizzes — мои сохранённые квизы (скачать JSON / повторить / история запусков)",
		"",
		"Для преподавателя: пришлите JSON файл квиза как документ.",
		"Для студента: отвечайте буквами A-F во время вопроса.",
	}, "\n")
}

func formatQuestion(qIdx int, q *quiz.Question, tl time.Duration) string {
	if q == nil {
		return fmt.Sprintf("Вопрос %d", qIdx+1)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("*Вопрос %d*\n\n", qIdx+1))
	sb.WriteString(q.Text)
	sb.WriteString("\n\n")

	for i, opt := range q.Options {
		letter := quiz.IndexToLetter(i)
		sb.WriteString(fmt.Sprintf("%s. %s\n", letter, opt))
	}

	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("⏱ %d секунд\n\n", int(tl.Seconds())))
	sb.WriteString("Отправьте букву ответа (A, B, C, ...)")

	return sb.String()
}

func formatResultsForParticipant(participantID int64, res *quiz.QuizResults) string {
	rank := 0
	score := 0

	for _, e := range res.Leaderboard {
		if e.Participant != nil && e.Participant.TelegramID == participantID {
			rank = e.Rank
			score = e.Score

			break
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("🏆 Результаты квиза \"%s\"\n\n", res.QuizTitle))

	if rank > 0 {
		sb.WriteString(fmt.Sprintf("Ваш результат: %d баллов (место %d)\n\n", score, rank))
	}

	sb.WriteString("Топ-10:\n")

	limit := 10
	if len(res.Leaderboard) < limit {
		limit = len(res.Leaderboard)
	}

	for i := 0; i < limit; i++ {
		e := res.Leaderboard[i]
		name := "unknown"

		if e.Participant != nil {
			if e.Participant.Username != "" {
				name = "@" + e.Participant.Username
			} else {
				name = fmt.Sprintf("%d", e.Participant.TelegramID)
			}
		}

		sb.WriteString(fmt.Sprintf("%d. %s — %d\n", e.Rank, name, e.Score))
	}

	return sb.String()
}

func (b *Bot) showMyQuizzes(chatID int64, ownerID int64) error {
	quizzes, err := b.storage.ListQuizzes(context.Background(), ownerID)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог получить список квизов.", nil)
		return nil
	}

	if len(quizzes) == 0 {
		_, _ = b.client.SendMessage(
			chatID,
			"У вас пока нет сохранённых квизов. Пришлите JSON файл.",
			nil,
		)

		return nil
	}

	btns := make([][]InlineKeyboardButton, 0, len(quizzes))

	limit := 20
	if len(quizzes) < limit {
		limit = len(quizzes)
	}

	for i := 0; i < limit; i++ {
		qz := quizzes[i]

		title := qz.Title
		if title == "" {
			title = qz.ID
		}

		btns = append(btns, []InlineKeyboardButton{
			{Text: title, CallbackData: "quiz_show:" + qz.ID},
		})
	}

	_, _ = b.client.SendMessage(
		chatID,
		"Мои квизы:",
		&SendOptions{ReplyMarkup: &InlineKeyboardMarkup{InlineKeyboard: btns}},
	)

	return nil
}

func (b *Bot) showQuiz(chatID int64, ownerID int64, quizID string) error {
	qz, err := b.storage.GetQuiz(context.Background(), quizID)
	if err != nil || qz == nil {
		_, _ = b.client.SendMessage(chatID, "Квиз не найден.", nil)
		return nil
	}

	if qz.OwnerID != ownerID {
		_, _ = b.client.SendMessage(chatID, "Нет доступа к этому квизу.", nil)
		return nil
	}

	text := fmt.Sprintf("Квиз: %s\nID: %s\nВопросов: %d", qz.Title, qz.ID, len(qz.Questions))
	btn := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Скачать JSON", CallbackData: "quiz_download:" + qz.ID}},
			{{Text: "Провести ещё раз", CallbackData: "quiz_rerun:" + qz.ID}},
			{{Text: "История запусков", CallbackData: "quiz_runs:" + qz.ID}},
		},
	}
	_, _ = b.client.SendMessage(chatID, text, &SendOptions{ReplyMarkup: &btn})

	return nil
}

func (b *Bot) downloadQuizJSON(chatID int64, ownerID int64, quizID string) error {
	qz, err := b.storage.GetQuiz(context.Background(), quizID)
	if err != nil || qz == nil {
		_, _ = b.client.SendMessage(chatID, "Квиз не найден.", nil)
		return nil
	}

	if qz.OwnerID != ownerID {
		_, _ = b.client.SendMessage(chatID, "Нет доступа к этому квизу.", nil)
		return nil
	}

	data, err := encodeQuizJSON(qz)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог сериализовать JSON.", nil)
		return nil
	}

	name := fmt.Sprintf("%s_%s.json", sanitizeFileName(qz.Title), qz.ID)
	if err := sendDocumentIfPossible(b.client, chatID, name, data); err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог отправить файл JSON.", nil)
	}

	return nil
}

func (b *Bot) createLobbyFromSavedQuiz(chatID int64, ownerID int64, quizID string) error {
	qz, err := b.storage.GetQuiz(context.Background(), quizID)
	if err != nil || qz == nil {
		_, _ = b.client.SendMessage(chatID, "Квиз не найден.", nil)
		return nil
	}

	if qz.OwnerID != ownerID {
		_, _ = b.client.SendMessage(chatID, "Нет доступа к этому квизу.", nil)
		return nil
	}

	run, err := b.engine.StartRun(context.Background(), qz)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, fmt.Sprintf("Не смог создать лобби: %v", err), nil)
		return nil
	}

	_ = b.storage.SaveRun(context.Background(), run)

	b.mu.Lock()
	b.teacherChatByRun[run.ID] = chatID
	b.mu.Unlock()

	link := buildDeepLink(b.botUsername, "join_"+run.ID)
	btn := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Начать квиз", CallbackData: "start_run:" + run.ID}},
			{{Text: "Ссылка для студентов", URL: link}},
		},
	}

	msg, _ := b.client.SendMessage(
		chatID,
		b.lobbyText(run.ID, qz.Title),
		&SendOptions{ReplyMarkup: &btn},
	)
	if msg != nil {
		b.mu.Lock()
		b.lobbyMsgByRun[run.ID] = messageRef{chatID: chatID, msgID: msg.MessageID}
		b.mu.Unlock()
		b.startLobbyTicker(run.ID, qz.Title, &btn)
	}

	return nil
}

func (b *Bot) showQuizRuns(chatID int64, ownerID int64, quizID string) error {
	qz, err := b.storage.GetQuiz(context.Background(), quizID)
	if err != nil || qz == nil {
		_, _ = b.client.SendMessage(chatID, "Квиз не найден.", nil)
		return nil
	}

	if qz.OwnerID != ownerID {
		_, _ = b.client.SendMessage(chatID, "Нет доступа к этому квизу.", nil)
		return nil
	}

	runs, err := b.storage.ListRuns(context.Background(), quizID)
	if err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог получить историю запусков.", nil)
		return nil
	}

	if len(runs) == 0 {
		_, _ = b.client.SendMessage(chatID, "Запусков пока нет.", nil)
		return nil
	}

	btns := make([][]InlineKeyboardButton, 0, len(runs))

	limit := 30
	if len(runs) < limit {
		limit = len(runs)
	}

	for i := 0; i < limit; i++ {
		r := runs[i]
		label := fmt.Sprintf("%s (%s)", r.ID, r.Status)
		btns = append(btns, []InlineKeyboardButton{{Text: label, CallbackData: "run_show:" + r.ID}})
	}

	_, _ = b.client.SendMessage(
		chatID,
		fmt.Sprintf("История запусков квиза: %s", qz.Title),
		&SendOptions{ReplyMarkup: &InlineKeyboardMarkup{InlineKeyboard: btns}},
	)

	return nil
}

func (b *Bot) showRun(chatID int64, ownerID int64, runID string) error {
	r, err := b.storage.GetRun(context.Background(), runID)
	if err != nil || r == nil {
		_, _ = b.client.SendMessage(chatID, "Запуск не найден.", nil)
		return nil
	}

	qz, _ := b.storage.GetQuiz(context.Background(), r.QuizID)
	if qz == nil || qz.OwnerID != ownerID {
		_, _ = b.client.SendMessage(chatID, "Нет доступа.", nil)
		return nil
	}

	text := fmt.Sprintf(
		"Run: %s\nQuiz: %s\nStatus: %s\nParticipants: %d",
		r.ID,
		qz.Title,
		r.Status,
		len(r.Participants),
	)
	btn := InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{{Text: "Скачать CSV результатов", CallbackData: "run_csv:" + r.ID}},
		},
	}
	_, _ = b.client.SendMessage(chatID, text, &SendOptions{ReplyMarkup: &btn})

	return nil
}

func (b *Bot) downloadRunCSV(chatID int64, ownerID int64, runID string) error {
	r, err := b.storage.GetRun(context.Background(), runID)
	if err != nil || r == nil {
		_, _ = b.client.SendMessage(chatID, "Запуск не найден.", nil)
		return nil
	}

	qz, _ := b.storage.GetQuiz(context.Background(), r.QuizID)
	if qz == nil || qz.OwnerID != ownerID {
		_, _ = b.client.SendMessage(chatID, "Нет доступа.", nil)
		return nil
	}

	// Export CSV через engine (если run всё ещё доступен в engine).
	csvData, err := b.engine.ExportCSV(runID)
	if err != nil {
		_, _ = b.client.SendMessage(
			chatID,
			"CSV недоступен (в текущей реализации engine хранит результаты только в памяти процесса).",
			nil,
		)

		return nil
	}

	name := fmt.Sprintf("%s_%s.csv", sanitizeFileName(qz.Title), runID)
	if err := sendDocumentIfPossible(b.client, chatID, name, csvData); err != nil {
		_, _ = b.client.SendMessage(chatID, "Не смог отправить CSV.", nil)
	}

	return nil
}

func sanitizeFileName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "quiz"
	}

	s = strings.ReplaceAll(s, " ", "_")
	s = strings.Map(func(r rune) rune {
		if r == '_' || r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') ||
			(r >= 'a' && r <= 'z') {
			return r
		}

		return '_'
	}, s)

	return s
}

func sendDocumentIfPossible(c Client, chatID int64, fileName string, data []byte) error {
	// We can only send document if underlying client is HTTPClient.
	type docSender interface {
		SendDocument(chatID int64, fileName string, data []byte) error
	}

	ds, ok := c.(docSender)
	if !ok {
		_, _ = c.SendMessage(
			chatID,
			"CSV готов, но клиент не поддерживает отправку документов.",
			nil,
		)

		return errors.New("client doesn't support SendDocument")
	}

	return ds.SendDocument(chatID, fileName, data)
}

func (b *Bot) findParticipantRun(userID int64) string {
	// Slow path: scan known runs.
	// Used only to route A-F answers.
	b.mu.Lock()

	runIDs := make([]string, 0, len(b.runEventsStarted))
	for id := range b.runEventsStarted {
		runIDs = append(runIDs, id)
	}

	b.mu.Unlock()

	for _, runID := range runIDs {
		run, err := b.engine.GetRun(runID)
		if err != nil || run == nil {
			continue
		}

		if run.Status != quiz.RunStatusRunning {
			continue
		}

		if _, ok := run.Participants[userID]; ok {
			return runID
		}
	}

	return ""
}

// Optional helper: allow uploading quizzes back to teacher (advanced part hook).
func encodeQuizJSON(q *quiz.Quiz) ([]byte, error) {
	if q == nil {
		return nil, errors.New("nil quiz")
	}

	out := struct {
		Title     string          `json:"title"`
		Settings  quiz.Settings   `json:"settings"`
		Questions []quiz.Question `json:"questions"`
	}{
		Title:     q.Title,
		Settings:  q.Settings,
		Questions: q.Questions,
	}

	return json.MarshalIndent(out, "", "  ")
}
