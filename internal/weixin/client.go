package weixin

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	defaultAPIBaseURL       = "https://ilinkai.weixin.qq.com"
	defaultCDNBaseURL       = "https://novac2c.cdn.weixin.qq.com/c2c"
	defaultStateDir         = "data/weixin"
	defaultPollTimeout      = 35 * time.Second
	defaultQRTimeout        = 480 * time.Second
	defaultQRPollTimeout    = 35 * time.Second
	defaultLoginTTL         = 5 * time.Minute
	defaultMessageLogLength = 200
	upstreamChannelVersion  = "1.0.2"
)

// Config defines the in-process Weixin runtime configuration.
type Config struct {
	APIBaseURL  string
	CDNBaseURL  string
	PollTimeout time.Duration
	QRTimeout   time.Duration
	StateDir    string
}

// Client talks to the Weixin iLink API directly.
type Client struct {
	conf   Config
	client *http.Client

	mu           sync.RWMutex
	session      *sessionState
	activeLogins map[string]*activeLogin
}

type sessionState struct {
	Token      string `json:"token"`
	SelfRawID  string `json:"self_raw_id"`
	BaseURL    string `json:"base_url"`
	CDNBaseURL string `json:"cdn_base_url"`
	SavedAt    string `json:"saved_at,omitempty"`
}

type syncState struct {
	GetUpdatesBuf string `json:"get_updates_buf"`
}

type activeLogin struct {
	QRCode    string
	QRDataURL string
	CreatedAt time.Time
}

type qrCodeResponse struct {
	QRCode    string `json:"qrcode"`
	QRDataURL string `json:"qrcode_img_content"`
}

type qrStatusResponse struct {
	Status     string `json:"status"`
	BotToken   string `json:"bot_token"`
	IlinkBotID string `json:"ilink_bot_id"`
	BaseURL    string `json:"baseurl"`
}

type getUpdatesResponse struct {
	Msgs          []map[string]any `json:"msgs"`
	GetUpdatesBuf string           `json:"get_updates_buf"`
}

type getUploadURLResponse struct {
	Ret         int    `json:"ret"`
	ErrMsg      string `json:"errmsg,omitempty"`
	UploadParam string `json:"upload_param"`
}

type getConfigResponse struct {
	Ret          int    `json:"ret"`
	ErrMsg       string `json:"errmsg,omitempty"`
	TypingTicket string `json:"typing_ticket,omitempty"`
}

type sendTypingResponse struct {
	Ret    int    `json:"ret"`
	ErrMsg string `json:"errmsg,omitempty"`
}

type rawMessageLog struct {
	Direction    string `json:"direction"`
	PeerRawID    string `json:"peer_raw_id"`
	RawMessageID string `json:"raw_message_id"`
	TimeMS       int64  `json:"time_ms"`
	LoggedAt     string `json:"logged_at"`
}

// HealthResponse is returned by Health.
type HealthResponse struct {
	Status    string `json:"status"`
	LoggedIn  bool   `json:"logged_in"`
	SelfRawID string `json:"self_raw_id"`
}

// AuthStartResponse is returned by StartQRLogin.
type AuthStartResponse struct {
	SessionKey string `json:"session_key"`
	QRDataURL  string `json:"qr_data_url"`
}

// AuthWaitResponse is returned by WaitQRLogin.
type AuthWaitResponse struct {
	Connected bool   `json:"connected"`
	SelfRawID string `json:"self_raw_id"`
}

// SelfResponse is returned by Self.
type SelfResponse struct {
	LoggedIn    bool   `json:"logged_in"`
	SelfRawID   string `json:"self_raw_id"`
	DisplayName string `json:"display_name"`
}

// GetConfigResponse is returned by GetConfig.
type GetConfigResponse struct {
	TypingTicket string `json:"typing_ticket"`
}

// Segment describes a normalized bridge segment.
type Segment struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	File string `json:"file,omitempty"`
	Name string `json:"name,omitempty"`
}

// Event is a normalized private message event.
type Event struct {
	Type         string    `json:"type"`
	SelfRawID    string    `json:"self_raw_id"`
	PeerRawID    string    `json:"peer_raw_id"`
	RawMessageID string    `json:"raw_message_id"`
	ContextToken string    `json:"context_token"`
	TimeMS       int64     `json:"time_ms"`
	Segments     []Segment `json:"segments"`
}

// SendPrivateRequest sends a private message through the runtime.
type SendPrivateRequest struct {
	PeerRawID    string    `json:"peer_raw_id"`
	ContextToken string    `json:"context_token"`
	Segments     []Segment `json:"segments"`
}

// SendPrivateResponse is returned by SendPrivate.
type SendPrivateResponse struct {
	RawMessageID string `json:"raw_message_id"`
	ClientID     string `json:"client_id"`
	TimeMS       int64  `json:"time_ms"`
}

// ErrNotLoggedIn indicates the runtime has no valid session.
var ErrNotLoggedIn = errors.New("weixin runtime not logged in")

