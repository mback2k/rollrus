// Package rollrus combines github.com/rollbar/rollbar-go with github.com/sirupsen/logrus
// via logrus.Hook mechanism, so that whenever logrus' logger.Error/f(),
// logger.Fatal/f() or logger.Panic/f() are used the messages are
// intercepted and sent to Rollbar.
//
// Using SetupLogging should suffice for basic use cases that use the logrus
// singleton logger.
//
// More custom uses are supported by creating a new Hook with NewHook and
// registering that hook with the logrus Logger of choice.
//
// The levels can be customized with the WithLevels OptionFunc.
//
// Specific errors can be ignored with the WithIgnoredErrors OptionFunc. This is
// useful for ignoring errors such as context.Canceled.
//
// See the Examples in the tests for more usage.
package rollrus

import (
	"fmt"
	"runtime"
	"strings"
	"time"

	rollbar "github.com/rollbar/rollbar-go"
	"github.com/sirupsen/logrus"
)

var defaultTriggerLevels = []logrus.Level{
	logrus.ErrorLevel,
	logrus.FatalLevel,
	logrus.PanicLevel,
}

// Hook is a wrapper for the Rollbar Client and is usable as a logrus.Hook.
type Hook struct {
	*rollbar.Client
	triggers        []logrus.Level
	ignoredErrors   []error
	ignoreErrorFunc func(error) bool
	ignoreFunc      func(error, map[string]interface{}) bool

	// only used for tests to verify whether or not a report happened.
	reported bool
}

// OptionFunc that can be passed to NewHook.
type OptionFunc func(*Hook)

// wellKnownErrorFields are the names of the fields to be checked for values of
// type `error`, in priority order.
var wellKnownErrorFields = []string{
	logrus.ErrorKey, "err",
}

// WithLevels is an OptionFunc that customizes the log.Levels the hook will
// report on.
func WithLevels(levels ...logrus.Level) OptionFunc {
	return func(h *Hook) {
		h.triggers = levels
	}
}

// WithMinLevel is an OptionFunc that customizes the log.Levels the hook will
// report on by selecting all levels more severe than the one provided.
func WithMinLevel(level logrus.Level) OptionFunc {
	var levels []logrus.Level
	for _, l := range logrus.AllLevels {
		if l <= level {
			levels = append(levels, l)
		}
	}

	return func(h *Hook) {
		h.triggers = levels
	}
}

// WithIgnoredErrors is an OptionFunc that whitelists certain errors to prevent
// them from firing. See https://golang.org/ref/spec#Comparison_operators
func WithIgnoredErrors(errors ...error) OptionFunc {
	return func(h *Hook) {
		h.ignoredErrors = append(h.ignoredErrors, errors...)
	}
}

// WithIgnoreErrorFunc is an OptionFunc that receives the error that is about
// to be logged and returns true/false if it wants to fire a Rollbar alert for.
func WithIgnoreErrorFunc(fn func(error) bool) OptionFunc {
	return func(h *Hook) {
		h.ignoreErrorFunc = fn
	}
}

// WithIgnoreFunc is an OptionFunc that receives the error and custom fields that are about
// to be logged and returns true/false if it wants to fire a Rollbar alert for.
func WithIgnoreFunc(fn func(err error, fields map[string]interface{}) bool) OptionFunc {
	return func(h *Hook) {
		h.ignoreFunc = fn
	}
}

// NewHook creates a hook that is intended for use with your own logrus.Logger
// instance. Uses the default report levels defined in wellKnownErrorFields.
func NewHook(token string, env string, opts ...OptionFunc) *Hook {
	h := NewHookForLevels(token, env, defaultTriggerLevels)

	for _, o := range opts {
		o(h)
	}

	return h
}

// NewHookForLevels provided by the caller. Otherwise works like NewHook.
func NewHookForLevels(token string, env string, levels []logrus.Level) *Hook {
	return &Hook{
		Client:          rollbar.NewSync(token, env, "", "", ""),
		triggers:        levels,
		ignoredErrors:   make([]error, 0),
		ignoreErrorFunc: func(error) bool { return false },
		ignoreFunc:      func(error, map[string]interface{}) bool { return false },
	}
}

// SetupLogging for use on Heroku. If token is not an empty string a Rollbar
// hook is added with the environment set to env. The log formatter is set to a
// TextFormatter with timestamps disabled.
func SetupLogging(token, env string) {
	setupLogging(token, env, defaultTriggerLevels)
}

// SetupLoggingForLevels works like SetupLogging, but allows you to
// set the levels on which to trigger this hook.
func SetupLoggingForLevels(token, env string, levels []logrus.Level) {
	setupLogging(token, env, levels)
}

