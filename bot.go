package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Hami-Lemon/bobo-bot/logger"
	"github.com/Hami-Lemon/bobo-bot/set"
	"github.com/Hami-Lemon/bobo-bot/util"
)

const (
	CountCap = 24 * 60
)

type Counter struct {
	todayComment int            //统计时段内记录到的评论数
	peopleCount  map[uint64]int //参与评论的用户，记录不同用户的发评数量

	hotCount  []int //统计时间段中，每一分钟内的评论数，数组索引表示距离统计开始时间的偏移量，单位分钟
	awlCount  []int //每一分钟内的延迟统计
	fansCount []int //粉丝数变化

	startTime time.Time  //统计的开始时间点
	lock      sync.Mutex //互斥锁
}

// Reporter 延迟反馈报告
type Reporter struct {
	offset   int    //误差
	last     uint64 //上一次反馈时间
	interval int    // 两次反馈的间隔时间
}

// BotOption Bot的可配置项
type BotOption struct {
	freshCD int     //获取评论cd
	likeCD  float32 //点赞cd，单位：秒
	isLike  bool    //是否开启点赞
	isPost  bool    //是否发布数据总结动态
}

type Bot struct {
	board     Board          //监控的评论区
	monitor   MonitorAccount //监控的账户
	bili      *BiliBili
	counter   *Counter //统计器
	logger    *logger.Logger
	stop      chan struct{} //退出信号
	likeQueue chan Comment  //点赞评论的任务队列
	BotOption
	report *Reporter
}

func NewBot(bili *BiliBili, board Board,
	monitor MonitorAccount, opt BotOption) *Bot {
	if !bili.AccountInfo(&monitor) {
		mainLogger.Error("获取用户信息失败！")
	}
	if !bili.AccountStat(&monitor) {
		mainLogger.Error("获取粉丝数失败！")
	}
	if !bili.BoardDetail(&board) {
		mainLogger.Error("获取评论区信息失败！")
	}
	if !bili.GetCommentsPage(&board) {
		mainLogger.Error("获取评论数量失败！")
	}
	now := time.Now()
	counter := Counter{
		peopleCount: make(map[uint64]int),
		hotCount:    make([]int, 0, CountCap),
		awlCount:    make([]int, 0, CountCap),
		fansCount:   make([]int, 1),
		startTime:   now,
	}
	counter.fansCount[0] = monitor.follower

	return &Bot{
		board:     board,
		monitor:   monitor,
		bili:      bili,
		counter:   &counter,
		logger:    logger.New(fmt.Sprintf("Bot-%s", board.name), logLevel, logDst),
		stop:      make(chan struct{}, 1),
		likeQueue: make(chan Comment, 32),
		BotOption: opt,
		report: &Reporter{
			offset:   opt.freshCD,
			interval: 60 * 3, //三分钟内只触发一次
		},
	}
}

// RecoverBot 使用上一次中断程序后保存的数据恢复
func RecoverBot(bili *BiliBili, opt BotOption, summary Summary) *Bot {
	if strings.Compare(summary.Version, Version) != 0 {
		mainLogger.Warn("当前版本：%s，恢复信息版本：%s", Version, summary.Version)
	}
	board := Board{
		name: summary.Board.Name,
		dId:  summary.Board.DynamicId,
		bvID: summary.Board.BvID,
	}
	if !bili.BoardDetail(&board) {
		mainLogger.Error("获取评论区信息失败！")
	}
	board.allCount = summary.Board.StartAllCount
	board.count = summary.Board.StartCount

	monitor := MonitorAccount{
		Account: Account{
			uname: summary.Account.Name,
			uid:   summary.Account.Uid,
			alias: summary.Account.Alias,
		},
		follower: summary.Account.StartFollowers,
	}

	counter := &Counter{
		todayComment: summary.Board.Count,
		peopleCount:  summary.Board.People,
		hotCount:     summary.Board.Hot,
		awlCount:     summary.Board.Awl,
		fansCount:    summary.Account.FansCount,
		startTime:    time.Unix(summary.Start, 0),
	}
	bot := &Bot{
		board:     board,
		monitor:   monitor,
		bili:      bili,
		counter:   counter,
		logger:    logger.New(fmt.Sprintf("Bot-%s", board.name), logLevel, logDst),
		stop:      make(chan struct{}, 1),
		likeQueue: make(chan Comment, 32),
		BotOption: opt,
		report: &Reporter{
			offset:   opt.freshCD,
			interval: 60 * 3,
		},
	}
	return bot
}