// NewClient constructs a direct runtime client from config.
func NewClient(conf Config) *Client {
	if conf.APIBaseURL == "" {
		conf.APIBaseURL = defaultAPIBaseURL
	}
	if conf.CDNBaseURL == "" {
		conf.CDNBaseURL = defaultCDNBaseURL
	}
	if conf.PollTimeout <= 0 {
		conf.PollTimeout = defaultPollTimeout
	}
	if conf.QRTimeout <= 0 {
		conf.QRTimeout = defaultQRTimeout
	}
	if strings.TrimSpace(conf.StateDir) == "" {
		conf.StateDir = defaultStateDir
	}
	c := &Client{
		conf: conf,
		client: &http.Client{
			Timeout: 0,
		},
		activeLogins: map[string]*activeLogin{},
	}
	c.ensureStateDirs()
	c.session = c.loadSession()
	return c
}

// StateDir returns the runtime state directory.
func (c *Client) StateDir() string {
	return c.conf.StateDir
}

// ContactStorePath returns the default contact cache path for higher layers.
func (c *Client) ContactStorePath() string {
	return filepath.Join(c.conf.StateDir, "contacts.json")
}

func (c *Client) ensureStateDirs() {
	for _, dir := range []string{
		c.conf.StateDir,
		c.inboundDir(),
		c.tmpDir(),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Warnf("创建 Weixin 状态目录失败: %v", err)
		}
	}
}

func (c *Client) sessionPath() string {
	return filepath.Join(c.conf.StateDir, "session.json")
}

func (c *Client) syncPath() string {
	return filepath.Join(c.conf.StateDir, "sync.json")
}

func (c *Client) messagesPath() string {
	return filepath.Join(c.conf.StateDir, "messages.json")
}

func (c *Client) inboundDir() string {
	return filepath.Join(c.conf.StateDir, "media", "inbound")
}

func (c *Client) tmpDir() string {
	return filepath.Join(c.conf.StateDir, "media", "tmp")
}

func (c *Client) loadSession() *sessionState {
	var session sessionState
	if err := readJSONFile(c.sessionPath(), &session); err != nil {
		return nil
	}
	if session.Token == "" || session.SelfRawID == "" {
		return nil
	}
	if session.BaseURL == "" {
		session.BaseURL = c.conf.APIBaseURL
	}
	if session.CDNBaseURL == "" {
		session.CDNBaseURL = c.conf.CDNBaseURL
	}
	return &session
}

func (c *Client) sessionSnapshot() (*sessionState, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.session == nil || c.session.Token == "" || c.session.SelfRawID == "" {
		return nil, false
	}
	cp := *c.session
	return &cp, true
}

func (c *Client) saveSession(session *sessionState) error {
	if session == nil {
		return nil
	}
	cp := *session
	cp.SavedAt = time.Now().Format(time.RFC3339)
	if err := writeJSONFile(c.sessionPath(), &cp); err != nil {
		return err
	}
	c.mu.Lock()
	c.session = &cp
	c.mu.Unlock()
	return nil
}

func (c *Client) invalidateSession() {
	c.mu.Lock()
	c.session = nil
	c.mu.Unlock()
	_ = os.Remove(c.sessionPath())
	_ = os.Remove(c.syncPath())
}

func (c *Client) loadSyncBuf() string {
	var state syncState
	if err := readJSONFile(c.syncPath(), &state); err != nil {
		return ""
	}
	return state.GetUpdatesBuf
}

func (c *Client) saveSyncBuf(buf string) {
	state := syncState{GetUpdatesBuf: buf}
	if err := writeJSONFile(c.syncPath(), &state); err != nil {
		log.Warnf("写入 Weixin sync 缓存失败: %v", err)
	}
}

func (c *Client) appendMessageLog(entry rawMessageLog) {
	var entries []rawMessageLog
	_ = readJSONFile(c.messagesPath(), &entries)
	entry.LoggedAt = time.Now().Format(time.RFC3339)
	entries = append(entries, entry)
	if len(entries) > defaultMessageLogLength {
		entries = entries[len(entries)-defaultMessageLogLength:]
	}
	if err := writeJSONFile(c.messagesPath(), entries); err != nil {
		log.Warnf("写入 Weixin 消息日志失败: %v", err)
	}
}

func (c *Client) purgeExpiredLogins() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	for key, login := range c.activeLogins {
		if now.Sub(login.CreatedAt) > defaultLoginTTL {
			delete(c.activeLogins, key)
		}
	}
}

// Health checks runtime liveness.
func (c *Client) Health(_ context.Context) (*HealthResponse, error) {
	session, ok := c.sessionSnapshot()
	resp := &HealthResponse{Status: "ok"}
	if ok {
		resp.LoggedIn = true
		resp.SelfRawID = session.SelfRawID
	}
	return resp, nil
}

