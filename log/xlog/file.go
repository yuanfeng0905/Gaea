// Copyright 2019 The Gaea Authors. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package xlog

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// XFileLog is the file logger
type XFileLog struct {
	filename string
	path     string
	level    int

	skip     int
	runtime  bool
	file     *os.File
	errFile  *os.File
	hostname string
	service  string
	split    sync.Once
	mu       sync.Mutex
	Cancel   context.CancelFunc
}

// constants of XFileLog
const (
	XFileLogDefaultLogID = "900000001"
	SpliterDelay         = 5
	DefaultLogKeepDays   = 3
	DefaultLogKeepCounts = 72
)

// NewXFileLog is the constructor of XFileLog
// 生成一个日志实例，service用来标识业务的服务名。
// 比如：logger := xlog.NewXFileLog("gaea")
func NewXFileLog() XLogger {
	return &XFileLog{
		skip: XLogDefSkipNum,
	}
}

// Init implements XLogger
func (p *XFileLog) Init(config map[string]string) (err error) {
	path, ok := config["path"]
	if !ok {
		err = fmt.Errorf("init XFileLog failed, not found path")
		return
	}

	filename, ok := config["filename"]
	if !ok {
		err = fmt.Errorf("init XFileLog failed, not found filename")
		return
	}

	level, ok := config["level"]
	if !ok {
		err = fmt.Errorf("init XFileLog failed, not found level")
		return
	}

	service, _ := config["service"]
	if len(service) > 0 {
		p.service = service
	}

	runtime, ok := config["runtime"]
	if (!ok || runtime == "true" || runtime == "TRUE") && LevelFromStr(config["level"]) == DebugLevel {
		p.runtime = true
	} else {
		p.runtime = false
	}

	skip, _ := config["skip"]
	if len(skip) > 0 {
		skipNum, err := strconv.Atoi(skip)
		if err == nil {
			p.skip = skipNum
		}
	}

	isDir, err := isDir(path)
	if err != nil || !isDir {
		err = os.MkdirAll(path, 0755)
		if err != nil {
			return newError("Mkdir failed, err:%v", err)
		}
	}

	p.path = path
	p.filename = filename
	p.level = LevelFromStr(level)

	hostname, _ := os.Hostname()
	p.hostname = hostname

	logKeepDays := DefaultLogKeepDays
	if days, err := strconv.Atoi(config["log_keep_days"]); err == nil && days != 0 {
		logKeepDays = days
	}

	logKeepCounts := DefaultLogKeepCounts
	if counts, err := strconv.Atoi(config["log_keep_counts"]); err == nil && counts != 0 {
		logKeepCounts = counts
	}
	var ctx context.Context
	ctx, p.Cancel = context.WithCancel(context.Background())
	body := func() {
		go p.spliter(ctx, logKeepDays, logKeepCounts)
	}
	doSplit, ok := config["dosplit"]
	if !ok {
		doSplit = "true"
	}
	if doSplit == "true" {
		p.split.Do(body)
	}
	return p.ReOpen()
}

// split the log file
func (p *XFileLog) spliter(ctx context.Context, keepDays int, logKeepCounts int) {
	preHour := time.Now().Hour()
	splitTime := time.Now().Format("2006010215")
	defer p.Close()
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second * SpliterDelay):
			p.clean(keepDays, logKeepCounts)
			if time.Now().Hour() != preHour {
				p.clean(keepDays, logKeepCounts)
				p.rename(splitTime)
				preHour = time.Now().Hour()
				splitTime = time.Now().Format("2006010215")
			}
		}
	}
}

// SetLevel implements XLogger
func (p *XFileLog) SetLevel(level string) {
	p.level = LevelFromStr(level)
}

// SetSkip implements XLogger
func (p *XFileLog) SetSkip(skip int) {
	p.skip = skip
}

func (p *XFileLog) openFile(filename string) (*os.File, error) {
	file, err := os.OpenFile(filename,
		os.O_CREATE|os.O_APPEND|os.O_WRONLY,
		0644,
	)

	if err != nil {
		return nil, newError("open %s failed, err:%v", filename, err)
	}

	return file, err
}

