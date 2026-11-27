package api

import (
	"compress/gzip"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

// gzipWriterPool reuses gzip.Writers across requests to avoid per-request
// allocation of the (fairly large) compression state.
var gzipWriterPool = sync.Pool{
	New: func() interface{} {
		// Level is set per-use via Reset; DefaultCompression is a good
		// size/CPU tradeoff for JSON.
		w, _ := gzip.NewWriterLevel(nil, gzip.DefaultCompression)
		return w
	},
}

// Gzip returns middleware that gzip-compresses response bodies when the client
// advertises support via Accept-Encoding. It is intended for the JSON API group
// only (not static files, to avoid interfering with range requests).
//
// Why this exists: API responses like /builds/batch can be ~0.5 MB of JSON.
// Over a high-latency link (e.g. ~140 ms RTT) an uncompressed body of that size
// is dominated by TCP slow-start — the congestion window needs ~5-6 round trips
// to ramp up, so wall-clock time is ~700 ms+ regardless of bandwidth. gzip cuts
// JSON ~8-10x, collapsing that to 1-2 round trips.
func Gzip() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Only compress when the client asked for it.
		if !strings.Contains(c.GetHeader("Accept-Encoding"), "gzip") {
			c.Next()
			return
		}
		// Don't double-encode if a handler already set an encoding.
		if c.Writer.Header().Get("Content-Encoding") != "" {
			c.Next()
			return
		}

		gz := gzipWriterPool.Get().(*gzip.Writer)
		gz.Reset(c.Writer)

		gw := &gzipResponseWriter{ResponseWriter: c.Writer, gz: gz}
		c.Header("Content-Encoding", "gzip")
		c.Header("Vary", "Accept-Encoding")
		c.Writer = gw

		defer func() {
			// Flush + close the gzip stream, then restore the original writer
			// and return the gzip.Writer to the pool.
			_ = gz.Close()
			c.Writer = gw.ResponseWriter
			gzipWriterPool.Put(gz)
		}()

		c.Next()
	}
}

// gzipResponseWriter wraps gin's ResponseWriter, routing the body through a
// gzip.Writer. Header/status handling is delegated to the embedded writer.
type gzipResponseWriter struct {
	gin.ResponseWriter
	gz *gzip.Writer
}

func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	return w.gz.Write(data)
}

func (w *gzipResponseWriter) WriteString(s string) (int, error) {
	return w.gz.Write([]byte(s))
}

// WriteHeader strips Content-Length: once the body is gzipped the original
// length (if any handler set it) no longer matches the bytes on the wire.
func (w *gzipResponseWriter) WriteHeader(code int) {
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(code)
}

// Flush flushes the gzip buffer before flushing the underlying writer, so
// streamed data isn't stuck in the compressor.
func (w *gzipResponseWriter) Flush() {
	_ = w.gz.Flush()
	w.ResponseWriter.Flush()
}