// Self returns the active bot identity.
func (c *Client) Self(_ context.Context) (*SelfResponse, error) {
	session, ok := c.sessionSnapshot()
	if !ok {
		return nil, ErrNotLoggedIn
	}
	return &SelfResponse{
		LoggedIn:    true,
		SelfRawID:   session.SelfRawID,
		DisplayName: session.SelfRawID,
	}, nil
}

// StartQRLogin starts a QR login session.
func (c *Client) StartQRLogin(ctx context.Context) (*AuthStartResponse, error) {
	c.purgeExpiredLogins()
	qr, err := c.fetchQRCode(ctx)
	if err != nil {
		return nil, err
	}
	sessionKey := randomID("qr")
	c.mu.Lock()
	c.activeLogins[sessionKey] = &activeLogin{
		QRCode:    qr.QRCode,
		QRDataURL: qr.QRDataURL,
		CreatedAt: time.Now(),
	}
	c.mu.Unlock()
	return &AuthStartResponse{
		SessionKey: sessionKey,
		QRDataURL:  qr.QRDataURL,
	}, nil
}

// WaitQRLogin waits for QR login completion.
func (c *Client) WaitQRLogin(ctx context.Context, sessionKey string, timeout time.Duration) (*AuthWaitResponse, error) {
	if timeout <= 0 {
		timeout = c.conf.QRTimeout
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		c.mu.RLock()
		login := c.activeLogins[sessionKey]
		c.mu.RUnlock()
		if login == nil {
			return &AuthWaitResponse{Connected: false}, nil
		}
		remaining := time.Until(deadline)
		pollTimeout := defaultQRPollTimeout
		if remaining < pollTimeout {
			pollTimeout = remaining
		}
		status, err := c.pollQRStatus(ctx, login.QRCode, pollTimeout)
		if err != nil {
			return nil, err
		}
		switch status.Status {
		case "confirmed":
			if status.BotToken == "" || status.IlinkBotID == "" {
				break
			}
			session := &sessionState{
				Token:      status.BotToken,
				SelfRawID:  status.IlinkBotID,
				BaseURL:    c.conf.APIBaseURL,
				CDNBaseURL: c.conf.CDNBaseURL,
			}
			if strings.TrimSpace(status.BaseURL) != "" {
				session.BaseURL = strings.TrimSpace(status.BaseURL)
			}
			if err := c.saveSession(session); err != nil {
				return nil, err
			}
			c.mu.Lock()
			delete(c.activeLogins, sessionKey)
			c.mu.Unlock()
			return &AuthWaitResponse{Connected: true, SelfRawID: session.SelfRawID}, nil
		case "expired":
			c.mu.Lock()
			delete(c.activeLogins, sessionKey)
			c.mu.Unlock()
			return &AuthWaitResponse{Connected: false}, nil
		}
	}
	return &AuthWaitResponse{Connected: false}, nil
}

// Poll pulls private message events from the Weixin API.
func (c *Client) Poll(ctx context.Context, timeout time.Duration) ([]Event, error) {
	session, ok := c.sessionSnapshot()
	if !ok {
		return nil, ErrNotLoggedIn
	}
	if timeout <= 0 {
		timeout = c.conf.PollTimeout
	}
	syncBuf := c.loadSyncBuf()
	resp, err := c.getUpdates(ctx, session, syncBuf, timeout)
	if err != nil {
		return nil, err
	}
	if resp.GetUpdatesBuf != "" {
		c.saveSyncBuf(resp.GetUpdatesBuf)
	}
	events := make([]Event, 0, len(resp.Msgs))
	for _, msg := range resp.Msgs {
		event, err := c.normalizeMessage(ctx, session, msg)
		if err != nil {
			log.Warnf("规范化 Weixin 消息失败: %v", err)
			continue
		}
		if event != nil {
			events = append(events, *event)
		}
	}
	return events, nil
}

// GetConfig fetches account configuration for a peer conversation.
func (c *Client) GetConfig(ctx context.Context, peerRawID, contextToken string) (*GetConfigResponse, error) {
	session, ok := c.sessionSnapshot()
	if !ok {
		return nil, ErrNotLoggedIn
	}
	if strings.TrimSpace(peerRawID) == "" {
		return nil, fmt.Errorf("peer_raw_id is required")
	}
	payload := map[string]any{
		"ilink_user_id": peerRawID,
	}
	if strings.TrimSpace(contextToken) != "" {
		payload["context_token"] = contextToken
	}
	resp, err := c.getConfig(ctx, session, payload)
	if err != nil {
		return nil, err
	}
	return &GetConfigResponse{TypingTicket: resp.TypingTicket}, nil
}

// SendTyping updates the typing indicator for a peer conversation.
func (c *Client) SendTyping(ctx context.Context, peerRawID, typingTicket string, status int) error {
	session, ok := c.sessionSnapshot()
	if !ok {
		return ErrNotLoggedIn
	}
	if strings.TrimSpace(peerRawID) == "" || strings.TrimSpace(typingTicket) == "" {
		return fmt.Errorf("peer_raw_id and typing_ticket are required")
	}
	if status == 0 {
		return fmt.Errorf("status is required")
	}
	return c.sendTyping(ctx, session, map[string]any{
		"ilink_user_id": peerRawID,
		"typing_ticket": typingTicket,
		"status":        status,
	})
}

