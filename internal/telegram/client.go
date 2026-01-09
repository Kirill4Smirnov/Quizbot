//go:build !solution

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"time"
)

const (
	apiURL  = "https://api.telegram.org/bot%s/%s"
	fileURL = "https://api.telegram.org/file/bot%s/%s"
)

type HTTPClient struct {
	token      string
	httpClient *http.Client
}

func NewHTTPClient(token string) *HTTPClient {
	return &HTTPClient{
		token:      token,
		httpClient: &http.Client{},
	}
}

func (c *HTTPClient) SendMessage(chatID int64, text string, opts *SendOptions) (*Message, error) {
	params := map[string]interface{}{
		"chat_id": chatID,
		"text":    text,
	}
	if opts != nil {
		params["parse_mode"] = opts.ParseMode
		if opts.ReplyMarkup != nil {
			params["reply_markup"] = opts.ReplyMarkup
		}
	}

	raw, err := c.doRequestTimeout("sendMessage", params, 10*time.Second)
	if err != nil {
		return nil, err
	}

	var msg Message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return nil, err
	}

	return &msg, nil
}

func (c *HTTPClient) EditMessage(
	chatID int64,
	messageID int,
	text string,
	opts *SendOptions,
) error {
	params := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	}
	if opts != nil {
		params["parse_mode"] = opts.ParseMode
		if opts.ReplyMarkup != nil {
			params["reply_markup"] = opts.ReplyMarkup
		}
	}

	_, err := c.doRequestTimeout("editMessageText", params, 10*time.Second)

	return err
}

func (c *HTTPClient) DeleteMessage(chatID int64, messageID int) error {
	params := map[string]interface{}{
		"chat_id":    chatID,
		"message_id": messageID,
	}
	_, err := c.doRequestTimeout("deleteMessage", params, 10*time.Second)

	return err
}

func (c *HTTPClient) AnswerCallback(callbackID string, text string) error {
	params := map[string]interface{}{
		"callback_query_id": callbackID,
	}
	if text != "" {
		params["text"] = text
	}

	_, err := c.doRequestTimeout("answerCallbackQuery", params, 10*time.Second)

	return err
}

func (c *HTTPClient) GetUpdates(offset int, timeout int) ([]Update, error) {
	params := map[string]interface{}{}
	if offset > 0 {
		params["offset"] = offset
	}

	if timeout > 0 {
		params["timeout"] = timeout
	}

	raw, err := c.doRequestTimeout("getUpdates", params, time.Duration(timeout+10)*time.Second)
	if err != nil {
		return nil, err
	}

	var updates []Update
	if err := json.Unmarshal(raw, &updates); err != nil {
		return nil, err
	}

	return updates, nil
}

func (c *HTTPClient) GetFile(fileID string) (string, error) {
	raw, err := c.doRequestTimeout(
		"getFile",
		map[string]interface{}{"file_id": fileID},
		10*time.Second,
	)
	if err != nil {
		return "", err
	}

	var res struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", err
	}

	if res.FilePath == "" {
		return "", fmt.Errorf("telegram api error: empty file_path")
	}

	return res.FilePath, nil
}

func (c *HTTPClient) DownloadFile(filePath string) ([]byte, error) {
	u := fmt.Sprintf(fileURL, c.token, filePath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("telegram file download failed: %s: %s", resp.Status, string(b))
	}

	return io.ReadAll(resp.Body)
}

func (c *HTTPClient) SendDocument(chatID int64, fileName string, data []byte) error {
	var buf bytes.Buffer

	w := multipart.NewWriter(&buf)

	_ = w.WriteField("chat_id", strconv.FormatInt(chatID, 10))

	part, err := w.CreateFormFile("document", fileName)
	if err != nil {
		_ = w.Close()
		return err
	}

	if _, writeErr := part.Write(data); writeErr != nil {
		_ = w.Close()
		return writeErr
	}

	if closeErr := w.Close(); closeErr != nil {
		return closeErr
	}

	endpoint := fmt.Sprintf(apiURL, c.token, "sendDocument")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, &buf)
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	var result struct {
		OK     bool            `json:"ok"`
		Error  string          `json:"description"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return err
	}

	if !result.OK {
		return fmt.Errorf("telegram api error: %s", result.Error)
	}

	return nil
}

func (c *HTTPClient) doRequestTimeout(
	method string,
	params map[string]interface{},
	timeout time.Duration,
) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return c.doRequestCtx(ctx, method, params)
}

func (c *HTTPClient) doRequestCtx(
	ctx context.Context,
	method string,
	params map[string]interface{},
) (json.RawMessage, error) {
	url := fmt.Sprintf(apiURL, c.token, method)

	body, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var result struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"description"`
	}

	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}

	if !result.OK {
		return nil, fmt.Errorf("telegram api error: %s", result.Error)
	}

	return result.Result, nil
}

// DeleteWebhook disables webhook mode so long polling via getUpdates works.
// See: https://core.telegram.org/bots/api#deletewebhook
func (c *HTTPClient) DeleteWebhook(dropPendingUpdates bool) error {
	params := map[string]interface{}{}
	if dropPendingUpdates {
		params["drop_pending_updates"] = true
	}

	_, err := c.doRequestTimeout("deleteWebhook", params, 5*time.Second)

	return err
}

// GetMe validates the token and returns basic bot info.
func (c *HTTPClient) GetMe() (*User, error) {
	raw, err := c.doRequestTimeout("getMe", map[string]interface{}{}, 5*time.Second)
	if err != nil {
		return nil, err
	}

	var u User
	if err := json.Unmarshal(raw, &u); err != nil {
		return nil, err
	}

	return &u, nil
}

// buildDeepLink builds a deep-link URL for bot start parameter (used by the bot, but handy here too).
func buildDeepLink(botUsername string, startParam string) string {
	u := url.URL{
		Scheme: "https",
		Host:   "t.me",
		Path:   path.Join("/", botUsername),
	}
	q := u.Query()
	q.Set("start", startParam)
	u.RawQuery = q.Encode()

	return u.String()
}