func delayClose(fp *os.File) {
	if fp == nil {
		return
	}
	time.Sleep(1000 * time.Millisecond)
	fp.Close()
}

func (p *XFileLog) clean(keepDays int, logKeepCounts int) (err error) {
	deadline := time.Now().AddDate(0, 0, -1*keepDays)
	fileTypeMap := make(map[string]int)
	var files []string
	files, err = filepath.Glob(fmt.Sprintf("%s/%s.log*", p.path, p.filename))
	if err != nil {
		return
	}
	var fileInfo os.FileInfo
	for i := len(files) - 1; i >= 0; i-- {
		file := files[i]
		if canSkipClean(filepath.Base(file), p.filename) {
			continue
		}

		reg := regexp.MustCompile("[0-9]+")
		fileType := reg.ReplaceAllString(file, "")
		fileTypeMap[fileType]++
		if fileTypeMap[fileType] > logKeepCounts {
			_ = os.Remove(file)
			fileTypeMap[fileType]--
		}
	}

	for _, file := range files {
		if canSkipClean(filepath.Base(file), p.filename) {
			continue
		}
		if fileInfo, err = os.Stat(file); err == nil {
			if fileInfo.ModTime().Before(deadline) {
				os.Remove(file)
			} else if fileInfo.Size() == 0 {
				os.Remove(file)
			}
		}
	}
	return
}

func (p *XFileLog) rename(suffix string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	defer p.ReOpen()
	if p.file == nil {
		return
	}
	normalLog := p.path + "/" + p.filename + ".log"
	warnLog := normalLog + ".wf"
	newLog := fmt.Sprintf("%s/%s.log-%s.log", p.path, p.filename, suffix)
	newWarnLog := fmt.Sprintf("%s/%s.log.wf-%s.log.wf", p.path, p.filename, suffix)
	_ = removeFile(normalLog, newLog)
	_ = removeFile(warnLog, newWarnLog)
}

func removeFile(oldFile string, newFile string) (err error) {
	if fileInfo, err := os.Stat(oldFile); err == nil && fileInfo.Size() == 0 {
		return nil
	}
	if _, err = os.Stat(newFile); err == nil {
		return nil
	}
	return os.Rename(oldFile, newFile)
}

// ReOpen implements XLogger
func (p *XFileLog) ReOpen() error {
	go delayClose(p.file)
	go delayClose(p.errFile)

	normalLog := p.path + "/" + p.filename + ".log"
	file, err := p.openFile(normalLog)
	if err != nil {
		return err
	}

	p.file = file
	warnLog := normalLog + ".wf"
	p.errFile, err = p.openFile(warnLog)
	if err != nil {
		p.file.Close()
		p.file = nil
		return err
	}

	return nil
}

// Warn implements XLogger
func (p *XFileLog) Warn(format string, a ...any) error {
	if p.level > WarnLevel {
		return nil
	}

	return p.warnx(XFileLogDefaultLogID, format, a...)
}

// Warnx implements XLogger
func (p *XFileLog) Warnx(logID, format string, a ...any) error {
	if p.level > WarnLevel {
		return nil
	}

	return p.warnx(logID, format, a...)
}

func (p *XFileLog) warnx(logID, format string, a ...any) error {
	logText := formatValue(format, a...)
	fun, filename, lineno := getRuntimeInfo(p.skip)
	logText = formatLineInfo(p.runtime, fun, filepath.Base(filename), logText, lineno)
	//logText = fmt.Sprintf("[%s:%s:%d] %s", fun, filepath.Base(filename), lineno, logText)

	return p.write(WarnLevel, &logText, logID)
}

// Fatal implements XLogger
func (p *XFileLog) Fatal(format string, a ...any) error {
	if p.level > FatalLevel {
		return nil
	}

	return p.fatalx(XFileLogDefaultLogID, format, a...)
}