func setAddComments(s *set.HashSet[uint64], c []Comment) {
	for i := range c {
		s.Add(c[i].replyId)
	}
}

// Monitor 开启赛博监控
func (b *Bot) Monitor() {
	if b.isLike {
		go b.likeComment()
	}
	tick := time.Tick(time.Duration(b.freshCD) * time.Second)
	//获取评论
	comments := b.bili.GetComments(b.board)
	if comments == nil {
		b.logger.Error("获取评论失败，oid=%d", b.board.oid)
		return
	}
	lastComments := set.New[uint64]()
	setAddComments(lastComments, comments)
loop:
	for {
		select {
		case <-b.stop:
			break loop
		case now := <-tick:
			comments = b.bili.GetComments(b.board)
			for _, comment := range comments {
				select {
				case <-b.stop:
					break loop
				default:
					break
				}
				//该评论出现在上次获取到的评论中，可能已经点赞了
				if lastComments.Contains(comment.replyId) {
					continue
				}
				b.work(comment, now)
				b.counter.Count(comment, now)
				//TODO 监控个人资料修改 #3
			}
			if comments == nil {
				b.logger.Error("获取评论失败，oid=%d, type=%d", b.board.oid, b.board.typeCode)
			} else {
				lastComments.Clear()
				setAddComments(lastComments, comments)
			}
			b.logger.Debug("刷新CD")
		}
	}
	b.logger.Info("停止监控")
}

// MonitorFans 监控粉丝数变化，十分钟更新一次
func (b *Bot) MonitorFans() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	account := &MonitorAccount{
		Account: Account{
			uid: b.monitor.uid,
		},
		follower: b.monitor.follower,
	}
	fansChange := func(c *Counter, fans int) {
		c.lock.Lock()
		defer c.lock.Unlock()
		c.fansCount = append(c.fansCount, fans)
	}
	counter := b.counter
	//db.InsertFollower(account.uid, counter.startTime.Unix(), account.follower)
	for {
		select {
		case <-b.stop:
			return
		case now := <-ticker.C:
			if b.bili.AccountStat(account) {
				b.logger.Info("获取粉丝数，uid=%d, fans=%d", account.uid, account.follower)
				db.InsertFollower(account.uid, now.Unix(), account.follower)
				fansChange(counter, account.follower)
			} else {
				b.logger.Error("获取粉丝数失败，uid=%d", account.uid)
			}
		}
	}
}

//处理点赞任务
func (b *Bot) likeComment() {
	for comment := range b.likeQueue {
		if b.bili.LikeComment(comment) {
			b.logger.Info("成功点赞评论, msg=%s, uname=%s, uid=%d",
				comment.msg, comment.uname, comment.uid)
		} else {
			b.logger.Error("点赞评论失败,oid=%d, rpid=%d, msg=%s",
				comment.oid, comment.replyId, comment.msg)
			//可能因为请求频繁而点赞失败，增加一倍cd时间
			time.Sleep(time.Duration(b.likeCD*1000) * time.Millisecond)
		}
		b.logger.Debug("点赞CD")
		time.Sleep(time.Duration(b.likeCD*1000) * time.Millisecond)
	}
}

