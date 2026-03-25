package coolq

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mrs4s/go-cqhttp/db"
	_ "github.com/Mrs4s/go-cqhttp/db/leveldb"
	"github.com/Mrs4s/go-cqhttp/global"
	"github.com/Mrs4s/go-cqhttp/internal/base"
	wx "github.com/Mrs4s/go-cqhttp/internal/weixin"
	"github.com/Mrs4s/go-cqhttp/pkg/onebot"
	"github.com/tidwall/gjson"
	"gopkg.in/yaml.v3"
)

func testWeixinBot() *CQBot {
	rt := NewWeixinRuntime(nil, "bot@im.bot", "bot@im.bot", "", 5*time.Second)
	return NewWeixinBot(rt)
}

func TestMapWeixinIDStable(t *testing.T) {
	id1 := mapWeixinID("user@im.wechat")
	id2 := mapWeixinID("user@im.wechat")
	if id1 <= 0 {
		t.Fatalf("expected positive id, got %d", id1)
	}
	if id1 != id2 {
		t.Fatalf("expected stable id mapping, got %d and %d", id1, id2)
	}
}

func TestWeixinSupportedActionsExcludeGroup(t *testing.T) {
	bot := testWeixinBot()
	resp := bot.CallWeixinAction("get_supported_actions", onebot.V11, func(string) gjson.Result {
		return gjson.Result{}
	})
	if resp["status"] != "ok" {
		t.Fatalf("expected ok response, got %#v", resp)
	}
	actions, ok := resp["data"].([]string)
	if !ok {
		t.Fatalf("expected []string actions, got %#v", resp["data"])
	}
	var hasPrivate, hasGroup bool
	for _, action := range actions {
		if action == "send_private_msg" {
			hasPrivate = true
		}
		if action == "send_group_msg" {
			hasGroup = true
		}
	}
	if !hasPrivate || hasGroup {
		t.Fatalf("unexpected supported actions: %#v", actions)
	}
	var hasUploadPrivate bool
	for _, action := range actions {
		if action == "upload_private_file" {
			hasUploadPrivate = true
			break
		}
	}
	if !hasUploadPrivate {
		t.Fatalf("expected upload_private_file in supported actions, got %#v", actions)
	}
}

func TestWeixinSendMsgRejectsGroup(t *testing.T) {
	bot := testWeixinBot()
	resp := bot.CallWeixinAction("send_msg", onebot.V11, func(key string) gjson.Result {
		switch key {
		case "group_id":
			return gjson.Parse("1")
		case "user_id":
			return gjson.Parse("2")
		case "message":
			return gjson.Parse(`"hello"`)
		default:
			return gjson.Result{}
		}
	})
	if resp["status"] != "failed" {
		t.Fatalf("expected failed response, got %#v", resp)
	}
}

func TestWeixinCQGetStatusDoesNotRequireQQClient(t *testing.T) {
	bot := testWeixinBot()
	resp := bot.CQGetStatus(onebot.V11)
	if resp["status"] != "ok" {
		t.Fatalf("expected ok response, got %#v", resp)
	}
	data, ok := resp["data"].(global.MSG)
	if !ok {
		t.Fatalf("expected status payload, got %#v", resp["data"])
	}
	if _, ok := data["online"].(bool); !ok {
		t.Fatalf("expected online bool, got %#v", data["online"])
	}
}

func TestBuildWeixinSendSegmentsReplyFallback(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tmp := t.TempDir()
	if err := os.Chdir(tmp); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	defer func() {
		_ = os.Chdir(cwd)
	}()

	var node yaml.Node
	if err := yaml.Unmarshal([]byte("enable: true\n"), &node); err != nil {
		t.Fatalf("yaml: %v", err)
	}
	base.Database = map[string]yaml.Node{
		"leveldb": *node.Content[0],
	}
	db.Init()
	if err := db.Open(); err != nil {
		t.Fatalf("db open: %v", err)
	}

	bot := NewWeixinBot(NewWeixinRuntime(nil, "bot@im.bot", "bot", filepath.Join(tmp, "contacts.json"), 5*time.Second))
	messageID := bot.InsertWeixinPrivateMessage("peer@im.wechat", "raw-1", []global.MSG{
		{"type": "text", "data": global.MSG{"text": "hello reply target"}},
	}, true, time.Now().Unix())
	segments, err := bot.buildWeixinSendSegments([]global.MSG{
		{"type": "reply", "data": global.MSG{"id": messageID}},
		{"type": "text", "data": global.MSG{"text": "follow up"}},
	})
	if err != nil {
		t.Fatalf("buildWeixinSendSegments: %v", err)
	}
	if len(segments) < 2 {
		t.Fatalf("expected reply prefix and text segment, got %#v", segments)
	}
	if segments[0].Type != "text" || !strings.Contains(segments[0].Text, "[回复") {
		t.Fatalf("expected reply fallback prefix, got %#v", segments[0])
	}
}

func TestBuildWeixinSendSegmentsFile(t *testing.T) {
	bot := testWeixinBot()
	segments, err := bot.buildWeixinSendSegments([]global.MSG{
		{"type": "file", "data": global.MSG{"url": "https://example.com/path/report.txt?download=1", "name": "custom.txt"}},
	})
	if err != nil {
		t.Fatalf("buildWeixinSendSegments: %v", err)
	}
	if len(segments) != 1 {
		t.Fatalf("expected one segment, got %#v", segments)
	}
	if segments[0].Type != "file" || segments[0].File != "https://example.com/path/report.txt?download=1" || segments[0].Name != "custom.txt" {
		t.Fatalf("unexpected file segment: %#v", segments[0])
	}

	segments, err = bot.buildWeixinSendSegments([]global.MSG{
		{"type": "file", "data": global.MSG{"url": "https://example.com/path/report.txt?download=1"}},
	})
	if err != nil {
		t.Fatalf("buildWeixinSendSegments url fallback: %v", err)
	}
	if segments[0].Name != "report.txt" {
		t.Fatalf("expected fallback file name from url, got %#v", segments[0])
	}
}