// Fatalx implements XLogger
func (p *XFileLog) Fatalx(logID, format string, a ...any) error {
	if p.level > FatalLevel {
		return nil
	}

	return p.fatalx(logID, format, a...)
}

func (p *XFileLog) fatalx(logID, format string, a ...any) error {
	logText := formatValue(format, a...)
	fun, filename, lineno := getRuntimeInfo(p.skip)
	logText = formatLineInfo(p.runtime, fun, filepath.Base(filename), logText, lineno)
	//logText = fmt.Sprintf("[%s:%s:%d] %s", fun, filepath.Base(filename), lineno, logText)

	return p.write(FatalLevel, &logText, logID)
}

// Notice implements XLogger
func (p *XFileLog) Notice(format string, a ...any) error {
	if p.level > NoticeLevel {
		return nil
	}
	return p.noticex(XFileLogDefaultLogID, format, a...)
}

// Noticex implements XLogger
func (p *XFileLog) Noticex(logID, format string, a ...any) error {
	if p.level > NoticeLevel {
		return nil
	}
	return p.noticex(logID, format, a...)
}

func (p *XFileLog) noticex(logID, format string, a ...any) error {
	logText := formatValue(format, a...)
	fun, filename, lineno := getRuntimeInfo(p.skip)
	logText = formatLineInfo(p.runtime, fun, filepath.Base(filename), logText, lineno)

	return p.write(NoticeLevel, &logText, logID)
}

// Trace implements XLogger
func (p *XFileLog) Trace(format string, a ...any) error {
	return p.tracex(XFileLogDefaultLogID, format, a...)
}

// Tracex implements XLogger
func (p *XFileLog) Tracex(logID, format string, a ...any) error {
	return p.tracex(logID, format, a...)
}

func (p *XFileLog) tracex(logID, format string, a ...any) error {
	if p.level > TraceLevel {
		return nil
	}

	logText := formatValue(format, a...)
	fun, filename, lineno := getRuntimeInfo(p.skip)
	logText = formatLineInfo(p.runtime, fun, filepath.Base(filename), logText, lineno)
	//logText = fmt.Sprintf("[%s:%s:%d] %s", fun, filepath.Base(filename), lineno, logText)

	return p.write(TraceLevel, &logText, logID)
}

// Debug implements XLogger
func (p *XFileLog) Debug(format string, a ...any) error {
	return p.debugx(XFileLogDefaultLogID, format, a...)
}

func (p *XFileLog) debugx(logID, format string, a ...any) error {
	if p.level > DebugLevel {
		return nil
	}

	logText := formatValue(format, a...)
	fun, filename, lineno := getRuntimeInfo(p.skip)
	logText = formatLineInfo(p.runtime, fun, filepath.Base(filename), logText, lineno)

	return p.write(DebugLevel, &logText, logID)
}

// Debugx implements XLogger
func (p *XFileLog) Debugx(logID, format string, a ...any) error {
	return p.debugx(logID, format, a...)
}

// Close implements XLogger
func (p *XFileLog) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.file != nil {
		p.file.Close()
		p.file = nil
	}

	if p.errFile != nil {
		p.errFile.Close()
		p.errFile = nil
	}

	p.Cancel()
}

// GetHost getter of hostname
func (p *XFileLog) GetHost() string {
	return p.hostname
}

func (p *XFileLog) write(level int, msg *string, logID string) error {
	levelText := levelTextArray[level]
	time := time.Now().Format("2006-01-02 15:04:05.000")

	logText := formatLog(msg, time, levelText, logID)
	file := p.file
	if level >= WarnLevel {
		file = p.errFile
	}

	file.Write([]byte(logText))
	return nil
}

func isDir(path string) (bool, error) {
	stat, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return stat.IsDir(), nil
}

// canSkipClean check if filename can be skipped by log clean
func canSkipClean(file string, filePrefix string) bool {
	skipList := []string{
		filePrefix + ".log",
		filePrefix + "_sql.log",
		filePrefix + ".log.wf",
		filePrefix + "_sql.log.wf",
	}

	for _, s := range skipList {
		if file == s {
			return true
		}
	}
	return false
}
