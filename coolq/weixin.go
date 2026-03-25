package coolq

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/Mrs4s/go-cqhttp/db"
	"github.com/Mrs4s/go-cqhttp/global"
	"github.com/Mrs4s/go-cqhttp/internal/base"
	"github.com/Mrs4s/go-cqhttp/internal/msg"
	wx "github.com/Mrs4s/go-cqhttp/internal/weixin"
)

var weixinSupportedActions = []string{
	".handle_quick_operation",
	"can_send_image",
	"can_send_record",
	"download_file",
	"get_friend_list",
	"get_login_info",
	"get_msg",
	"get_status",
	"get_stranger_info",
	"get_supported_actions",
	"get_version_info",
	"send_msg",
	"send_private_msg",
	"upload_private_file",
}

// WeixinContact stores observed private contacts.
type WeixinContact struct {
	RawID            string `json:"raw_id"`
	ID               int64  `json:"id"`
	Nickname         string `json:"nickname"`
	LastSeen         int64  `json:"last_seen"`
	LastContextToken string `json:"last_context_token"`
}

// WeixinRuntime holds in-process Weixin runtime state used by CQBot.
type WeixinRuntime struct {
	Client       *wx.Client
	SelfRawID    string
	DisplayName  string
	SelfMappedID int64
	PollTimeout  time.Duration
	ContactStore string

	mu          sync.RWMutex
	contacts    map[int64]*WeixinContact
	contactsRaw map[string]*WeixinContact
}

// NewWeixinRuntime creates a new Weixin runtime and loads persisted contacts.
func NewWeixinRuntime(cli *wx.Client, selfRawID, displayName, contactStore string, pollTimeout time.Duration) *WeixinRuntime {
	rt := &WeixinRuntime{
		Client:       cli,
		SelfRawID:    selfRawID,
		DisplayName:  strings.TrimSpace(displayName),
		SelfMappedID: mapWeixinID(selfRawID),
		PollTimeout:  pollTimeout,
		ContactStore: contactStore,
		contacts:     map[int64]*WeixinContact{},
		contactsRaw:  map[string]*WeixinContact{},
	}
	if rt.DisplayName == "" {
		rt.DisplayName = selfRawID
	}
	rt.loadContacts()
	return rt
}

// UpdateSelf refreshes the active self identity after login or relogin.
func (rt *WeixinRuntime) UpdateSelf(rawID, displayName string) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.SelfRawID = rawID
	rt.SelfMappedID = mapWeixinID(rawID)
	rt.DisplayName = strings.TrimSpace(displayName)
	if rt.DisplayName == "" {
		rt.DisplayName = rawID
	}
}

func mapWeixinID(raw string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("wx:" + raw))
	id := int64(h.Sum64() & 0x7fffffffffffffff)
	if id == 0 {
		return 1
	}
	return id
}

func mapWeixinMessageID(selfRawID, peerRawID, rawMessageID string) int32 {
	key := fmt.Sprintf("wx:%s:%s:%s", selfRawID, peerRawID, rawMessageID)
	return int32(crc32.ChecksumIEEE([]byte(key)))
}

func mapWeixinMessageSeq(rawMessageID string) int32 {
	return int32(crc32.ChecksumIEEE([]byte("wxseq:" + rawMessageID)))
}

func (rt *WeixinRuntime) loadContacts() {
	if strings.TrimSpace(rt.ContactStore) == "" {
		return
	}
	data, err := os.ReadFile(rt.ContactStore)
	if err != nil {
		return
	}
	var contacts []*WeixinContact
	if err := json.Unmarshal(data, &contacts); err != nil {
		log.Warnf("加载 Weixin 联系人缓存失败: %v", err)
		return
	}
	for _, contact := range contacts {
		if contact == nil || contact.RawID == "" || contact.ID == 0 {
			continue
		}
		cp := *contact
		rt.contacts[cp.ID] = &cp
		rt.contactsRaw[cp.RawID] = &cp
	}
}