// SendPrivate sends a private message through the Weixin API.
func (c *Client) SendPrivate(ctx context.Context, req SendPrivateRequest) (*SendPrivateResponse, error) {
	session, ok := c.sessionSnapshot()
	if !ok {
		return nil, ErrNotLoggedIn
	}
	if strings.TrimSpace(req.PeerRawID) == "" || strings.TrimSpace(req.ContextToken) == "" {
		return nil, fmt.Errorf("peer_raw_id and context_token are required")
	}
	if len(req.Segments) == 0 {
		return nil, fmt.Errorf("segments are required")
	}
	lastClientID := ""
	for _, segment := range req.Segments {
		switch segment.Type {
		case "text":
			clientID, err := c.sendTextSegment(ctx, session, req.PeerRawID, req.ContextToken, segment.Text)
			if err != nil {
				return nil, err
			}
			lastClientID = clientID
		case "image", "video", "file":
			clientID, err := c.sendMediaSegment(ctx, session, req.PeerRawID, req.ContextToken, segment)
			if err != nil {
				return nil, err
			}
			lastClientID = clientID
		default:
			return nil, fmt.Errorf("unsupported segment %s", segment.Type)
		}
	}
	now := time.Now().UnixMilli()
	c.appendMessageLog(rawMessageLog{
		Direction:    "outbound",
		PeerRawID:    req.PeerRawID,
		RawMessageID: lastClientID,
		TimeMS:       now,
	})
	return &SendPrivateResponse{
		RawMessageID: lastClientID,
		ClientID:     lastClientID,
		TimeMS:       now,
	}, nil
}