//处理评论，now为获取到该评论的时间
func (b *Bot) work(comment Comment, now time.Time) {
	//插入到数据库中
	db.InsertComment(comment, now.Unix())
	bili := b.bili
	//点赞该评论
	if b.isLike {
		select {
		case b.likeQueue <- comment:
			break
		default:
			b.logger.Warn("缓冲区已满，不点赞该评论：msg=%s, uname=%s, uid=%d",
				comment.msg, comment.uname, comment.uid)
		}
	} else {
		b.logger.Info("获取到评论，msg=%s, uname=%s, uid=%d",
			comment.msg, comment.uname, comment.uid)
	}
	//如果评论包含 test 触发延迟反馈
	if strings.Contains(comment.msg, "test") {
		//计算延迟，当前时间 - 评论发布时间 - freshCD
		//如果小于0，则延迟为0
		delay := b.report.Report(comment, now)
		if delay == "" {
			b.logger.Info("间隔过短，不触发延迟反馈")
		} else {
			if bili.PostComment(b.board, &comment, delay) {
				b.logger.Info("反馈延迟成功：%s, rpid=%d, msg=%s, ctime=%d",
					delay, comment.replyId, comment.msg, comment.ctime)
			} else {
				b.logger.Error("反馈延迟失败, delay=%s, rpid=%d, msg=%s, ctime=%d",
					delay, comment.replyId, comment.msg, comment.ctime)
			}
		}
	}
	//嘿嘿嘿...33的评论...小小的...香香的...
	if comment.uid == b.monitor.uid {
		pushAndLog(b.logger, "[%s]\n%s的评论：%s",
			time.Unix(int64(comment.ctime), 0).Format("01-02 15:04:05"),
			b.monitor.alias, comment.msg)
	}
}

// Stop 停止赛博监控
func (b *Bot) Stop() {
	b.logger.Debug("调用停止函数")
	close(b.stop)
	close(b.likeQueue)
}

// Report 通过获取到评论的时间，减去评论的发出时间，计算延迟，nowTime为获取到该评论的时间
func (r *Reporter) Report(comment Comment, nowTime time.Time) string {
	now := uint64(nowTime.Unix())
	delay := int(now-comment.ctime) - r.offset
	// 因为设定每隔几秒获取一次评论，所以会存在几秒的误差，
	// 如果计算的延迟小于该间隔时间，则延迟为0
	if delay < 0 {
		delay = 0
	}
	if r.last == 0 || int(now-r.last) > r.interval {
		r.last = now
	} else {
		//间隔过短，不触发延迟反馈
		return ""
	}
	var delayMsg string
	if delay <= 60 {
		delayMsg = fmt.Sprintf("延迟为%2d秒", delay)
	} else if delay <= 60*60 {
		delayMsg = fmt.Sprintf("延迟为%d分%02d秒", delay/60, delay%60)
	} else {
		s := delay % 60
		delay /= 60
		m, h := delay%60, delay/60
		delayMsg = fmt.Sprintf("延迟为%d时%02d分%02d秒", h, m, s)
	}
	return delayMsg
}

// Count 评论数据计数，nowTime为获取到该评论的时间
func (c *Counter) Count(comment Comment, nowTime time.Time) {
	c.lock.Lock()
	defer c.lock.Unlock()

	c.peopleCount[comment.uid]++
	c.todayComment++

	ctime := int64(comment.ctime)
	index := int(ctime - c.startTime.Unix())
	if index >= 0 {
		index /= 60
		var hot int
		c.hotCount, hot = util.SliceGet(c.hotCount, index)
		c.hotCount = util.SliceSet(c.hotCount, index, hot+1)
	}

	now := nowTime.Unix()
	delay := int(now - ctime)
	index = int(now-c.startTime.Unix()) / 60
	var d int
	c.awlCount, d = util.SliceGet(c.awlCount, index)
	//只记录最大延迟时间，单位：秒
	if delay > d {
		c.awlCount = util.SliceSet(c.awlCount, index, delay)
	}
}

//重置
func (c *Counter) reset() {
	//重置
	c.todayComment = 0
	c.peopleCount = make(map[uint64]int)
	c.hotCount = make([]int, 0, CountCap)
	c.awlCount = make([]int, 0, CountCap)
	c.fansCount = make([]int, 0)
	c.startTime = time.Now()
}

type Summary struct {
	Version string `json:"version"` //对应程序的版本号
	Start   int64  `json:"start"`   //统计的开始时间
	End     int64  `json:"end"`     //统计结束时间
	Board   struct {
		Name          string         `json:"name"`          //版聊区名称
		DynamicId     uint64         `json:"dynamicId"`     //对应的动态id
		BvID          string         `json:"bvID"`          //如果是视频评论区，则是对应视频的bv号，否则为空
		Oid           uint64         `json:"oid"`           //oid
		Hot           []int          `json:"hot"`           //每分钟内的评论数
		Awl           []int          `json:"awl"`           //每分钟内的最大延迟
		People        map[uint64]int `json:"people"`        //参与评论的用户，键为uid, 值为发送的评论数
		Count         int            `json:"count"`         //记录到的评论数，不含楼中楼
		StartAllCount int            `json:"startAllCount"` //开始时的总评论数，包含楼中楼
		StartCount    int            `json:"startCount"`    //开始时的评论数，不含楼中楼
		EndAllCount   int            `json:"endAllCount"`   //结束时的总评论数，包含楼中楼
		EndCount      int            `json:"endCount"`      //结束时的评论数，不含楼中楼
	} `json:"board"`
	Account struct {
		Name           string `json:"name"`           //用户名
		Alias          string `json:"alias"`          //别名
		Uid            uint64 `json:"uid"`            //uid
		StartFollowers int    `json:"startFollowers"` //粉丝数
		EndFollowers   int    `json:"endFollowers"`
		FansCount      []int  `json:"fansCount"` //粉丝数变化
	} `json:"account"`
}

