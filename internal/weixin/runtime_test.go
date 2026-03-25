package weixin

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRandomWechatUin(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(randomWechatUin())
	if err != nil {
		t.Fatalf("decode random wechat uin: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("expected non-empty decoded header")
	}
	for _, ch := range raw {
		if ch < '0' || ch > '9' {
			t.Fatalf("expected decimal string, got %q", string(raw))
		}
	}
}

func TestAESRoundTripAndParseKey(t *testing.T) {
	key := []byte("0123456789abcdef")
	enc, err := encryptAESECB([]byte("hello weixin"), key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	dec, err := decryptAESECB(enc, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(dec) != "hello weixin" {
		t.Fatalf("unexpected plaintext: %q", string(dec))
	}

	parsed, err := parseAesKey(base64.StdEncoding.EncodeToString(key))
	if err != nil {
		t.Fatalf("parse raw key: %v", err)
	}
	if string(parsed) != string(key) {
		t.Fatalf("unexpected parsed key")
	}
	hexWrapped := base64.StdEncoding.EncodeToString([]byte("00112233445566778899aabbccddeeff"))
	parsed, err = parseAesKey(hexWrapped)
	if err != nil {
		t.Fatalf("parse hex key: %v", err)
	}
	if got := strings.ToLower(hex.EncodeToString(parsed)); got != "00112233445566778899aabbccddeeff" {
		t.Fatalf("unexpected parsed hex key: %s", got)
	}
}

func TestLoginPollSendAndPersistence(t *testing.T) {
	tmp := t.TempDir()
	imagePath := filepath.Join(tmp, "image.jpg")
	if err := os.WriteFile(imagePath, []byte("image-bytes"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}
	videoPath := filepath.Join(tmp, "video.mp4")
	if err := os.WriteFile(videoPath, []byte("video-bytes"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}
	filePath := filepath.Join(tmp, "note.txt")
	if err := os.WriteFile(filePath, []byte("file-bytes"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	type requestLog struct {
		UploadAuthType  string
		UploadCount     int
		UploadURLBodies []string
		SendBodies      []string
	}
	logs := &requestLog{}
	currentStatus := "confirmed"
	pollUnauthorized := false

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/ilink/bot/get_bot_qrcode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"qrcode":             "qr-code",
				"qrcode_img_content": "weixin-qr-content",
			})
		case r.URL.Path == "/ilink/bot/get_qrcode_status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       currentStatus,
				"bot_token":    "token-1",
				"ilink_bot_id": "bot@im.bot",
				"baseurl":      server.URL,
			})
		case r.URL.Path == "/ilink/bot/getupdates":
			if pollUnauthorized {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), "get_updates_buf") {
				t.Fatalf("missing get_updates_buf in poll body: %s", string(body))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"get_updates_buf": "sync-2",
				"msgs": []map[string]any{
					{
						"message_type":   1,
						"from_user_id":   "peer@im.wechat",
						"message_id":     1001,
						"context_token":  "ctx-1",
						"create_time_ms": 1234567,
						"item_list": []map[string]any{
							{"type": 1, "text_item": map[string]any{"text": "hello"}},
						},
					},
				},
			})
		case r.URL.Path == "/ilink/bot/getuploadurl":
			body, _ := io.ReadAll(r.Body)
			logs.UploadURLBodies = append(logs.UploadURLBodies, string(body))
			_ = json.NewEncoder(w).Encode(map[string]any{"upload_param": "upload-param"})
		case r.URL.Path == "/c2c/upload":
			logs.UploadAuthType = r.Header.Get("Content-Type")
			logs.UploadCount++
			w.Header().Set("X-Encrypted-Param", "download-param")
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/ilink/bot/sendmessage":
			body, _ := io.ReadAll(r.Body)
			logs.SendBodies = append(logs.SendBodies, string(body))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		APIBaseURL: server.URL,
		CDNBaseURL: server.URL + "/c2c",
		StateDir:   tmp,
	})
	start, err := client.StartQRLogin(context.Background())
	if err != nil {
		t.Fatalf("start qr login: %v", err)
	}
	if start.QRDataURL != "weixin-qr-content" {
		t.Fatalf("unexpected qr data: %s", start.QRDataURL)
	}
	wait, err := client.WaitQRLogin(context.Background(), start.SessionKey, time.Second)
	if err != nil {
		t.Fatalf("wait qr login: %v", err)
	}
	if !wait.Connected || wait.SelfRawID != "bot@im.bot" {
		t.Fatalf("unexpected wait response: %#v", wait)
	}
	self, err := client.Self(context.Background())
	if err != nil {
		t.Fatalf("self: %v", err)
	}
	if !self.LoggedIn || self.SelfRawID != "bot@im.bot" {
		t.Fatalf("unexpected self: %#v", self)
	}
	if _, err := os.Stat(filepath.Join(tmp, "session.json")); err != nil {
		t.Fatalf("expected session file: %v", err)
	}

	events, err := client.Poll(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(events) != 1 || events[0].PeerRawID != "peer@im.wechat" {
		t.Fatalf("unexpected events: %#v", events)
	}
	if events[0].Segments[0].Type != "text" || events[0].Segments[0].Text != "hello" {
		t.Fatalf("unexpected segments: %#v", events[0].Segments)
	}
	if _, err := os.Stat(filepath.Join(tmp, "sync.json")); err != nil {
		t.Fatalf("expected sync file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, "messages.json")); err != nil {
		t.Fatalf("expected messages file: %v", err)
	}

	send, err := client.SendPrivate(context.Background(), SendPrivateRequest{
		PeerRawID:    "peer@im.wechat",
		ContextToken: "ctx-1",
		Segments: []Segment{
			{Type: "text", Text: "reply"},
			{Type: "image", File: imagePath},
			{Type: "video", File: videoPath},
			{Type: "file", File: filePath},
		},
	})
	if err != nil {
		t.Fatalf("send private: %v", err)
	}
	if send.RawMessageID == "" {
		t.Fatalf("expected raw message id")
	}
	if len(logs.SendBodies) != 4 {
		t.Fatalf("expected four sendmessage calls, got %d", len(logs.SendBodies))
	}
	if logs.UploadAuthType != "application/octet-stream" {
		t.Fatalf("unexpected upload content type: %s", logs.UploadAuthType)
	}
	if logs.UploadCount != 3 {
		t.Fatalf("expected three CDN uploads (image+video+file), got %d", logs.UploadCount)
	}
	if len(logs.UploadURLBodies) != 3 {
		t.Fatalf("expected three getuploadurl calls, got %d", len(logs.UploadURLBodies))
	}
	if !strings.Contains(logs.UploadURLBodies[0], "\"no_need_thumb\":true") {
		t.Fatalf("expected image upload request to include no_need_thumb=true, got %s", logs.UploadURLBodies[0])
	}
	if strings.Contains(logs.UploadURLBodies[0], "thumb_rawsize") ||
		strings.Contains(logs.UploadURLBodies[0], "thumb_rawfilemd5") ||
		strings.Contains(logs.UploadURLBodies[0], "thumb_filesize") {
		t.Fatalf("did not expect image upload request to include thumb metadata, got %s", logs.UploadURLBodies[0])
	}
	if strings.Contains(logs.UploadURLBodies[1], "thumb_rawsize") ||
		strings.Contains(logs.UploadURLBodies[1], "thumb_rawfilemd5") ||
		strings.Contains(logs.UploadURLBodies[1], "thumb_filesize") {
		t.Fatalf("did not expect video upload request to include thumb metadata, got %s", logs.UploadURLBodies[1])
	}
	var imageUploadReq map[string]any
	if err := json.Unmarshal([]byte(logs.UploadURLBodies[0]), &imageUploadReq); err != nil {
		t.Fatalf("unmarshal image upload request: %v", err)
	}
	aesKeyHex, _ := imageUploadReq["aeskey"].(string)
	expectedAESKey := base64.StdEncoding.EncodeToString([]byte(aesKeyHex))
	if !strings.Contains(logs.SendBodies[1], expectedAESKey) {
		t.Fatalf("expected image payload aes_key to match weclaw encoding, got %s", logs.SendBodies[1])
	}
	if strings.Contains(logs.SendBodies[1], "thumb_media") ||
		strings.Contains(logs.SendBodies[1], "\"hd_size\"") ||
		strings.Contains(logs.SendBodies[1], "\"aeskey\"") {
		t.Fatalf("did not expect image payload to include custom image fields, got %s", logs.SendBodies[1])
	}
	if !strings.Contains(logs.SendBodies[0], "\"channel_version\":\"1.0.2\"") {
		t.Fatalf("expected upstream channel version in text payload, got %s", logs.SendBodies[0])
	}
	if !strings.Contains(logs.SendBodies[0], "\"client_id\":\"openclaw-weixin:") {
		t.Fatalf("expected upstream-style client_id in text payload, got %s", logs.SendBodies[0])
	}
	if !strings.Contains(logs.SendBodies[2], "video_item") {
		t.Fatalf("expected video payload, got %s", logs.SendBodies[2])
	}
	if !strings.Contains(logs.UploadURLBodies[2], "\"media_type\":3") {
		t.Fatalf("expected file upload request to use media_type=3, got %s", logs.UploadURLBodies[2])
	}
	if !strings.Contains(logs.SendBodies[3], "\"file_item\"") {
		t.Fatalf("expected file payload, got %s", logs.SendBodies[3])
	}
	if !strings.Contains(logs.SendBodies[3], "\"file_name\":\"note.txt\"") {
		t.Fatalf("expected file payload to include file_name, got %s", logs.SendBodies[3])
	}
	if !strings.Contains(logs.SendBodies[3], "\"len\":\"10\"") {
		t.Fatalf("expected file payload to include plaintext len, got %s", logs.SendBodies[3])
	}

	pollUnauthorized = true
	if _, err := client.Poll(context.Background(), time.Second); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("expected ErrNotLoggedIn, got %v", err)
	}
	if _, err := client.Self(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("expected cleared session, got %v", err)
	}
}