func (c *Client) fetchQRCode(ctx context.Context) (*qrCodeResponse, error) {
	rawURL := strings.TrimRight(c.conf.APIBaseURL, "/") + "/ilink/bot/get_bot_qrcode?bot_type=3"
	var out qrCodeResponse
	if err := c.doJSON(ctx, http.MethodGet, rawURL, nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) pollQRStatus(ctx context.Context, qrCode string, timeout time.Duration) (*qrStatusResponse, error) {
	if timeout <= 0 {
		timeout = defaultQRPollTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	rawURL := strings.TrimRight(c.conf.APIBaseURL, "/") + "/ilink/bot/get_qrcode_status?qrcode=" + url.QueryEscape(qrCode)
	var out qrStatusResponse
	err := c.doJSON(reqCtx, http.MethodGet, rawURL, nil, map[string]string{"iLink-App-ClientVersion": "1"}, &out)
	if isTimeoutError(err) {
		return &qrStatusResponse{Status: "wait"}, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getUpdates(ctx context.Context, session *sessionState, syncBuf string, timeout time.Duration) (*getUpdatesResponse, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var out getUpdatesResponse
	err := c.apiPost(reqCtx, session, "ilink/bot/getupdates", map[string]any{
		"get_updates_buf": syncBuf,
		"base_info":       c.baseInfo(),
	}, &out)
	if isTimeoutError(err) {
		return &getUpdatesResponse{Msgs: []map[string]any{}, GetUpdatesBuf: syncBuf}, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getUploadURL(ctx context.Context, session *sessionState, payload map[string]any) (*getUploadURLResponse, error) {
	payload["base_info"] = c.baseInfo()
	var out getUploadURLResponse
	if err := c.apiPost(ctx, session, "ilink/bot/getuploadurl", payload, &out); err != nil {
		return nil, err
	}
	if out.Ret != 0 {
		return nil, fmt.Errorf("getuploadurl failed: ret=%d errmsg=%s", out.Ret, strings.TrimSpace(out.ErrMsg))
	}
	return &out, nil
}

func (c *Client) getConfig(ctx context.Context, session *sessionState, payload map[string]any) (*getConfigResponse, error) {
	payload["base_info"] = c.baseInfo()
	var out getConfigResponse
	if err := c.apiPost(ctx, session, "ilink/bot/getconfig", payload, &out); err != nil {
		return nil, err
	}
	if out.Ret != 0 {
		return nil, fmt.Errorf("getconfig failed: ret=%d errmsg=%s", out.Ret, strings.TrimSpace(out.ErrMsg))
	}
	return &out, nil
}

func (c *Client) sendTyping(ctx context.Context, session *sessionState, payload map[string]any) error {
	payload["base_info"] = c.baseInfo()
	var out sendTypingResponse
	if err := c.apiPost(ctx, session, "ilink/bot/sendtyping", payload, &out); err != nil {
		return err
	}
	if out.Ret != 0 {
		return fmt.Errorf("sendtyping failed: ret=%d errmsg=%s", out.Ret, strings.TrimSpace(out.ErrMsg))
	}
	return nil
}

func (c *Client) sendMessage(ctx context.Context, session *sessionState, payload map[string]any) error {
	payload["base_info"] = c.baseInfo()
	return c.apiPost(ctx, session, "ilink/bot/sendmessage", payload, nil)
}

func (c *Client) apiPost(ctx context.Context, session *sessionState, endpoint string, payload any, out any) error {
	body, err := marshalJSON(payload)
	if err != nil {
		return err
	}
	rawURL := strings.TrimRight(session.BaseURL, "/") + "/" + endpoint
	headers := c.buildAPIHeaders(session.Token, body)
	err = c.doJSON(ctx, http.MethodPost, rawURL, body, headers, out)
	if err != nil && errors.Is(err, ErrNotLoggedIn) {
		c.invalidateSession()
	}
	return err
}

func (c *Client) buildAPIHeaders(token string, body []byte) map[string]string {
	headers := map[string]string{
		"Content-Type":      "application/json",
		"Content-Length":    strconv.Itoa(len(body)),
		"AuthorizationType": "ilink_bot_token",
		"X-WECHAT-UIN":      randomWechatUin(),
	}
	if strings.TrimSpace(token) != "" {
		headers["Authorization"] = "Bearer " + strings.TrimSpace(token)
	}
	return headers
}

func (c *Client) doJSON(ctx context.Context, method, rawURL string, body []byte, headers map[string]string, out any) error {
	var payload io.Reader
	if len(body) > 0 {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, payload)
	if err != nil {
		return err
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return ErrNotLoggedIn
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("weixin api %s %s failed: %s", method, rawURL, strings.TrimSpace(string(raw)))
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	return decodeJSON(raw, out)
}

func (c *Client) normalizeMessage(ctx context.Context, session *sessionState, msg map[string]any) (*Event, error) {
	if intValue(msg["message_type"]) != 1 {
		return nil, nil
	}
	if groupID, ok := msg["group_id"]; ok && intValue(groupID) != 0 {
		return nil, nil
	}
	itemList := sliceValue(msg["item_list"])
	segments := make([]Segment, 0, len(itemList))
	text := textFromItemList(itemList)
	if text != "" {
		segments = append(segments, Segment{Type: "text", Text: text})
	}
	for _, itemRaw := range itemList {
		item := mapValue(itemRaw)
		switch intValue(item["type"]) {
		case 2:
			query := nestedString(item, "image_item", "media", "encrypt_query_param")
			if query == "" {
				continue
			}
			aesKey := nestedString(item, "image_item", "media", "aes_key")
			if aesKey == "" {
				if hexKey := nestedString(item, "image_item", "aeskey"); hexKey != "" {
					if rawKey, err := hex.DecodeString(hexKey); err == nil {
						aesKey = base64.StdEncoding.EncodeToString(rawKey)
					}
				}
			}
			buffer, err := c.downloadBuffer(ctx, session, query, aesKey)
			if err != nil {
				return nil, err
			}
			file, err := c.saveInboundBuffer("image", fmt.Sprintf("%s.img", messageIDString(msg)), buffer)
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Type: "image", File: file})
		case 3:
			query := nestedString(item, "voice_item", "media", "encrypt_query_param")
			aesKey := nestedString(item, "voice_item", "media", "aes_key")
			if query == "" || aesKey == "" {
				continue
			}
			buffer, err := c.downloadBuffer(ctx, session, query, aesKey)
			if err != nil {
				return nil, err
			}
			file, err := c.saveInboundBuffer("voice", fmt.Sprintf("%s.silk", messageIDString(msg)), buffer)
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Type: "record", File: file})
		case 4:
			query := nestedString(item, "file_item", "media", "encrypt_query_param")
			aesKey := nestedString(item, "file_item", "media", "aes_key")
			if query == "" || aesKey == "" {
				continue
			}
			buffer, err := c.downloadBuffer(ctx, session, query, aesKey)
			if err != nil {
				return nil, err
			}
			name := nestedString(item, "file_item", "file_name")
			if name == "" {
				name = messageIDString(msg) + ".bin"
			}
			file, err := c.saveInboundBuffer("file", filepath.Base(name), buffer)
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Type: "file", File: file, Name: filepath.Base(name)})
		case 5:
			query := nestedString(item, "video_item", "media", "encrypt_query_param")
			aesKey := nestedString(item, "video_item", "media", "aes_key")
			if query == "" || aesKey == "" {
				continue
			}
			buffer, err := c.downloadBuffer(ctx, session, query, aesKey)
			if err != nil {
				return nil, err
			}
			file, err := c.saveInboundBuffer("video", fmt.Sprintf("%s.mp4", messageIDString(msg)), buffer)
			if err != nil {
				return nil, err
			}
			segments = append(segments, Segment{Type: "video", File: file})
		}
	}
	if len(segments) == 0 {
		return nil, nil
	}
	event := &Event{
		Type:         "private_message",
		SelfRawID:    session.SelfRawID,
		PeerRawID:    stringValue(msg["from_user_id"]),
		RawMessageID: messageIDString(msg),
		ContextToken: stringValue(msg["context_token"]),
		TimeMS:       intValue(msg["create_time_ms"]),
		Segments:     segments,
	}
	if event.TimeMS == 0 {
		event.TimeMS = time.Now().UnixMilli()
	}
	c.appendMessageLog(rawMessageLog{
		Direction:    "inbound",
		PeerRawID:    event.PeerRawID,
		RawMessageID: event.RawMessageID,
		TimeMS:       event.TimeMS,
	})
	return event, nil
}

func (c *Client) downloadBuffer(ctx context.Context, session *sessionState, encryptedQueryParam, aesKeyBase64 string) ([]byte, error) {
	rawURL := strings.TrimRight(session.CDNBaseURL, "/") + "/download?encrypted_query_param=" + url.QueryEscape(encryptedQueryParam)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("cdn download failed: %d", resp.StatusCode)
	}
	ciphertext, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if aesKeyBase64 == "" {
		return ciphertext, nil
	}
	key, err := parseAesKey(aesKeyBase64)
	if err != nil {
		return nil, err
	}
	return decryptAESECB(ciphertext, key)
}

