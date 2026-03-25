package coolq

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/Mrs4s/go-cqhttp/global"
	wx "github.com/Mrs4s/go-cqhttp/internal/weixin"
	"github.com/Mrs4s/go-cqhttp/pkg/onebot"
)

func decodeWeixinMessagePayload(value gjson.Result) (any, error) {
	if !value.Exists() {
		return "", nil
	}
	if value.Type == gjson.String {
		return value.String(), nil
	}
	var out []global.MSG
	if err := json.Unmarshal([]byte(value.Raw), &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (bot *CQBot) weixinSendPrivate(userID int64, message gjson.Result) global.MSG {
	contact, ok := bot.Weixin.ContactByID(userID)
	if !ok {
		return Failed(100, "USER_NOT_FOUND", "用户不存在")
	}
	if contact.LastContextToken == "" {
		return Failed(100, "CONTEXT_TOKEN_MISSING", "用户最近没有可用会话")
	}
	payload, err := decodeWeixinMessagePayload(message)
	if err != nil {
		return Failed(100, "BAD_MESSAGE", err.Error())
	}
	segments, err := bot.buildWeixinSendSegments(payload)
	if err != nil {
		return Failed(100, "UNSUPPORTED_MESSAGE", err.Error())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	sent, err := bot.Weixin.Client.SendPrivate(ctx, wx.SendPrivateRequest{
		PeerRawID:    contact.RawID,
		ContextToken: contact.LastContextToken,
		Segments:     segments,
	})
	if err != nil {
		return Failed(100, "SEND_ERROR", err.Error())
	}
	content, _ := bot.normalizeWeixinSegments(segments)
	messageID := bot.DispatchWeixinSelfMessage(contact.RawID, sent.RawMessageID, content, contact.LastContextToken, sent.TimeMS/1000)
	return OK(global.MSG{"message_id": messageID})
}

func (bot *CQBot) weixinUploadPrivateFile(userID int64, file, name string) global.MSG {
	contact, ok := bot.Weixin.ContactByID(userID)
	if !ok {
		return Failed(100, "USER_NOT_FOUND", "用户不存在")
	}
	if contact.LastContextToken == "" {
		return Failed(100, "CONTEXT_TOKEN_MISSING", "用户最近没有可用会话")
	}
	if err := validateWeixinUploadFile(file); err != nil {
		return Failed(100, "OPEN_FILE_ERROR", "打开文件失败")
	}
	segment := wx.Segment{
		Type: "file",
		File: file,
		Name: weixinSegmentName(file, name),
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*30)
	defer cancel()
	sent, err := bot.Weixin.Client.SendPrivate(ctx, wx.SendPrivateRequest{
		PeerRawID:    contact.RawID,
		ContextToken: contact.LastContextToken,
		Segments:     []wx.Segment{segment},
	})
	if err != nil {
		return Failed(100, "SEND_ERROR", err.Error())
	}
	content, _ := bot.normalizeWeixinSegments([]wx.Segment{segment})
	_ = bot.DispatchWeixinSelfMessage(contact.RawID, sent.RawMessageID, content, contact.LastContextToken, sent.TimeMS/1000)
	return OK(nil)
}

func (bot *CQBot) weixinHandleQuickOperation(contextResult, operation gjson.Result) global.MSG {
	if !contextResult.Exists() || !operation.Exists() {
		return OK(nil)
	}
	if contextResult.Get("post_type").String() != "message" || contextResult.Get("message_type").String() != "private" {
		return OK(nil)
	}
	reply := operation.Get("reply")
	if !reply.Exists() {
		return OK(nil)
	}
	userID := contextResult.Get("user_id").Int()
	return bot.weixinSendPrivate(userID, reply)
}

// CallWeixinAction handles the reduced Weixin-only OneBot surface.
func (bot *CQBot) CallWeixinAction(action string, spec *onebot.Spec, getter func(string) gjson.Result) global.MSG {
	if spec.Version != 11 {
		return Failed(1404, "UNSUPPORTED_ACTION", "only onebot v11 is supported in weixin mode")
	}
	switch action {
	case ".handle_quick_operation":
		return bot.weixinHandleQuickOperation(getter("context"), getter("operation"))
	case "can_send_image":
		return bot.weixinCanSendImage()
	case "can_send_record":
		return bot.weixinCanSendRecord()
	case "download_file":
		return bot.CQDownloadFile(getter("url").String(), getter("headers"), int(getter("thread_count").Int()))
	case "get_friend_list":
		return bot.weixinGetFriendList()
	case "get_login_info":
		return bot.weixinGetLoginInfo()
	case "get_msg":
		return bot.weixinGetMessage(int32(getter("message_id").Int()))
	case "get_status":
		return bot.weixinGetStatus()
	case "get_stranger_info":
		return bot.weixinGetStrangerInfo(getter("user_id").Int())
	case "get_supported_actions":
		return bot.weixinSupported()
	case "get_version_info":
		return bot.weixinGetVersionInfo()
	case "send_msg":
		if getter("group_id").Exists() && getter("group_id").Int() != 0 {
			return Failed(100, "UNSUPPORTED_MESSAGE_TYPE", "send_msg only supports private messages in weixin mode")
		}
		messageType := strings.TrimSpace(getter("message_type").String())
		if messageType != "" && messageType != "private" {
			return Failed(100, "UNSUPPORTED_MESSAGE_TYPE", "send_msg only supports private messages in weixin mode")
		}
		return bot.weixinSendPrivate(getter("user_id").Int(), getter("message"))
	case "send_private_msg":
		return bot.weixinSendPrivate(getter("user_id").Int(), getter("message"))
	case "upload_private_file":
		return bot.weixinUploadPrivateFile(getter("user_id").Int(), getter("file").String(), getter("name").String())
	default:
		return weixinUnsupportedAction(action)
	}
}

func validateWeixinUploadFile(file string) error {
	rawPath := strings.TrimSpace(file)
	if rawPath == "" {
		return os.ErrNotExist
	}
	if strings.HasPrefix(rawPath, "http://") || strings.HasPrefix(rawPath, "https://") {
		return nil
	}
	if strings.HasPrefix(rawPath, "file://") {
		parsed, err := url.Parse(rawPath)
		if err != nil {
			return err
		}
		rawPath = parsed.Path
	}
	if !filepath.IsAbs(rawPath) {
		abs, err := filepath.Abs(rawPath)
		if err == nil {
			rawPath = abs
		}
	}
	_, err := os.Stat(rawPath)
	return err
}