func (rt *WeixinRuntime) saveContactsLocked() {
	if strings.TrimSpace(rt.ContactStore) == "" {
		return
	}
	contacts := make([]*WeixinContact, 0, len(rt.contacts))
	for _, contact := range rt.contacts {
		cp := *contact
		contacts = append(contacts, &cp)
	}
	sort.Slice(contacts, func(i, j int) bool {
		return contacts[i].ID < contacts[j].ID
	})
	if err := os.MkdirAll(filepath.Dir(rt.ContactStore), 0o755); err != nil {
		log.Warnf("创建 Weixin 联系人缓存目录失败: %v", err)
		return
	}
	data, err := json.MarshalIndent(contacts, "", "  ")
	if err != nil {
		log.Warnf("序列化 Weixin 联系人缓存失败: %v", err)
		return
	}
	if err := os.WriteFile(rt.ContactStore, data, 0o644); err != nil {
		log.Warnf("写入 Weixin 联系人缓存失败: %v", err)
	}
}

// UpsertContact records an observed peer and latest context token.
func (rt *WeixinRuntime) UpsertContact(rawID, contextToken string) *WeixinContact {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if contact, ok := rt.contactsRaw[rawID]; ok {
		contact.LastSeen = time.Now().Unix()
		if contextToken != "" {
			contact.LastContextToken = contextToken
		}
		rt.saveContactsLocked()
		return contact
	}
	contact := &WeixinContact{
		RawID:            rawID,
		ID:               mapWeixinID(rawID),
		Nickname:         rawID,
		LastSeen:         time.Now().Unix(),
		LastContextToken: contextToken,
	}
	rt.contacts[contact.ID] = contact
	rt.contactsRaw[contact.RawID] = contact
	rt.saveContactsLocked()
	return contact
}

// ContactByID resolves a contact by hashed user id.
func (rt *WeixinRuntime) ContactByID(id int64) (*WeixinContact, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	contact, ok := rt.contacts[id]
	return contact, ok
}

// ListContacts returns a stable snapshot of observed contacts.
func (rt *WeixinRuntime) ListContacts() []*WeixinContact {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	ret := make([]*WeixinContact, 0, len(rt.contacts))
	for _, contact := range rt.contacts {
		cp := *contact
		ret = append(ret, &cp)
	}
	sort.Slice(ret, func(i, j int) bool {
		return ret[i].LastSeen > ret[j].LastSeen
	})
	return ret
}

// IsWeixinMode reports whether the bot is backed by the Weixin runtime.
func (bot *CQBot) IsWeixinMode() bool {
	return bot != nil && bot.Weixin != nil
}

// SelfID returns the public self id for the current bot.
func (bot *CQBot) SelfID() int64 {
	switch {
	case bot == nil:
		return 0
	case bot.Weixin != nil:
		return bot.Weixin.SelfMappedID
	case bot.Client != nil:
		return bot.Client.Uin
	default:
		return 0
	}
}

// SelfName returns the current bot display name.
func (bot *CQBot) SelfName() string {
	switch {
	case bot == nil:
		return ""
	case bot.Weixin != nil:
		return bot.Weixin.DisplayName
	case bot.Client != nil:
		return bot.Client.Nickname
	default:
		return ""
	}
}

func (bot *CQBot) weixinActions() []string {
	ret := make([]string, len(weixinSupportedActions))
	copy(ret, weixinSupportedActions)
	return ret
}

func (bot *CQBot) formatWeixinContent(content []global.MSG) any {
	if base.PostFormat == "array" {
		return content
	}
	return bot.formatWeixinRawMessage(content)
}

func (bot *CQBot) formatWeixinRawMessage(content []global.MSG) string {
	var sb strings.Builder
	for _, segment := range content {
		typ, _ := segment["type"].(string)
		data, _ := segment["data"].(global.MSG)
		switch typ {
		case "text":
			sb.WriteString(fmt.Sprint(data["text"]))
		case "image", "record", "video", "reply", "file":
			elem := msg.Element{Type: typ}
			for k, v := range data {
				elem.Data = append(elem.Data, msg.Pair{K: k, V: fmt.Sprint(v)})
			}
			sb.WriteString(elem.CQCode())
		}
	}
	return sb.String()
}