func (c *Client) saveInboundBuffer(subdir, filename string, buffer []byte) (string, error) {
	dir := filepath.Join(c.inboundDir(), subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	file := filepath.Join(dir, filepath.Base(filename))
	if err := os.WriteFile(file, buffer, 0o644); err != nil {
		return "", err
	}
	return file, nil
}

func (c *Client) sendTextSegment(ctx context.Context, session *sessionState, peerRawID, contextToken, text string) (string, error) {
	clientID := randomMessageClientID()
	err := c.sendMessage(ctx, session, map[string]any{
		"msg": map[string]any{
			"from_user_id":  "",
			"to_user_id":    peerRawID,
			"client_id":     clientID,
			"message_type":  2,
			"message_state": 2,
			"item_list": []map[string]any{
				{
					"type":      1,
					"text_item": map[string]any{"text": text},
				},
			},
			"context_token": contextToken,
		},
	})
	return clientID, err
}

func (c *Client) sendMediaSegment(ctx context.Context, session *sessionState, peerRawID, contextToken string, segment Segment) (string, error) {
	localPath, err := c.resolveMediaPath(ctx, segment.File, segment.Type)
	if err != nil {
		return "", err
	}
	upload, err := c.uploadMedia(ctx, session, localPath, segment.Type, peerRawID)
	if err != nil {
		return "", err
	}
	clientID := randomMessageClientID()
	item := map[string]any{}
	switch segment.Type {
	case "image":
		item = map[string]any{
			"type": 2,
			"image_item": map[string]any{
				"media": map[string]any{
					"encrypt_query_param": upload.DownloadParam,
					"aes_key":             upload.AESKeyBase64,
					"encrypt_type":        1,
				},
				"mid_size": upload.CiphertextSize,
			},
		}
	case "video":
		item = map[string]any{
			"type": 5,
			"video_item": map[string]any{
				"media": map[string]any{
					"encrypt_query_param": upload.DownloadParam,
					"aes_key":             upload.AESKeyBase64,
					"encrypt_type":        1,
				},
				"video_size": upload.CiphertextSize,
			},
		}
	case "file":
		item = map[string]any{
			"type": 4,
			"file_item": map[string]any{
				"media": map[string]any{
					"encrypt_query_param": upload.DownloadParam,
					"aes_key":             upload.AESKeyBase64,
					"encrypt_type":        1,
				},
				"file_name": resolveSegmentName(segment, localPath),
				"len":       strconv.Itoa(upload.PlaintextSize),
			},
		}
	default:
		return "", fmt.Errorf("unsupported media segment %s", segment.Type)
	}
	err = c.sendMessage(ctx, session, map[string]any{
		"msg": map[string]any{
			"from_user_id":  "",
			"to_user_id":    peerRawID,
			"client_id":     clientID,
			"message_type":  2,
			"message_state": 2,
			"item_list":     []map[string]any{item},
			"context_token": contextToken,
		},
	})
	return clientID, err
}

type uploadedMedia struct {
	DownloadParam  string
	AESKeyBase64   string
	CiphertextSize int
	PlaintextSize  int
}

func (c *Client) uploadMedia(ctx context.Context, session *sessionState, filePath, mediaType, toUserID string) (*uploadedMedia, error) {
	plaintext, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	fileKey := randomHex(16)
	aesKey := randomBytes(16)
	rawMD5 := md5.Sum(plaintext)
	request := map[string]any{
		"filekey":       fileKey,
		"media_type":    mediaTypeCode(mediaType),
		"to_user_id":    toUserID,
		"rawsize":       len(plaintext),
		"rawfilemd5":    hex.EncodeToString(rawMD5[:]),
		"filesize":      aesECBSize(len(plaintext)),
		"no_need_thumb": true,
		"aeskey":        hex.EncodeToString(aesKey),
	}
	resp, err := c.getUploadURL(ctx, session, request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(resp.UploadParam) == "" {
		return nil, fmt.Errorf("missing upload_param")
	}
	uploaded, err := c.uploadBuffer(ctx, session.CDNBaseURL, resp.UploadParam, fileKey, plaintext, aesKey)
	if err != nil {
		return nil, err
	}
	result := &uploadedMedia{
		DownloadParam:  uploaded.DownloadParam,
		AESKeyBase64:   base64.StdEncoding.EncodeToString([]byte(hex.EncodeToString(aesKey))),
		CiphertextSize: uploaded.CiphertextSize,
		PlaintextSize:  len(plaintext),
	}
	return result, nil
}

type uploadedBuffer struct {
	DownloadParam  string
	CiphertextSize int
}

func (c *Client) uploadBuffer(ctx context.Context, cdnBaseURL, uploadParam, fileKey string, plaintext, aesKey []byte) (*uploadedBuffer, error) {
	ciphertext, err := encryptAESECB(plaintext, aesKey)
	if err != nil {
		return nil, err
	}
	uploadURL := strings.TrimRight(cdnBaseURL, "/") + "/upload?encrypted_query_param=" + url.QueryEscape(uploadParam) + "&filekey=" + url.QueryEscape(fileKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, uploadURL, bytes.NewReader(ciphertext))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	httpResp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
		return nil, fmt.Errorf("cdn upload failed: %d %s", httpResp.StatusCode, strings.TrimSpace(string(raw)))
	}
	downloadParam := httpResp.Header.Get("X-Encrypted-Param")
	if downloadParam == "" {
		return nil, fmt.Errorf("missing x-encrypted-param")
	}
	return &uploadedBuffer{
		DownloadParam:  downloadParam,
		CiphertextSize: len(ciphertext),
	}, nil
}

func (c *Client) resolveMediaPath(ctx context.Context, rawPath, mediaType string) (string, error) {
	if strings.TrimSpace(rawPath) == "" {
		return "", fmt.Errorf("segment missing file")
	}
	if strings.HasPrefix(rawPath, "http://") || strings.HasPrefix(rawPath, "https://") {
		return c.downloadRemoteToTemp(ctx, rawPath, mediaType)
	}
	if strings.HasPrefix(rawPath, "file://") {
		parsed, err := url.Parse(rawPath)
		if err != nil {
			return "", err
		}
		return parsed.Path, nil
	}
	if filepath.IsAbs(rawPath) {
		return rawPath, nil
	}
	return filepath.Abs(rawPath)
}

func (c *Client) downloadRemoteToTemp(ctx context.Context, remoteURL, mediaType string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, remoteURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("remote download failed: %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", err
	}
	u, _ := url.Parse(remoteURL)
	ext := path.Ext(u.Path)
	if ext == "" {
		switch mediaType {
		case "image":
			ext = ".img"
		case "video":
			ext = ".mp4"
		default:
			ext = ".bin"
		}
	}
	file := filepath.Join(c.tmpDir(), fmt.Sprintf("%d-%s%s", time.Now().UnixNano(), randomHex(4), ext))
	if err := os.WriteFile(file, data, 0o644); err != nil {
		return "", err
	}
	return file, nil
}

func (c *Client) baseInfo() map[string]any {
	return map[string]any{"channel_version": upstreamChannelVersion}
}

func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeJSON(data, out)
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func marshalJSON(value any) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	enc := json.NewEncoder(buf)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSpace(buf.Bytes()), nil
}

