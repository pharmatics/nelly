package nelly

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"time"

	"k8s.io/klog"

	"github.com/julienschmidt/httprouter"
)

// StacktracePred returns true if a stacktrace should be logged for this status.
type StacktracePred func(httpStatus int) (logStacktrace bool)

type logger interface {
	Addf(format string, data ...interface{})
}

type respLoggerContextKeyType int

// respLoggerContextKey is used to store the respLogger pointer in the request context.
const respLoggerContextKey respLoggerContextKeyType = iota

// Add a layer on top of ResponseWriter, so we can track latency and error
// message sources.
type respLogger struct {
	hijacked       bool
	statusRecorded bool
	status         int
	statusStack    string
	addedInfo      string
	startTime      time.Time

	captureErrorOutput bool

	req *http.Request
	w   http.ResponseWriter

	logStacktracePred StacktracePred
}

// Simple logger that logs immediately when Addf is called
type passthroughLogger struct{}

// Addf logs info immediately.
func (passthroughLogger) Addf(format string, data ...interface{}) {
	klog.V(2).Info(fmt.Sprintf(format, data...))
}

// defaultStacktracePred is the default implementation of StacktracePred.
func defaultStacktracePred(status int) bool {
	return (status < http.StatusOK || status >= http.StatusInternalServerError) && status != http.StatusSwitchingProtocols
}

// WithLogging handler wraps httprouter.Handle with logging
func WithLogging() Handler {

	fn := func(h httprouter.Handle) httprouter.Handle {
		return withLogging(h, defaultStacktracePred)
	}

	return fn
}

func withLogging(h httprouter.Handle, pred StacktracePred) httprouter.Handle {
	return func(w http.ResponseWriter, req *http.Request, p httprouter.Params) {
		ctx := req.Context()
		if old := respLoggerFromContext(req); old != nil {
			panic("multiple WithLogging calls!")
		}
		rl := newLogged(req, w).StacktraceWhen(pred)
		req = req.WithContext(context.WithValue(ctx, respLoggerContextKey, rl))

		defer rl.Log()
		h(rl, req, p)
	}
}

// respLoggerFromContext returns the respLogger or nil.
func respLoggerFromContext(req *http.Request) *respLogger {
	ctx := req.Context()
	val := ctx.Value(respLoggerContextKey)
	if rl, ok := val.(*respLogger); ok {
		return rl
	}
	return nil
}

// newLogged turns a normal response writer into a logged response writer.
func newLogged(req *http.Request, w http.ResponseWriter) *respLogger {
	return &respLogger{
		startTime:         time.Now(),
		req:               req,
		w:                 w,
		logStacktracePred: defaultStacktracePred,
	}
}

// StacktraceWhen sets the stacktrace logging predicate, which decides when to log a stacktrace.
// There's a default, so you don't need to call this unless you don't like the default.
func (rl *respLogger) StacktraceWhen(pred StacktracePred) *respLogger {
	rl.logStacktracePred = pred
	return rl
}

// StatusIsNot returns a StacktracePred which will cause stacktraces to be logged
// for any status *not* in the given list.
func StatusIsNot(statuses ...int) StacktracePred {
	statusesNoTrace := map[int]bool{}
	for _, s := range statuses {
		statusesNoTrace[s] = true
	}
	return func(status int) bool {
		_, ok := statusesNoTrace[status]
		return !ok
	}
}

// Addf adds additional data to be logged with this request.
func (rl *respLogger) Addf(format string, data ...interface{}) {
	rl.addedInfo += "\n" + fmt.Sprintf(format, data...)
}

// Log is intended to be called once at the end of your request handler, via defer
func (rl *respLogger) Log() {
	latency := time.Since(rl.startTime)
	if klog.V(3) {
		if !rl.hijacked {
			klog.InfoDepth(1, fmt.Sprintf("%s %s: (%v) %v%v%v [%s %s]", rl.req.Method, rl.req.RequestURI, latency, rl.status, rl.statusStack, rl.addedInfo, rl.req.UserAgent(), rl.req.RemoteAddr))
		} else {
			klog.InfoDepth(1, fmt.Sprintf("%s %s: (%v) hijacked [%s %s]", rl.req.Method, rl.req.RequestURI, latency, rl.req.UserAgent(), rl.req.RemoteAddr))
		}
	}
}

// Header implements http.ResponseWriter.
func (rl *respLogger) Header() http.Header {
	return rl.w.Header()
}

// Write implements http.ResponseWriter.
func (rl *respLogger) Write(b []byte) (int, error) {
	if !rl.statusRecorded {
		rl.recordStatus(http.StatusOK) // Default if WriteHeader hasn't been called
	}
	if rl.captureErrorOutput {
		rl.Addf("logging error output: %q\n", string(b))
	}
	return rl.w.Write(b)
}

// Flush implements http.Flusher even if the underlying http.Writer doesn't implement it.
// Flush is used for streaming purposes and allows to flush buffered data to the client.
func (rl *respLogger) Flush() {
	if flusher, ok := rl.w.(http.Flusher); ok {
		flusher.Flush()
	} else if klog.V(2) {
		klog.InfoDepth(1, fmt.Sprintf("Unable to convert %+v into http.Flusher", rl.w))
	}
}

// WriteHeader implements http.ResponseWriter.
func (rl *respLogger) WriteHeader(status int) {
	rl.recordStatus(status)
	rl.w.WriteHeader(status)
}

// Hijack implements http.Hijacker.
func (rl *respLogger) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	rl.hijacked = true
	return rl.w.(http.Hijacker).Hijack()
}

// CloseNotify implements http.CloseNotifier
func (rl *respLogger) CloseNotify() <-chan bool {
	return rl.w.(http.CloseNotifier).CloseNotify()
}

func (rl *respLogger) recordStatus(status int) {
	rl.status = status
	rl.statusRecorded = true
	if rl.logStacktracePred(status) {
		// Only log stacks for errors
		stack := make([]byte, 50*1024)
		stack = stack[:runtime.Stack(stack, false)]
		rl.statusStack = "\n" + string(stack)
		rl.captureErrorOutput = true
	} else {
		rl.statusStack = ""
	}
}