func (bot *CQBot) normalizeWeixinSegments(segments []wx.Segment) (content []global.MSG, filePath string) {
	for _, segment := range segments {
		switch segment.Type {
		case "text":
			content = append(content, global.MSG{
				"type": "text",
				"data": global.MSG{"text": segment.Text},
			})
		case "image":
			content = append(content, global.MSG{
				"type": "image",
				"data": global.MSG{"file": segment.File, "url": ""},
			})
		case "record":
			content = append(content, global.MSG{
				"type": "record",
				"data": global.MSG{"file": segment.File, "url": ""},
			})
		case "video":
			content = append(content, global.MSG{
				"type": "video",
				"data": global.MSG{"file": segment.File, "url": ""},
			})
		case "file":
			text := "[文件]"
			if segment.Name != "" {
				text = "[文件] " + segment.Name
			}
			content = append(content, global.MSG{
				"type": "text",
				"data": global.MSG{"text": text},
			})
			filePath = segment.File
		}
	}
	return content, filePath
}

// InsertWeixinPrivateMessage stores a normalized private message in the database.
func (bot *CQBot) InsertWeixinPrivateMessage(peerRawID, rawMessageID string, content []global.MSG, inbound bool, ts int64) int32 {
	contact := bot.Weixin.UpsertContact(peerRawID, "")
	globalID := mapWeixinMessageID(bot.Weixin.SelfRawID, peerRawID, rawMessageID)
	messageSeq := mapWeixinMessageSeq(rawMessageID)
	senderID := bot.SelfID()
	senderName := bot.SelfName()
	targetID := contact.ID
	if inbound {
		senderID = contact.ID
		senderName = contact.Nickname
		targetID = bot.SelfID()
	}
	msg := &db.StoredPrivateMessage{
		ID:       fmt.Sprintf("%s:%s:%s", bot.Weixin.SelfRawID, peerRawID, rawMessageID),
		GlobalID: globalID,
		SubType:  "normal",
		Attribute: &db.StoredMessageAttribute{
			MessageSeq: messageSeq,
			InternalID: messageSeq,
			SenderUin:  senderID,
			SenderName: senderName,
			Timestamp:  ts,
		},
		SessionUin: contact.ID,
		TargetUin:  targetID,
		Content:    content,
	}
	if err := db.InsertPrivateMessage(msg); err != nil {
		log.Warnf("记录 Weixin 私聊消息失败: %v", err)
		return -1
	}
	return globalID
}

// DispatchWeixinPrivateMessage emits a OneBot private message event from a Weixin runtime event.
func (bot *CQBot) DispatchWeixinPrivateMessage(event wx.Event) int32 {
	contact := bot.Weixin.UpsertContact(event.PeerRawID, event.ContextToken)
	content, filePath := bot.normalizeWeixinSegments(event.Segments)
	messageID := bot.InsertWeixinPrivateMessage(event.PeerRawID, event.RawMessageID, content, true, event.TimeMS/1000)
	payload := global.MSG{
		"message_id":      messageID,
		"user_id":         contact.ID,
		"target_id":       bot.SelfID(),
		"message":         bot.formatWeixinContent(content),
		"raw_message":     bot.formatWeixinRawMessage(content),
		"font":            0,
		"_wx_raw_self_id": bot.Weixin.SelfRawID,
		"_wx_raw_user_id": event.PeerRawID,
		"sender": global.MSG{
			"user_id":         contact.ID,
			"nickname":        contact.Nickname,
			"_wx_raw_user_id": event.PeerRawID,
		},
	}
	if filePath != "" {
		payload["_wx_file_path"] = filePath
	}
	bot.dispatchEvent("message/private/friend", payload)
	return messageID
}

// DispatchWeixinSelfMessage emits a self message event when self report is enabled.
func (bot *CQBot) DispatchWeixinSelfMessage(peerRawID, rawMessageID string, content []global.MSG, contextToken string, ts int64) int32 {
	contact := bot.Weixin.UpsertContact(peerRawID, contextToken)
	messageID := bot.InsertWeixinPrivateMessage(peerRawID, rawMessageID, content, false, ts)
	if base.ReportSelfMessage {
		bot.dispatchEvent("message_sent/private/friend", global.MSG{
			"message_id":      messageID,
			"user_id":         bot.SelfID(),
			"target_id":       contact.ID,
			"message":         bot.formatWeixinContent(content),
			"raw_message":     bot.formatWeixinRawMessage(content),
			"font":            0,
			"_wx_raw_self_id": bot.Weixin.SelfRawID,
			"_wx_raw_user_id": peerRawID,
			"sender": global.MSG{
				"user_id":  bot.SelfID(),
				"nickname": bot.SelfName(),
			},
		})
	}
	return messageID
}