func decodeJSON(raw []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

func randomWechatUin() string {
	raw := randomBytes(4)
	n := binary.BigEndian.Uint32(raw)
	return base64.StdEncoding.EncodeToString([]byte(strconv.FormatUint(uint64(n), 10)))
}

func randomID(prefix string) string {
	return prefix + "-" + randomHex(8)
}

func randomMessageClientID() string {
	return fmt.Sprintf("openclaw-weixin:%d-%s", time.Now().UnixMilli(), randomHex(4))
}

func randomHex(n int) string {
	return hex.EncodeToString(randomBytes(n))
}

func randomBytes(n int) []byte {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return buf
}

func parseAesKey(aesKeyBase64 string) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(aesKeyBase64)
	if err != nil {
		return nil, err
	}
	switch {
	case len(decoded) == 16:
		return decoded, nil
	case len(decoded) == 32:
		hexString := string(decoded)
		if _, err := hex.DecodeString(hexString); err != nil {
			return nil, fmt.Errorf("invalid aes_key")
		}
		return hex.DecodeString(hexString)
	default:
		return nil, fmt.Errorf("invalid aes_key")
	}
}

func encryptAESECB(plaintext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plaintext = pkcs7Pad(plaintext, block.BlockSize())
	ciphertext := make([]byte, len(plaintext))
	for start := 0; start < len(plaintext); start += block.BlockSize() {
		block.Encrypt(ciphertext[start:start+block.BlockSize()], plaintext[start:start+block.BlockSize()])
	}
	return ciphertext, nil
}