// Summarize 总结评论数据
func (b *Bot) Summarize() string {
	counter := b.counter
	counter.lock.Lock()
	defer counter.lock.Unlock()
	board := &Board{
		oid:      b.board.oid,
		typeCode: b.board.typeCode,
	}
	account := &MonitorAccount{
		Account: Account{
			uid:   b.monitor.uid,
			uname: b.monitor.uname,
		},
	}
	//未统计到数据
	if len(counter.hotCount) == 0 && len(counter.fansCount) == 1 {
		b.logger.Warn("未统计到数据")
		return ""
	}
	b.bili.GetCommentsPage(board)
	b.bili.AccountInfo(account)
	b.bili.AccountStat(account)

	report := Summary{Version: Version}
	report.Board.Name = b.board.name
	report.Board.DynamicId = b.board.dId
	report.Board.BvID = b.board.bvID
	report.Board.Oid = b.board.oid
	report.Start = counter.startTime.Unix()
	report.End = time.Now().Unix()
	report.Board.Hot = counter.hotCount
	report.Board.Awl = counter.awlCount
	report.Board.People = counter.peopleCount
	report.Board.Count = counter.todayComment
	report.Board.StartAllCount = b.board.allCount
	report.Board.StartCount = b.board.count
	report.Board.EndAllCount = board.allCount
	report.Board.EndCount = board.count

	report.Account.Name = account.uname
	report.Account.Uid = b.monitor.uid
	report.Account.Alias = b.monitor.alias
	report.Account.StartFollowers = b.monitor.follower
	report.Account.EndFollowers = account.follower
	report.Account.FansCount = counter.fansCount

	reportJson, _ := json.Marshal(report)
	now := time.Now()
	fileName := fmt.Sprintf("./report/%s.json", now.Format("200601021504"))
	jsonFile, err := os.Create(fileName)
	if err != nil && os.IsNotExist(err) {
		err = os.Mkdir("./report", os.ModePerm)
		if util.IsError(err, "creat dir report fail!") {
			_, _ = os.Stdout.Write(reportJson)
			return ""
		}
		jsonFile, _ = os.Create(fileName)
	}
	_, _ = jsonFile.Write(reportJson)
	_ = jsonFile.Close()
	b.monitor.follower = account.follower
	b.monitor.uname = account.uname
	b.board.allCount = board.allCount
	b.board.count = board.count
	counter.reset()
	b.logger.Info("数据保存为：%s", fileName)
	return fileName
}

func (b *Bot) ReportSummarize(fileName string) {
	//调用python脚本，处理数据并发布动态
	var cmd *exec.Cmd
	if b.isPost {
		cmd = exec.Command("python", "./analyse/main.py", fileName, "post")
	} else {
		cmd = exec.Command("python", "./analyse/main.py", fileName)
	}
	b.logger.Info("run python command: %s", cmd.String())
	cmd.Stdout = logDst
	cmd.Stderr = logDst
	err := cmd.Start()
	if err != nil {
		b.logger.Error("run python error: %v", err)
		pushAndLog(b.logger, "运行python脚本出现错误，%v", err)
		return
	}
	go func() {
		//等待子进程结束并释放资源
		err = cmd.Wait()
		if err != nil {
			b.logger.Error("脚本运行出现错误，%v", err)
			pushAndLog(b.logger, "脚本运行出现错误，%v", err)
		}
	}()
}

// MonitorDynamic 动态监控 TODO
func (b *Bot) MonitorDynamic() {

}