// DispatchLifecycleConnect emits a OneBot lifecycle connect event.
func (bot *CQBot) DispatchLifecycleConnect() {
	bot.dispatchEvent("meta_event/lifecycle", global.MSG{})
}

func weixinUnsupportedAction(action string) global.MSG {
	return Failed(1404, "UNSUPPORTED_ACTION", fmt.Sprintf("%s unsupported in weixin mode", action))
}

func buildReplyFallback(messageID int32) string {
	msg0, err := db.GetMessageByGlobalID(messageID)
	if err != nil {
		return ""
	}
	var parts []string
	for _, segment := range msg0.GetContent() {
		if segment["type"] == "text" {
			data, _ := segment["data"].(global.MSG)
			parts = append(parts, strings.TrimSpace(fmt.Sprint(data["text"])))
		}
	}
	snippet := strings.TrimSpace(strings.Join(parts, " "))
	if snippet == "" {
		snippet = "消息"
	}
	if len(snippet) > 64 {
		snippet = snippet[:64] + "..."
	}
	return fmt.Sprintf("[回复 %d] %s", messageID, snippet)
}

// formatStoredWeixinMessage formats cached content for get_msg in weixin mode.
func (bot *CQBot) formatStoredWeixinMessage(content []global.MSG) any {
	return bot.formatWeixinContent(content)
}

func (bot *CQBot) weixinGetMessage(messageID int32) global.MSG {
	msg0, err := db.GetMessageByGlobalID(messageID)
	if err != nil {
		log.Warnf("获取 Weixin 消息失败: %v", err)
		return Failed(100, "MSG_NOT_FOUND", "消息不存在")
	}
	ret := global.MSG{
		"message_id":    msg0.GetGlobalID(),
		"message_id_v2": msg0.GetID(),
		"message_type":  msg0.GetType(),
		"real_id":       msg0.GetAttribute().MessageSeq,
		"message_seq":   msg0.GetAttribute().MessageSeq,
		"group":         false,
		"sender": global.MSG{
			"user_id":  msg0.GetAttribute().SenderUin,
			"nickname": msg0.GetAttribute().SenderName,
		},
		"time":    msg0.GetAttribute().Timestamp,
		"message": bot.formatStoredWeixinMessage(msg0.GetContent()),
	}
	return OK(ret)
}

func (bot *CQBot) weixinGetStrangerInfo(userID int64) global.MSG {
	contact, ok := bot.Weixin.ContactByID(userID)
	if !ok {
		return Failed(100, "USER_NOT_FOUND", "用户不存在")
	}
	return OK(global.MSG{
		"user_id":         contact.ID,
		"nickname":        contact.Nickname,
		"sex":             "unknown",
		"age":             0,
		"_wx_raw_user_id": contact.RawID,
	})
}

func (bot *CQBot) weixinGetFriendList() global.MSG {
	list := bot.Weixin.ListContacts()
	ret := make([]global.MSG, 0, len(list))
	for _, contact := range list {
		ret = append(ret, global.MSG{
			"user_id":         contact.ID,
			"nickname":        contact.Nickname,
			"_wx_raw_user_id": contact.RawID,
		})
	}
	return OK(ret)
}

func (bot *CQBot) weixinGetLoginInfo() global.MSG {
	return OK(global.MSG{
		"user_id":         bot.SelfID(),
		"nickname":        bot.SelfName(),
		"_wx_raw_self_id": bot.Weixin.SelfRawID,
	})
}

func (bot *CQBot) weixinGetStatus() global.MSG {
	online := false
	appGood := false
	if bot != nil && bot.Weixin != nil && bot.Weixin.Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		health, err := bot.Weixin.Client.Health(ctx)
		if err == nil {
			appGood = true
			online = health.LoggedIn
		}
	}
	return OK(global.MSG{
		"app_initialized": true,
		"app_enabled":     true,
		"plugins_good":    nil,
		"app_good":        appGood,
		"online":          online,
		"good":            appGood && online,
		"stat":            global.MSG{},
	})
}