func decryptAESECB(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	if len(ciphertext)%block.BlockSize() != 0 {
		return nil, fmt.Errorf("invalid ciphertext length")
	}
	plaintext := make([]byte, len(ciphertext))
	for start := 0; start < len(ciphertext); start += block.BlockSize() {
		block.Decrypt(plaintext[start:start+block.BlockSize()], ciphertext[start:start+block.BlockSize()])
	}
	return pkcs7Unpad(plaintext, block.BlockSize())
}

func pkcs7Pad(in []byte, blockSize int) []byte {
	padding := blockSize - (len(in) % blockSize)
	if padding == 0 {
		padding = blockSize
	}
	out := make([]byte, len(in)+padding)
	copy(out, in)
	for i := len(in); i < len(out); i++ {
		out[i] = byte(padding)
	}
	return out
}

func pkcs7Unpad(in []byte, blockSize int) ([]byte, error) {
	if len(in) == 0 || len(in)%blockSize != 0 {
		return nil, fmt.Errorf("invalid padded plaintext")
	}
	padding := int(in[len(in)-1])
	if padding == 0 || padding > blockSize || padding > len(in) {
		return nil, fmt.Errorf("invalid padding")
	}
	for _, v := range in[len(in)-padding:] {
		if int(v) != padding {
			return nil, fmt.Errorf("invalid padding")
		}
	}
	return in[:len(in)-padding], nil
}

func aesECBSize(plaintextSize int) int {
	padding := 16 - (plaintextSize % 16)
	if padding == 0 {
		padding = 16
	}
	return plaintextSize + padding
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	type timeout interface {
		Timeout() bool
	}
	var te timeout
	return errors.As(err, &te) && te.Timeout()
}

func mediaTypeCode(mediaType string) int {
	switch mediaType {
	case "image":
		return 1
	case "video":
		return 2
	default:
		return 3
	}
}

func resolveSegmentName(segment Segment, localPath string) string {
	if name := strings.TrimSpace(segment.Name); name != "" {
		return filepath.Base(name)
	}
	rawPath := strings.TrimSpace(segment.File)
	if rawPath != "" {
		switch {
		case strings.HasPrefix(rawPath, "http://"), strings.HasPrefix(rawPath, "https://"):
			if parsed, err := url.Parse(rawPath); err == nil {
				if name := path.Base(parsed.Path); name != "." && name != "/" && name != "" {
					return name
				}
			}
		case strings.HasPrefix(rawPath, "file://"):
			if parsed, err := url.Parse(rawPath); err == nil {
				if name := filepath.Base(parsed.Path); name != "." && name != string(filepath.Separator) && name != "" {
					return name
				}
			}
		default:
			if name := filepath.Base(rawPath); name != "." && name != string(filepath.Separator) && name != "" {
				return name
			}
		}
	}
	if name := filepath.Base(localPath); name != "." && name != string(filepath.Separator) && name != "" {
		return name
	}
	return "file"
}

func textFromItemList(itemList []any) string {
	lines := make([]string, 0, len(itemList))
	for _, itemRaw := range itemList {
		item := mapValue(itemRaw)
		switch intValue(item["type"]) {
		case 1:
			text := nestedString(item, "text_item", "text")
			if title := nestedString(item, "ref_msg", "title"); title != "" {
				text = "[引用: " + title + "]\n" + text
			}
			if strings.TrimSpace(text) != "" {
				lines = append(lines, text)
			}
		case 3:
			text := nestedString(item, "voice_item", "text")
			if strings.TrimSpace(text) != "" {
				lines = append(lines, text)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func messageIDString(msg map[string]any) string {
	if id := stringValue(msg["message_id"]); id != "" {
		return id
	}
	if id := stringValue(msg["client_id"]); id != "" {
		return id
	}
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

func sliceValue(v any) []any {
	if v == nil {
		return nil
	}
	if out, ok := v.([]any); ok {
		return out
	}
	return nil
}

func mapValue(v any) map[string]any {
	if out, ok := v.(map[string]any); ok {
		return out
	}
	return nil
}

func nestedString(root map[string]any, keys ...string) string {
	var current any = root
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = m[key]
	}
	return stringValue(current)
}

func stringValue(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case uint64:
		return strconv.FormatUint(t, 10)
	default:
		return fmt.Sprint(t)
	}
}

func intValue(v any) int64 {
	switch t := v.(type) {
	case nil:
		return 0
	case int:
		return int64(t)
	case int64:
		return t
	case int32:
		return int64(t)
	case uint64:
		return int64(t)
	case float64:
		return int64(t)
	case json.Number:
		i, _ := t.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		return i
	default:
		i, _ := strconv.ParseInt(fmt.Sprint(t), 10, 64)
		return i
	}
}
