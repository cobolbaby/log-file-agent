package watchdog

import (
	"errors"
	"github.com/cobolbaby/log-agent/watchdog/handler"
	"github.com/cobolbaby/log-agent/watchdog/lib/fsnotify"
	// "github.com/cobolbaby/log-agent/watchdog/lib/hook"
	"github.com/cobolbaby/log-agent/watchdog/lib/log"
	"github.com/cobolbaby/log-agent/watchdog/watcher"
	"github.com/djherbis/times"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

type Plugin interface {
	Init(*Watchdog)
	IsActive() bool
	Description() string
	AutoCheck() error
	Listen() error
	Process() error
}

type Watchdog struct {
	host     string
	logger   log.Logger
	watchers map[string]watcher.Watcher
	rules    map[string][]string
	adapters map[string][]handler.WatchdogHandler // 优先级队列
	plugins  []Plugin
	cacheQ   []fsnotify.FileEvent
}

func NewWatchdog() *Watchdog {
	return &Watchdog{
		rules:    make(map[string][]string),
		watchers: make(map[string]watcher.Watcher),
		adapters: make(map[string][]handler.WatchdogHandler),
	}
}

func (this *Watchdog) SetHost(host string) *Watchdog {
	this.host = host
	return this
}

func (this *Watchdog) SetLogger(logger log.Logger) *Watchdog {
	this.logger = logger
	return this
}

func (this *Watchdog) SetWatcher(biz string, listener watcher.Watcher) *Watchdog {
	this.watchers[biz] = listener
	return this
}

func (this *Watchdog) SetRules(biz string, rule string) *Watchdog {
	// 将rules按照分隔符拆分，合并当前规则
	ruleSlice := strings.Split(rule, ",")
	this.rules[biz] = append(this.rules[biz], ruleSlice...)
	return this
}

func (this *Watchdog) AddHandler(biz string, adapter ...handler.WatchdogHandler) *Watchdog {
	this.adapters[biz] = append(this.adapters[biz], adapter...)

	// 按照Priority排序
	adapters := this.adapters[biz]
	sort.SliceStable(adapters, func(i, j int) bool { return adapters[i].GetPriority() > adapters[j].GetPriority() })
	this.adapters[biz] = adapters

	return this
}

func (this *Watchdog) LoadPlugins(plugin Plugin) *Watchdog {
	if !reflect.ValueOf(plugin).MethodByName("IsActive").IsValid() {
		this.logger.Error("plugin %s does not have method IsActive, so skip...", reflect.TypeOf(plugin))
		return this
	}
	if !plugin.IsActive() {
		this.logger.Info("plugin %s is not active, so skip...", reflect.TypeOf(plugin))
		return this
	}
	if !reflect.ValueOf(plugin).MethodByName("Init").IsValid() {
		this.logger.Error("plugin %s does not have method Init, so skip...", reflect.TypeOf(plugin))
		return this
	}

	this.plugins = append(this.plugins, plugin)
	// TODO:Import hook

	plugin.Init(this)
	return this
}

func (this *Watchdog) Run() {
	// TODO:AutoCheck hook
	// TODO:Init hook

	// 支持同时配置多种业务的监控策略
	for biz, rules := range this.rules {
		aRule := &watcher.Rule{
			Biz:            biz,
			Rules:          rules,
			DelayQueueChan: make(chan fsnotify.FileEvent),
			Delay:          3 * time.Second,
			TaskQueueChan:  make(chan []fsnotify.FileEvent),
		}
		go this.Listen(aRule)
		go this.TransferDebounce(aRule)
		// go this.Transfer(aRule)
		go this.Handle(aRule)
	}

	done := make(chan bool)
	// 如果done中还没放数据，那main挂起，直到放数据为止
	<-done
}

func (this *Watchdog) Listen(rule *watcher.Rule) {
	this.watchers[rule.Biz].Listen(rule)
}

func (this *Watchdog) TransferDebounce(rule *watcher.Rule) {
	timer := time.NewTimer(rule.Delay)
	var e fsnotify.FileEvent
	for {
		select {
		case e = <-rule.DelayQueueChan:
			this.cacheQ = append(this.cacheQ, e)
			timer.Reset(rule.Delay)
		case <-timer.C:
			if len(this.cacheQ) == 0 {
				break
			}
			rule.TaskQueueChan <- this.cacheQ
			this.cacheQ = []fsnotify.FileEvent{}
		}
	}
}

func (this *Watchdog) Transfer(rule *watcher.Rule) {
	var e fsnotify.FileEvent
	for {
		select {
		case e = <-rule.DelayQueueChan:
			rule.TaskQueueChan <- []fsnotify.FileEvent{e}
		}
	}
}

func (this *Watchdog) Handle(rule *watcher.Rule) {
	var e []fsnotify.FileEvent
	for {
		select {
		case e = <-rule.TaskQueueChan:
			this.handle(e)
		}
	}
}

func (this *Watchdog) handle(fileEvents []fsnotify.FileEvent) error {
	fileEvents = this.filterEvents(fileEvents)
	changeFileMeta, err := this.getFileMeta(fileEvents)
	if err != nil {
		this.logger.Error("[getFileMeta]%s", err)
		return err
	}
	this.adapterHandle(changeFileMeta)
	return nil
}

func (this *Watchdog) filterEvents(fileEvents []fsnotify.FileEvent) []fsnotify.FileEvent {
	var list []fsnotify.FileEvent
	keys := make(map[string]bool)
	// 倒序，确保list中维护一个最新的事件列表
	for i := len(fileEvents) - 1; i >= 0; i-- {
		filename := fileEvents[i].Name
		if _, ok := keys[filename]; !ok {
			keys[filename] = true
			list = append(list, fileEvents[i])
		}
	}
	return list
}

func (this *Watchdog) getFileMeta(fileEvents []fsnotify.FileEvent) ([]*handler.FileMeta, error) {
	var fileMetas []*handler.FileMeta
	// TODO:如何并行获取
	for _, event := range fileEvents {
		fileMeta, err := this.getOneFileMeta(event)
		if err != nil {
			return nil, err
		}

		// TODO:Filter hook

		fileMetas = append(fileMetas, fileMeta)
	}
	return fileMetas, nil
}

func (this *Watchdog) getOneFileMeta(fileEvent fsnotify.FileEvent) (*handler.FileMeta, error) {
	fileInfo, err := os.Lstat(fileEvent.Name)
	if err != nil {
		return new(handler.FileMeta), err
	}
	if fileInfo.IsDir() {
		return new(handler.FileMeta), errors.New("[getOneFileMeta]仅处理文件，忽略目录")
	}

	// 获取文件目录
	// Ref: https://golang.org/pkg/path/filepath/#Split
	dirName, _ := filepath.Split(fileEvent.Name)
	// 获取文件相关时间，支持跨平台
	// fileTime, err := times.Stat(fileEvent.Name)
	// if err != nil {
	// 	return new(FileMeta), err
	// }
	// var fileCreateTime time.Time
	// if fileTime.HasChangeTime() { // 非Win
	// 	fileCreateTime = fileTime.ChangeTime()
	// }
	// if fileTime.HasBirthTime() { // Win
	// 	fileCreateTime = fileTime.BirthTime()
	// }
	fileTime := times.Get(fileInfo)

	// fileCreateTime, _ := time.Parse("2006-01-02 15:04:05-0700", "2018-09-28 08:15:22+0000")
	// TODO:矫正文件的创建时间
	fileCreateTime := fileTime.ChangeTime().Truncate(time.Millisecond).UTC()

	return &handler.FileMeta{
		Filepath:   fileEvent.Name,
		Dirname:    dirName,
		Filename:   fileInfo.Name(),
		Ext:        filepath.Ext(fileInfo.Name()),
		Size:       fileInfo.Size(),
		CreateTime: fileCreateTime,
		ModifyTime: fileInfo.ModTime().Truncate(time.Millisecond).UTC(),
		LastOp:     fileEvent,
		Host:       this.host,
	}, nil
}

func (this *Watchdog) adapterHandle(files []*handler.FileMeta) {
	var wg sync.WaitGroup
	for _, fi := range files {
		wg.Add(1)
		go func(file *handler.FileMeta) {
			defer wg.Done()

			failure := false
			for _, Adapter := range this.adapters[file.LastOp.Biz] {
				Adapter.SetLogger(this.logger)
				if err := Adapter.Handle(*file); err != nil {
					// TODO:失败重试
					this.logger.Error("File Handle Error: %s", err)
					failure = true
					break
				}
			}
			if failure {
				this.logger.Error("Need To Rollback File: %s", file.Filepath)
				this.adapterRollback(file)
			}
		}(fi)
	}
	wg.Wait()
}

func (this *Watchdog) adapterRollback(file *handler.FileMeta) {
	// 	var syncWg sync.WaitGroup
	// 	for _, Adapter := range this.adapters[file.LastOp.Biz] {
	// 		syncWg.Add(1)
	// 		go func(adapterhandler.WatchdogHandler) {
	// 			defer syncWg.Done()

	// 			go adapter.SetLogger(this.logger).Rollback(*file)
	// 		}(Adapter)
	// 	}
	// 	syncWg.Wait()

	// 	// TODO:将处理失败的事件传送至失败通道
}
