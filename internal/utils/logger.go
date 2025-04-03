package utils

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// LogLevel represents the logging level
type LogLevel string

// Log levels
const (
	DebugLevel LogLevel = "debug"
	InfoLevel  LogLevel = "info"
	WarnLevel  LogLevel = "warn"
	ErrorLevel LogLevel = "error"
	FatalLevel LogLevel = "fatal"
)

// contextKey is a type for context value keys
type contextKey string

// Context keys
const (
	loggerKey contextKey = "logger"
	requestID contextKey = "request_id"
)

// InitLogger initializes the global logger with the given settings
func InitLogger(level LogLevel, pretty bool) {
	// Set global log level
	zerolog.SetGlobalLevel(parseLogLevel(level))

	// Configure output writer
	var output io.Writer = os.Stdout
	if pretty {
		output = zerolog.ConsoleWriter{
			Out:        os.Stdout,
			TimeFormat: time.RFC3339,
		}
	}

	// Set global logger
	log.Logger = zerolog.New(output).
		With().
		Timestamp().
		Caller().
		Logger()

	Info("Logger initialized", map[string]interface{}{
		"level":  level,
		"pretty": pretty,
	})
}

// parseLogLevel converts our LogLevel to zerolog.Level
func parseLogLevel(level LogLevel) zerolog.Level {
	switch level {
	case DebugLevel:
		return zerolog.DebugLevel
	case InfoLevel:
		return zerolog.InfoLevel
	case WarnLevel:
		return zerolog.WarnLevel
	case ErrorLevel:
		return zerolog.ErrorLevel
	case FatalLevel:
		return zerolog.FatalLevel
	default:
		return zerolog.InfoLevel
	}
}

// WithRequestID adds a request ID to the logger in the context
func WithRequestID(ctx context.Context, reqID string) context.Context {
	logger := zerolog.Ctx(ctx).With().Str("request_id", reqID).Logger()
	ctx = context.WithValue(ctx, requestID, reqID)
	return logger.WithContext(ctx)
}

// Debug logs a debug message with additional fields
func Debug(msg string, fields map[string]interface{}) {
	event := log.Debug()
	addFields(event, fields)
	event.Msg(msg)
}

// Info logs an info message with additional fields
func Info(msg string, fields map[string]interface{}) {
	event := log.Info()
	addFields(event, fields)
	event.Msg(msg)
}

// Warn logs a warning message with additional fields
func Warn(msg string, fields map[string]interface{}) {
	event := log.Warn()
	addFields(event, fields)
	event.Msg(msg)
}

// Error logs an error message with additional fields
func Error(err error, msg string, fields map[string]interface{}) {
	if fields == nil {
		fields = make(map[string]interface{})
	}
	fields["error"] = err.Error()

	event := log.Error()
	addFields(event, fields)
	event.Msg(msg)
}

// Fatal logs a fatal error message with additional fields and exits the program
func Fatal(err error, msg string, fields map[string]interface{}) {
	if fields == nil {
		fields = make(map[string]interface{})
	}
	fields["error"] = err.Error()

	event := log.Fatal()
	addFields(event, fields)
	event.Msg(msg)
}

// addFields adds map fields to the event
func addFields(event *zerolog.Event, fields map[string]interface{}) {
	if fields == nil {
		return
	}

	for k, v := range fields {
		event = event.Interface(k, v)
	}
}

// DebugContext logs a debug message with context
func DebugContext(ctx context.Context, msg string, fields map[string]interface{}) {
	// Get logger from context or use default
	event := log.Ctx(ctx).Debug()
	addFields(event, fields)
	event.Msg(msg)
}

// InfoContext logs an info message with context
func InfoContext(ctx context.Context, msg string, fields map[string]interface{}) {
	// Get logger from context or use default
	event := log.Ctx(ctx).Info()
	addFields(event, fields)
	event.Msg(msg)
}

// WarnContext logs a warning message with context
func WarnContext(ctx context.Context, msg string, fields map[string]interface{}) {
	// Get logger from context or use default
	event := log.Ctx(ctx).Warn()
	addFields(event, fields)
	event.Msg(msg)
}

// ErrorContext logs an error message with context
func ErrorContext(ctx context.Context, err error, msg string, fields map[string]interface{}) {
	if fields == nil {
		fields = make(map[string]interface{})
	}
	fields["error"] = err.Error()

	// Get logger from context or use default
	event := log.Ctx(ctx).Error()
	addFields(event, fields)
	event.Msg(msg)
}

// Timer is a utility for measuring and logging execution times
type Timer struct {
	Name      string
	StartTime time.Time
}

// NewTimer creates a new timer with the given name
func NewTimer(name string) *Timer {
	return &Timer{
		Name:      name,
		StartTime: time.Now(),
	}
}

// Stop stops the timer and logs the elapsed time
func (t *Timer) Stop() time.Duration {
	elapsed := time.Since(t.StartTime)
	Info(fmt.Sprintf("%s completed", t.Name), map[string]interface{}{
		"duration_ms": elapsed.Milliseconds(),
	})
	return elapsed
}

// StopContext stops the timer and logs the elapsed time with context
func (t *Timer) StopContext(ctx context.Context) time.Duration {
	elapsed := time.Since(t.StartTime)
	InfoContext(ctx, fmt.Sprintf("%s completed", t.Name), map[string]interface{}{
		"duration_ms": elapsed.Milliseconds(),
	})
	return elapsed
}

// GenerateShortID generates a short random ID string
// used for request and reservation identifiers
func GenerateShortID() string {
	return fmt.Sprintf("%x-%x", time.Now().UnixNano(),
		rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000))
}

// WithLogger adds a logger to the context
func WithLogger(ctx context.Context, logger *zerolog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// GetLogger retrieves the logger from the context
func GetLogger(ctx context.Context) *zerolog.Logger {
	if l, ok := ctx.Value(loggerKey).(*zerolog.Logger); ok {
		return l
	}
	logger := log.With().Str("from", "context").Logger()
	return &logger
}
