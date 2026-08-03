package log

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

var levelNames = []string{"DEBUG", "INFO", "WARN", "ERROR"}

type Logger struct {
	mu      sync.Mutex
	file    *os.File
	logPath string
	level   LogLevel
}

func NewLogger(logPath string, level LogLevel) (*Logger, error) {
	logDir := filepath.Dir(logPath)
	if logDir != "" && logDir != "." {
		if err := os.MkdirAll(logDir, 0755); err != nil {
			return nil, fmt.Errorf("创建日志目录失败: %w", err)
		}
	}

	// 保留最近 2 次运行的日志：将当前日志重命名为 .1，再创建新日志
	rotateLog(logPath)

	file, err := os.OpenFile(logPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("打开日志文件失败: %w", err)
	}

	return &Logger{
		file:    file,
		logPath: logPath,
		level:   level,
	}, nil
}

// rotateLog 日志轮转：保留最近 2 次运行的日志
// cf-speedtest.log → cf-speedtest.log.1（覆盖旧的 .1）
func rotateLog(logPath string) {
	// 删除旧的 .1 备份
	oldBackup := logPath + ".1"
	os.Remove(oldBackup)
	// 将当前日志重命名为 .1
	os.Rename(logPath, oldBackup)
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}

func (l *Logger) Log(level LogLevel, operation, message string, args ...interface{}) {
	if level < l.level {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	logMsg := fmt.Sprintf(message, args...)

	entry := fmt.Sprintf("[%s] [%s] [%s] %s\n", timestamp, levelNames[level], operation, logMsg)

	_, _ = l.file.WriteString(entry)
	_, _ = os.Stdout.WriteString(entry)
}

func (l *Logger) Debug(operation, message string, args ...interface{}) {
	l.Log(LevelDebug, operation, message, args...)
}

func (l *Logger) Info(operation, message string, args ...interface{}) {
	l.Log(LevelInfo, operation, message, args...)
}

func (l *Logger) Warn(operation, message string, args ...interface{}) {
	l.Log(LevelWarn, operation, message, args...)
}

func (l *Logger) Error(operation, message string, args ...interface{}) {
	l.Log(LevelError, operation, message, args...)
}

func (l *Logger) GetLogPath() string {
	return l.logPath
}