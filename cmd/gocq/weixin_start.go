package gocq

import (
	"context"
	"os"
	"time"

	qrterminal "github.com/mdp/qrterminal/v3"
	log "github.com/sirupsen/logrus"

	"github.com/Mrs4s/go-cqhttp/coolq"
	"github.com/Mrs4s/go-cqhttp/internal/base"
	wx "github.com/Mrs4s/go-cqhttp/internal/weixin"
	"github.com/Mrs4s/go-cqhttp/modules/servers"
)

var runningBot *coolq.CQBot

func weixinConfig() wx.Config {
	conf := base.Weixin
	if conf == nil {
		return wx.Config{
			APIBaseURL:  "https://ilinkai.weixin.qq.com",
			CDNBaseURL:  "https://novac2c.cdn.weixin.qq.com/c2c",
			PollTimeout: 35 * time.Second,
			QRTimeout:   480 * time.Second,
			StateDir:    "data/weixin",
		}
	}
	pollTimeout := time.Duration(conf.PollTimeout) * time.Second
	if pollTimeout <= 0 {
		pollTimeout = 35 * time.Second
	}
	qrTimeout := time.Duration(conf.QRTimeout) * time.Second
	if qrTimeout <= 0 {
		qrTimeout = 480 * time.Second
	}
	stateDir := conf.StateDir
	if stateDir == "" {
		stateDir = "data/weixin"
	}
	return wx.Config{
		APIBaseURL:  conf.APIBaseURL,
		CDNBaseURL:  conf.CDNBaseURL,
		PollTimeout: pollTimeout,
		QRTimeout:   qrTimeout,
		StateDir:    stateDir,
	}
}

func ensureWeixinLogin(cli *wx.Client, conf wx.Config) (*wx.SelfResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	self, err := cli.Self(ctx)
	if err == nil && self.LoggedIn && self.SelfRawID != "" {
		return self, nil
	}

	if _, err := cli.Health(ctx); err != nil {
		return nil, err
	}

	start, err := cli.StartQRLogin(ctx)
	if err != nil {
		return nil, err
	}
	log.Info("请使用微信扫描以下二维码完成登录:")
	qrterminal.GenerateHalfBlock(start.QRDataURL, qrterminal.L, os.Stdout)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), conf.QRTimeout+10*time.Second)
	defer waitCancel()
	waitResp, err := cli.WaitQRLogin(waitCtx, start.SessionKey, conf.QRTimeout)
	if err != nil {
		return nil, err
	}
	if !waitResp.Connected {
		return nil, wx.ErrNotLoggedIn
	}
	finalCtx, finalCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer finalCancel()
	return cli.Self(finalCtx)
}

func pollWeixinEvents(bot *coolq.CQBot) {
	conf := weixinConfig()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), conf.PollTimeout+10*time.Second)
		events, err := bot.Weixin.Client.Poll(ctx, conf.PollTimeout)
		cancel()
		if err != nil {
			if err == wx.ErrNotLoggedIn {
				log.Warn("Weixin 会话失效，正在等待重新扫码登录")
				self, loginErr := ensureWeixinLogin(bot.Weixin.Client, conf)
				if loginErr != nil {
					log.Warnf("重新登录 Weixin 失败: %v", loginErr)
					time.Sleep(5 * time.Second)
					continue
				}
				bot.Weixin.UpdateSelf(self.SelfRawID, self.DisplayName)
				bot.DispatchLifecycleConnect()
				log.Infof("Weixin reconnected as %s (%d)", bot.Weixin.SelfRawID, bot.Weixin.SelfMappedID)
				continue
			}
			log.Warnf("拉取 Weixin 事件失败: %v", err)
			time.Sleep(3 * time.Second)
			continue
		}
		for _, event := range events {
			if event.Type != "private_message" {
				continue
			}
			bot.DispatchWeixinPrivateMessage(event)
		}
	}
}

// StartWeixin starts the Weixin-only OneBot shell.
func StartWeixin() {
	conf := weixinConfig()
	cli := wx.NewClient(conf)
	self, err := ensureWeixinLogin(cli, conf)
	if err != nil {
		log.Fatalf("初始化 Weixin 登录失败: %v", err)
	}
	rt := coolq.NewWeixinRuntime(cli, self.SelfRawID, self.DisplayName, cli.ContactStorePath(), conf.PollTimeout)
	runningBot = coolq.NewWeixinBot(rt)
	servers.Run(runningBot)
	runningBot.DispatchLifecycleConnect()
	log.Infof("Weixin connected as %s (%d)", rt.SelfRawID, rt.SelfMappedID)
	go pollWeixinEvents(runningBot)
}
