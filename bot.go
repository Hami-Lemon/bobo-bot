package main

import (
	"encoding/json"
	"fmt"
	"os"
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

	hotCount []int //统计时间段中，每一分钟内的评论数，数组索引表示距离统计开始时间的偏移量，单位分钟
	awlCount []int //每一分钟内的延迟统计

	startTime time.Time  //统计的开始时间点
	lock      sync.Mutex //互斥锁
}

// Reporter 延迟反馈报告
type Reporter struct {
	offset   int    //误差
	last     uint64 //上一次反馈时间
	interval int    // 两次反馈的间隔时间
}

type Bot struct {
	board   Board          //监控的评论区
	monitor MonitorAccount //监控的账户
	bili    *BiliBili
	counter *Counter //统计器
	logger  *logger.Logger
	stop    chan struct{} //退出信号
	freshCD int           //抓取评论cd
	likeCD  int           //点赞cd
	report  *Reporter
}

func NewBot(bili *BiliBili, board Board,
	monitor MonitorAccount, freshCD, likeCD int) *Bot {
	bili.AccountInfo(&monitor)
	bili.AccountStat(&monitor)
	bili.BoardDetail(&board)
	bili.GetCommentsPage(&board)
	now := time.Now()
	counter := Counter{
		peopleCount: make(map[uint64]int),
		hotCount:    make([]int, 0, CountCap),
		awlCount:    make([]int, 0, CountCap),
		startTime:   now,
	}

	return &Bot{
		board:   board,
		monitor: monitor,
		bili:    bili,
		counter: &counter,
		logger:  logger.New(fmt.Sprintf("Bot-%s", board.name), logLevel, logDst),
		stop:    make(chan struct{}, 1),
		freshCD: freshCD,
		likeCD:  likeCD,
		report: &Reporter{
			offset:   freshCD,
			interval: 60 * 3, //三分钟内只触发一次
		},
	}
}

// Monitor 开启赛博监控
func (b *Bot) Monitor() {
	tick := time.Tick(time.Duration(b.freshCD) * time.Second)
	//获取评论
	comments := b.bili.GetComments(b.board)
	if comments == nil {
		b.logger.Error("获取评论失败，oid=%d", b.board.oid)
		return
	}
	lastComments := set.NewSlice(comments)
loop:
	for {
		select {
		case <-b.stop:
			break loop
		case <-tick:
			comments = b.bili.GetComments(b.board)
			now := time.Now()
			for _, comment := range comments {
				//该评论出现在上次获取到的评论中，可能已经点赞了
				if lastComments.Contains(comment) {
					continue
				}
				select {
				case <-b.stop:
					break loop
				default:
					break
				}
				b.work(comment, now)
				b.counter.Count(comment, now)
				//TODO 监控个人资料修改 #3
				b.logger.Debug("点赞CD")
				time.Sleep(time.Duration(b.likeCD) * time.Second)
			}
			if comments == nil {
				b.logger.Error("获取评论失败，oid=%d, type=%d", b.board.oid, b.board.typeCode)
			} else {
				lastComments.Clear()
				lastComments.Add(comments...)
			}
			b.logger.Debug("刷新CD")
		}
	}
	b.logger.Info("停止监控")
}

//点赞评论，now为获取到该评论的时间
func (b *Bot) work(comment Comment, now time.Time) {
	db.InsertComment(comment, now.Unix())
	bili := b.bili
	//点赞该评论
	if bili.LikeComment(comment) {
		b.logger.Info("成功点赞评论, msg=%s, uname=%s, uid=%d",
			comment.msg, comment.uname, comment.uid)
	} else {
		b.logger.Error("点赞评论失败,oid=%d, rpid=%d, msg=%s",
			comment.oid, comment.replyId, comment.msg)
		return
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
				b.logger.Info("反馈延迟：%s, rpid=%d, msg=%s, ctime=%d",
					delay, comment.replyId, comment.msg, comment.ctime)
			} else {
				b.logger.Error("反馈延迟失败, delay=%s, rpid=%d, msg=%s, ctime=%d",
					delay, comment.replyId, comment.msg, comment.ctime)
			}
		}
	}
}

// Stop 停止赛博监控
func (b *Bot) Stop() {
	b.logger.Info("调用停止函数")
	close(b.stop)
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
		hot++
		c.hotCount = util.SliceSet(c.hotCount, index, hot)
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
	c.startTime = time.Now()
}

type summary struct {
	Board struct {
		Name          string         `json:"name"`          //版聊区名称
		Oid           uint64         `json:"oid"`           //oid
		Start         int64          `json:"start"`         //统计的开始时间
		End           int64          `json:"end"`           //统计结束时间
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
	} `json:"account"`
}

// Summarize 总结评论数据
func (b *Bot) Summarize() {
	counter := b.counter
	counter.lock.Lock()
	defer counter.lock.Unlock()
	board := &Board{
		oid:      b.board.oid,
		typeCode: b.board.typeCode,
	}
	account := &MonitorAccount{
		Account: Account{
			uid: b.monitor.uid,
		},
	}
	b.bili.GetCommentsPage(board)
	b.bili.AccountStat(account)

	report := summary{}
	report.Board.Name = b.board.name
	report.Board.Oid = b.board.oid
	report.Board.Start = counter.startTime.Unix()
	report.Board.End = time.Now().Unix()
	report.Board.Hot = counter.hotCount
	report.Board.Awl = counter.awlCount
	report.Board.People = counter.peopleCount
	report.Board.Count = counter.todayComment
	report.Board.StartAllCount = b.board.allCount
	report.Board.StartCount = b.board.count
	report.Board.EndAllCount = board.allCount
	report.Board.EndCount = board.count

	report.Account.Name = b.monitor.uname
	report.Account.Uid = b.monitor.uid
	report.Account.Alias = b.monitor.alias
	report.Account.StartFollowers = b.monitor.follower
	report.Account.EndFollowers = account.follower

	reportJson, _ := json.Marshal(report)
	now := time.Now()
	fileName := fmt.Sprintf("./report/%s.json", now.Format("200601021504"))
	jsonFile, err := os.Create(fileName)
	if err != nil && os.IsNotExist(err) {
		err = os.Mkdir("./report", os.ModePerm)
		if util.IsError(err, "creat dir report fail!") {
			_, _ = os.Stdout.Write(reportJson)
			return
		}
		jsonFile, _ = os.Create(fileName)
	}
	_, _ = jsonFile.Write(reportJson)
	_ = jsonFile.Close()
	b.monitor.follower = account.follower
	b.board.allCount = board.allCount
	b.board.count = board.count
	counter.reset()
}

// MonitorDynamic 动态监控 TODO
func (b *Bot) MonitorDynamic() {

}