func setupLogging(token, env string, levels []logrus.Level) {
	logrus.SetFormatter(&logrus.TextFormatter{DisableTimestamp: true})

	if token != "" {
		logrus.AddHook(NewHookForLevels(token, env, levels))
	}
}

// ReportPanic attempts to report the panic to Rollbar using the provided
// client and then re-panic. If it can't report the panic it will print an
// error to stderr.
func (r *Hook) ReportPanic() {
	if p := recover(); p != nil {
		r.Client.ErrorWithLevel(rollbar.CRIT, fmt.Errorf("panic: %q", p))
		panic(p)
	}
}

// ReportPanic attempts to report the panic to Rollbar if the token is set
func ReportPanic(token, env string) {
	if token != "" {
		h := &Hook{Client: rollbar.New(token, env, "", "", "")}
		h.ReportPanic()
	}
}

// Levels returns the logrus log.Levels that this hook handles
func (r *Hook) Levels() []logrus.Level {
	if r.triggers == nil {
		return defaultTriggerLevels
	}
	return r.triggers
}

// Fire the hook. This is called by Logrus for entries that match the levels
// returned by Levels().
func (r *Hook) Fire(entry *logrus.Entry) error {
	cause := extractError(entry)
	for _, ie := range r.ignoredErrors {
		if ie == cause {
			return nil
		}
	}

	if r.ignoreErrorFunc(cause) {
		return nil
	}

	m := convertFields(entry.Data)
	if _, exists := m["time"]; !exists {
		m["time"] = entry.Time.Format(time.RFC3339)
	}

	if _, exists := m["msg"]; !exists && entry.Message != "" {
		m["msg"] = entry.Message
	}

	if r.ignoreFunc(cause, m) {
		return nil
	}

	r.report(entry, cause, m)

	return nil
}

func (r *Hook) report(entry *logrus.Entry, cause error, m map[string]interface{}) {
	level := entry.Level

	r.reported = true

	switch {
	case level == logrus.FatalLevel || level == logrus.PanicLevel:
		skip := framesToSkip(2)
		r.Client.ErrorWithStackSkipWithExtras(rollbar.CRIT, cause, skip, m)
	case level == logrus.ErrorLevel:
		skip := framesToSkip(2)
		r.Client.ErrorWithStackSkipWithExtras(rollbar.ERR, cause, skip, m)
	case level == logrus.WarnLevel:
		skip := framesToSkip(2)
		r.Client.ErrorWithStackSkipWithExtras(rollbar.WARN, cause, skip, m)
	case level == logrus.InfoLevel:
		r.Client.MessageWithExtras(rollbar.INFO, entry.Message, m)
	case level == logrus.DebugLevel:
		r.Client.MessageWithExtras(rollbar.DEBUG, entry.Message, m)
	}
}

// convertFields converts from log.Fields to map[string]interface{} so that we can
// report extra fields to Rollbar
func convertFields(fields logrus.Fields) map[string]interface{} {
	m := make(map[string]interface{})
	for k, v := range fields {
		switch t := v.(type) {
		case time.Time:
			m[k] = t.Format(time.RFC3339)
		case error:
			m[k] = t.Error()
		default:
			if s, ok := v.(fmt.Stringer); ok {
				m[k] = s.String()
			} else {
				m[k] = fmt.Sprintf("%+v", t)
			}
		}
	}

	return m
}

// extractError attempts to extract an error from a well known field, err or error
func extractError(entry *logrus.Entry) error {
	for _, f := range wellKnownErrorFields {
		e, ok := entry.Data[f]
		if !ok {
			continue
		}
		err, ok := e.(error)
		if !ok {
			continue
		}

		return err
	}

	// when no error found, default to the logged message.
	return fmt.Errorf(entry.Message)
}

// framesToSkip returns the number of caller frames to skip
// to get a stack trace that excludes rollrus and logrus.
func framesToSkip(rollrusSkip int) int {
	// skip 1 to get out of this function
	skip := rollrusSkip + 1

	// to get out of logrus, the amount can vary
	// depending on how the user calls the log functions
	// figure it out dynamically by skipping until
	// we're out of the logrus package
	for i := skip; ; i++ {
		_, file, _, ok := runtime.Caller(i)
		if !ok || !strings.Contains(file, "github.com/sirupsen/logrus") {
			skip = i
			break
		}
	}

	// rollbar-go is skipping too few frames (2)
	// subtract 1 since we're currently working from a function
	return skip + 2 - 1
}
