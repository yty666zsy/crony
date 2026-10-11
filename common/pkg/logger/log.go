package logger

import (
	"fmt"
	"github.com/natefinch/lumberjack"
	"github.com/tmnhs/crony/common/pkg/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"sync"
	"time"

	"os"
)

var (
	_defaultLogger *zap.Logger
	loggerMu       sync.RWMutex
)

func Init(projectName string, level string, format, prefix, director string, showLine bool, encodeLevel string, stacktraceKey string, logInConsole bool) (logger *zap.Logger) {
	if ok := utils.Exists(fmt.Sprintf("%s/%s", projectName, director)); !ok { // 判断是否有Director文件夹
		fmt.Printf("create %v directory\n", director)
		_ = os.Mkdir(fmt.Sprintf("%s/%s", projectName, director), os.ModePerm)
	}
	debugPriority := zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
		return lev == zap.DebugLevel
	})
	infoPriority := zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
		return lev == zap.InfoLevel
	})
	warnPriority := zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
		return lev == zap.WarnLevel
	})
	errorPriority := zap.LevelEnablerFunc(func(lev zapcore.Level) bool {
		return lev >= zap.ErrorLevel
	})
	cores := make([]zapcore.Core, 0)
	switch level {
	case "info":
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_info.log", projectName, director), infoPriority))
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_warn.log", projectName, director), warnPriority))
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_error.log", projectName, director), errorPriority))
	case "warn":
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_warn.log", projectName, director), warnPriority))
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_error.log", projectName, director), errorPriority))
	case "error":
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_error.log", projectName, director), errorPriority))
	default:
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_debug.log", projectName, director), debugPriority))
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_info.log", projectName, director), infoPriority))
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_warn.log", projectName, director), warnPriority))
		cores = append(cores, getEncoderCore(logInConsole, prefix, format, encodeLevel, stacktraceKey, fmt.Sprintf("%s/%s/server_error.log", projectName, director), errorPriority))
	}
	logger = zap.New(zapcore.NewTee(cores[:]...), zap.AddCaller())

	if showLine {
		logger = logger.WithOptions(zap.AddCaller())
	}
	loggerMu.Lock()
	_defaultLogger = logger
	loggerMu.Unlock()
	return logger
}

func getEncoderConfig(prefix, encodeLevel, stacktraceKey string) (config zapcore.EncoderConfig) {
	config = zapcore.EncoderConfig{
		MessageKey:    "message",
		LevelKey:      "level",
		TimeKey:       "time",
		NameKey:       "logger",
		CallerKey:     "caller",
		StacktraceKey: stacktraceKey,
		LineEnding:    zapcore.DefaultLineEnding,
		EncodeLevel:   zapcore.LowercaseLevelEncoder,
		EncodeTime: func(t time.Time, enc zapcore.PrimitiveArrayEncoder) {
			enc.AppendString(t.Format(prefix + utils.TimeFormatDateV4))
		},
		EncodeDuration: zapcore.SecondsDurationEncoder,
		EncodeCaller:   zapcore.FullCallerEncoder,
	}
	switch {
	case encodeLevel == "LowercaseLevelEncoder":
		config.EncodeLevel = zapcore.LowercaseLevelEncoder
	case encodeLevel == "LowercaseColorLevelEncoder":
		config.EncodeLevel = zapcore.LowercaseColorLevelEncoder
	case encodeLevel == "CapitalLevelEncoder":
		config.EncodeLevel = zapcore.CapitalLevelEncoder
	case encodeLevel == "CapitalColorLevelEncoder":
		config.EncodeLevel = zapcore.CapitalColorLevelEncoder
	default:
		config.EncodeLevel = zapcore.LowercaseLevelEncoder
	}
	return config
}

func getEncoder(prefix, format, encodeLevel, stacktraceKey string) zapcore.Encoder {
	if format == "json" {
		return zapcore.NewJSONEncoder(getEncoderConfig(prefix, encodeLevel, stacktraceKey))
	}
	return zapcore.NewConsoleEncoder(getEncoderConfig(prefix, encodeLevel, stacktraceKey))
}

func getEncoderCore(logInConsole bool, prefix, format, encodeLevel, stacktraceKey string, fileName string, level zapcore.LevelEnabler) (core zapcore.Core) {
	writer := getWriteSyncer(logInConsole, fileName) // 使用file-rotatelogs进行日志分割
	return zapcore.NewCore(getEncoder(prefix, format, encodeLevel, stacktraceKey), writer, level)
}

func getWriteSyncer(logInConsole bool, file string) zapcore.WriteSyncer {
	lumberJackLogger := &lumberjack.Logger{
		Filename:   file,
		MaxSize:    10,
		MaxBackups: 200,
		MaxAge:     30,
		Compress:   true,
	}

	if logInConsole {
		return zapcore.NewMultiWriteSyncer(zapcore.AddSync(os.Stdout), zapcore.AddSync(lumberJackLogger))
	}
	return zapcore.AddSync(lumberJackLogger)
}

func Sync() error {
	return GetLogger().Sync()
}

// Shutdown 全局关闭
func Shutdown() {
	_ = GetLogger().Sync()
}

// GetLogger 返回全局 logger。
//
// 旧实现直接返回包级变量：只要调用方忘记先执行 Init（单元测试、
// 启动早期的失败分支、被复用的库函数），返回值就是 nil，
// 紧接着的 Info/Error 调用会以空指针 panic 崩溃
// （node/internal/handler 的既有测试就是因此崩掉的）。
// 现在改为惰性创建"输出到 stderr"的兜底 logger，保证日志调用永不 panic。
func GetLogger() *zap.Logger {
	loggerMu.RLock()
	current := _defaultLogger
	loggerMu.RUnlock()
	if current != nil {
		return current
	}

	loggerMu.Lock()
	defer loggerMu.Unlock()
	if _defaultLogger == nil {
		cfg := zap.NewProductionConfig()
		cfg.OutputPaths = []string{"stderr"}
		cfg.ErrorOutputPaths = []string{"stderr"}
		fallback, err := cfg.Build()
		if err != nil {
			fallback = zap.NewNop()
		}
		fallback.Warn("logger 未初始化，已启用 stderr 兜底 logger（请检查 logger.Init 的调用时机）")
		_defaultLogger = fallback
	}
	return _defaultLogger
}