func TestGetConfigAndSendTyping(t *testing.T) {
	tmp := t.TempDir()
	var configBody string
	var typingBodies []string

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"qrcode":             "qr-code",
				"qrcode_img_content": "weixin-qr-content",
			})
		case "/ilink/bot/get_qrcode_status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       "confirmed",
				"bot_token":    "token-1",
				"ilink_bot_id": "bot@im.bot",
				"baseurl":      server.URL,
			})
		case "/ilink/bot/getconfig":
			body, _ := io.ReadAll(r.Body)
			configBody = string(body)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ret":           0,
				"typing_ticket": "ticket-1",
			})
		case "/ilink/bot/sendtyping":
			body, _ := io.ReadAll(r.Body)
			typingBodies = append(typingBodies, string(body))
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 0})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		APIBaseURL: server.URL,
		CDNBaseURL: server.URL + "/c2c",
		StateDir:   tmp,
	})
	start, err := client.StartQRLogin(context.Background())
	if err != nil {
		t.Fatalf("start qr login: %v", err)
	}
	if _, err := client.WaitQRLogin(context.Background(), start.SessionKey, time.Second); err != nil {
		t.Fatalf("wait qr login: %v", err)
	}

	cfg, err := client.GetConfig(context.Background(), "peer@im.wechat", "ctx-1")
	if err != nil {
		t.Fatalf("get config: %v", err)
	}
	if cfg.TypingTicket != "ticket-1" {
		t.Fatalf("unexpected typing ticket: %+v", cfg)
	}
	if !strings.Contains(configBody, "\"ilink_user_id\":\"peer@im.wechat\"") || !strings.Contains(configBody, "\"context_token\":\"ctx-1\"") {
		t.Fatalf("unexpected getconfig body: %s", configBody)
	}

	if err := client.SendTyping(context.Background(), "peer@im.wechat", "ticket-1", 1); err != nil {
		t.Fatalf("send typing start: %v", err)
	}
	if err := client.SendTyping(context.Background(), "peer@im.wechat", "ticket-1", 2); err != nil {
		t.Fatalf("send typing cancel: %v", err)
	}
	if len(typingBodies) != 2 {
		t.Fatalf("expected two sendtyping calls, got %d", len(typingBodies))
	}
	if !strings.Contains(typingBodies[0], "\"status\":1") || !strings.Contains(typingBodies[1], "\"status\":2") {
		t.Fatalf("unexpected sendtyping bodies: %#v", typingBodies)
	}
}