func (bot *CQBot) weixinGetVersionInfo() global.MSG {
	return OK(global.MSG{
		"app_name":                   "go-cqhttp",
		"app_version":                base.Version,
		"protocol_version":           "v11",
		"coolq_edition":              "weixin",
		"go_cqhttp":                  true,
		"plugin_version":             "weixin-runtime",
		"plugin_build_number":        1,
		"plugin_build_configuration": "weixin-only",
		"version":                    base.Version,
		"protocol_name":              "weixin",
	})
}

func (bot *CQBot) weixinCanSendImage() global.MSG {
	return OK(global.MSG{"yes": true})
}

func (bot *CQBot) weixinCanSendRecord() global.MSG {
	return OK(global.MSG{"yes": false})
}

func (bot *CQBot) weixinSupported() global.MSG {
	return OK(bot.weixinActions())
}

func (bot *CQBot) buildWeixinSendSegments(input any) ([]wx.Segment, error) {
	var elements []msg.Element
	switch val := input.(type) {
	case string:
		elements = msg.ParseString(val)
	case []global.MSG:
		elements = make([]msg.Element, 0, len(val))
		for _, raw := range val {
			elem := msg.Element{Type: fmt.Sprint(raw["type"])}
			data, _ := raw["data"].(global.MSG)
			for k, v := range data {
				elem.Data = append(elem.Data, msg.Pair{K: k, V: fmt.Sprint(v)})
			}
			elements = append(elements, elem)
		}
	default:
		return nil, fmt.Errorf("unsupported message payload: %T", input)
	}

	var segments []wx.Segment
	var replyPrefix string
	for _, elem := range elements {
		switch elem.Type {
		case "text":
			text := elem.Get("text")
			if text != "" {
				segments = append(segments, wx.Segment{Type: "text", Text: text})
			}
		case "image":
			file := elem.Get("file")
			if file == "" {
				file = elem.Get("url")
			}
			if file == "" {
				return nil, fmt.Errorf("image segment missing file")
			}
			segments = append(segments, wx.Segment{Type: "image", File: file})
		case "video":
			file := elem.Get("file")
			if file == "" {
				file = elem.Get("url")
			}
			if file == "" {
				return nil, fmt.Errorf("video segment missing file")
			}
			segments = append(segments, wx.Segment{Type: "video", File: file})
		case "file":
			file := elem.Get("file")
			if file == "" {
				file = elem.Get("url")
			}
			if file == "" {
				return nil, fmt.Errorf("file segment missing file")
			}
			segments = append(segments, wx.Segment{
				Type: "file",
				File: file,
				Name: weixinSegmentName(file, elem.Get("name")),
			})
		case "reply":
			id, err := strconv.Atoi(elem.Get("id"))
			if err != nil {
				return nil, fmt.Errorf("invalid reply id")
			}
			replyPrefix = buildReplyFallback(int32(id))
		case "record":
			return nil, fmt.Errorf("record unsupported in weixin mode")
		default:
			return nil, fmt.Errorf("%s unsupported in weixin mode", elem.Type)
		}
	}
	if replyPrefix != "" {
		segments = append([]wx.Segment{{Type: "text", Text: replyPrefix}}, segments...)
	}
	return segments, nil
}

func weixinSegmentName(file, explicitName string) string {
	if name := strings.TrimSpace(explicitName); name != "" {
		return filepath.Base(name)
	}
	rawPath := strings.TrimSpace(file)
	if rawPath == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(rawPath, "http://"), strings.HasPrefix(rawPath, "https://"):
		if parsed, err := url.Parse(rawPath); err == nil {
			name := path.Base(parsed.Path)
			if name != "." && name != "/" && name != "" {
				return name
			}
		}
	case strings.HasPrefix(rawPath, "file://"):
		if parsed, err := url.Parse(rawPath); err == nil {
			name := filepath.Base(parsed.Path)
			if name != "." && name != string(filepath.Separator) && name != "" {
				return name
			}
		}
	default:
		name := filepath.Base(rawPath)
		if name != "." && name != string(filepath.Separator) && name != "" {
			return name
		}
	}
	return ""
}