func TestWeixinUploadPrivateFile(t *testing.T) {
	tmp := t.TempDir()
	localFile := filepath.Join(tmp, "source.txt")
	if err := os.WriteFile(localFile, []byte("hello-file"), 0o644); err != nil {
		t.Fatalf("write local file: %v", err)
	}

	type requestLog struct {
		UploadURLBodies []string
		SendBodies      []string
	}
	logs := &requestLog{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ilink/bot/getuploadurl":
			body, _ := io.ReadAll(r.Body)
			logs.UploadURLBodies = append(logs.UploadURLBodies, string(body))
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ret":          0,
				"upload_param": "upload-param",
			})
		case "/c2c/upload":
			w.Header().Set("X-Encrypted-Param", "download-param")
			w.WriteHeader(http.StatusOK)
		case "/ilink/bot/sendmessage":
			body, _ := io.ReadAll(r.Body)
			logs.SendBodies = append(logs.SendBodies, string(body))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("{}"))
		default:
			t.Fatalf("unexpected request: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	session := map[string]string{
		"token":        "token-1",
		"self_raw_id":  "bot@im.bot",
		"base_url":     server.URL,
		"cdn_base_url": server.URL + "/c2c",
	}
	sessionData, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "session.json"), sessionData, 0o644); err != nil {
		t.Fatalf("write session: %v", err)
	}

	bridgeClient := wx.NewClient(wx.Config{
		APIBaseURL: server.URL,
		CDNBaseURL: server.URL + "/c2c",
		StateDir:   tmp,
	})
	runtime := NewWeixinRuntime(bridgeClient, "bot@im.bot", "bot", filepath.Join(tmp, "contacts.json"), 5*time.Second)
	contact := runtime.UpsertContact("peer@im.wechat", "ctx-1")
	bot := NewWeixinBot(runtime)

	resp := bot.CallWeixinAction("upload_private_file", onebot.V11, func(key string) gjson.Result {
		switch key {
		case "user_id":
			return gjson.Parse(strconv.FormatInt(contact.ID, 10))
		case "file":
			return gjson.Parse(`"` + localFile + `"`)
		case "name":
			return gjson.Parse(`"renamed.txt"`)
		default:
			return gjson.Result{}
		}
	})
	if resp["status"] != "ok" {
		t.Fatalf("expected ok response, got %#v", resp)
	}
	if len(logs.UploadURLBodies) != 1 || !strings.Contains(logs.UploadURLBodies[0], "\"media_type\":3") {
		t.Fatalf("expected file upload request, got %#v", logs.UploadURLBodies)
	}
	if len(logs.SendBodies) != 1 {
		t.Fatalf("expected one sendmessage call, got %#v", logs.SendBodies)
	}
	if !strings.Contains(logs.SendBodies[0], "\"file_item\"") ||
		!strings.Contains(logs.SendBodies[0], "\"file_name\":\"renamed.txt\"") ||
		!strings.Contains(logs.SendBodies[0], "\"len\":\"10\"") {
		t.Fatalf("unexpected upload_private_file payload: %s", logs.SendBodies[0])
	}
}

func TestWeixinUploadPrivateFileErrors(t *testing.T) {
	bot := testWeixinBot()
	resp := bot.CallWeixinAction("upload_private_file", onebot.V11, func(key string) gjson.Result {
		switch key {
		case "user_id":
			return gjson.Parse("1")
		case "file":
			return gjson.Parse(`"/tmp/missing.txt"`)
		default:
			return gjson.Result{}
		}
	})
	if resp["status"] != "failed" || resp["msg"] != "USER_NOT_FOUND" {
		t.Fatalf("expected failed user-not-found response, got %#v", resp)
	}

	rt := NewWeixinRuntime(nil, "bot@im.bot", "bot", "", 5*time.Second)
	contact := rt.UpsertContact("peer@im.wechat", "")
	bot = NewWeixinBot(rt)
	resp = bot.CallWeixinAction("upload_private_file", onebot.V11, func(key string) gjson.Result {
		switch key {
		case "user_id":
			return gjson.Parse(strconv.FormatInt(contact.ID, 10))
		case "file":
			return gjson.Parse(`"/tmp/missing.txt"`)
		default:
			return gjson.Result{}
		}
	})
	if resp["status"] != "failed" || resp["wording"] != "用户最近没有可用会话" {
		t.Fatalf("expected context token missing response, got %#v", resp)
	}

	contact.LastContextToken = "ctx-1"
	resp = bot.CallWeixinAction("upload_private_file", onebot.V11, func(key string) gjson.Result {
		switch key {
		case "user_id":
			return gjson.Parse(strconv.FormatInt(contact.ID, 10))
		case "file":
			return gjson.Parse(`"/tmp/missing.txt"`)
		default:
			return gjson.Result{}
		}
	})
	if resp["status"] != "failed" || resp["msg"] != "OPEN_FILE_ERROR" {
		t.Fatalf("expected open file error, got %#v", resp)
	}
}