func TestSendPrivateIgnoresBusinessRetWhenHTTP200(t *testing.T) {
	tmp := t.TempDir()
	imagePath := filepath.Join(tmp, "image.jpg")
	if err := os.WriteFile(imagePath, []byte("image-bytes"), 0o644); err != nil {
		t.Fatalf("write image: %v", err)
	}

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/get_bot_qrcode":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"qrcode":             "qr-code",
				"qrcode_img_content": "weixin-qr-content",
			})
		case "/ilink/bot/get_qrcode_status":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status":       "confirmed",
				"bot_token":    "token-1",
				"ilink_bot_id": "bot@im.bot",
				"baseurl":      server.URL,
			})
		case "/ilink/bot/getuploadurl":
			_ = json.NewEncoder(w).Encode(map[string]any{"upload_param": "upload-param"})
		case "/c2c/upload":
			w.Header().Set("X-Encrypted-Param", "download-param")
			w.WriteHeader(http.StatusOK)
		case "/ilink/bot/sendmessage":
			_ = json.NewEncoder(w).Encode(map[string]any{"ret": 1001, "errmsg": "bad image"})
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClient(Config{
		APIBaseURL: server.URL,
		CDNBaseURL: server.URL + "/c2c",
		StateDir:   tmp,
	})
	start, err := client.StartQRLogin(context.Background())
	if err != nil {
		t.Fatalf("start qr login: %v", err)
	}
	if _, err := client.WaitQRLogin(context.Background(), start.SessionKey, time.Second); err != nil {
		t.Fatalf("wait qr login: %v", err)
	}

	resp, err := client.SendPrivate(context.Background(), SendPrivateRequest{
		PeerRawID:    "peer@im.wechat",
		ContextToken: "ctx-1",
		Segments:     []Segment{{Type: "image", File: imagePath}},
	})
	if err != nil {
		t.Fatalf("expected HTTP 200 sendmessage response to be accepted, got %v", err)
	}
	if resp == nil || resp.ClientID == "" {
		t.Fatalf("expected send response metadata, got %+v", resp)
	}
}
