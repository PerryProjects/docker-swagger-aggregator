// Package logging builds the process logger (zap) and routes client-go's klog output through it.
package logging

import (
	"fmt"
	"io"

	"github.com/go-logr/zapr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/klog/v2"
)

// Format values accepted by New.
const (
	FormatJSON    = "json"
	FormatConsole = "console"
)

// New builds a logger writing to out. level is debug, info, warn or error; format is
// "json" (one object per line, for log collectors) or "console" (human readable).
func New(level, format string, out io.Writer) (*zap.Logger, error) {
	lvl, err := zapcore.ParseLevel(level)
	if err != nil {
		return nil, fmt.Errorf("invalid log level %q (use debug, info, warn or error)", level)
	}
	enc := zapcore.EncoderConfig{
		TimeKey: "time", LevelKey: "level", NameKey: "logger", MessageKey: "msg", StacktraceKey: "stacktrace",
		EncodeTime: zapcore.ISO8601TimeEncoder, EncodeDuration: zapcore.StringDurationEncoder,
		EncodeLevel: zapcore.LowercaseLevelEncoder, EncodeName: zapcore.FullNameEncoder,
	}
	var encoder zapcore.Encoder
	switch format {
	case FormatJSON:
		encoder = zapcore.NewJSONEncoder(enc)
	case FormatConsole:
		enc.EncodeLevel = zapcore.CapitalLevelEncoder
		encoder = zapcore.NewConsoleEncoder(enc)
	default:
		return nil, fmt.Errorf("invalid log format %q (use json or console)", format)
	}
	return zap.New(zapcore.NewCore(encoder, zapcore.AddSync(out), lvl), zap.ErrorOutput(zapcore.AddSync(out))), nil
}

// RedirectKlog sends client-go's klog output to log. klog is process-global, so call this once, from main.
func RedirectKlog(log *zap.Logger) {
	klog.SetLogger(zapr.NewLogger(log.Named("client-go")))
}
