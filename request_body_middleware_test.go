package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

type unreadSizeTestBody struct{ reads int }

func (b *unreadSizeTestBody) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}
func (b *unreadSizeTestBody) Close() error { return nil }

func TestRequestBodyMiddlewareSizeDiagnosticsAndCORS(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const limit = 100
	var compressed bytes.Buffer
	zip := gzip.NewWriter(&compressed)
	_, _ = zip.Write([]byte(strings.Repeat("x", 200)))
	if err := zip.Close(); err != nil {
		t.Fatal(err)
	}
	observedLimit := int64(limit + 1)
	for _, tc := range []struct {
		name, stage, encoding string
		body                  []byte
		declared              int64
		observed              *int64
	}{
		{"declared size", "content_length", "", nil, 200, nil},
		{"chunked body", "wire_body", "", []byte(strings.Repeat("x", 200)), -1, &observedLimit},
		{"decoded body", "decompressed_body", "gzip", compressed.Bytes(), int64(compressed.Len()), &observedLimit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			configureRequestBodyMiddleware(router, limit)
			router.POST("/v1/responses", func(c *gin.Context) { t.Error("oversize body reached handler") })
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(tc.body))
			request.ContentLength = tc.declared
			unread := &unreadSizeTestBody{}
			if tc.stage == "content_length" {
				request.Body = unread
			}
			request.Header.Set("Origin", "https://client.example")
			request.Header.Set("Content-Encoding", tc.encoding)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if unread.reads != 0 {
				t.Fatal("Content-Length rejection read the request body")
			}
			if recorder.Header().Get("Access-Control-Allow-Origin") != "*" ||
				!strings.Contains(recorder.Header().Get("Access-Control-Expose-Headers"), "X-Codex2API-Max-Request-Body-Bytes") ||
				recorder.Header().Get("X-Codex2API-Max-Request-Body-Bytes") != "100" {
				t.Fatalf("inaccessible size diagnostics: %v", recorder.Header())
			}
			var response struct {
				Error struct {
					Stage      string `json:"size_stage"`
					Observed   *int64 `json:"observed_body_bytes"`
					Declared   *int64 `json:"declared_content_length"`
					LowerBound bool   `json:"observed_is_lower_bound"`
				} `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			got := response.Error
			if got.Stage != tc.stage || (got.Observed == nil) != (tc.observed == nil) ||
				got.Observed != nil && *got.Observed != *tc.observed || got.LowerBound != (tc.observed != nil) {
				t.Fatalf("incorrect observation: %+v", got)
			}
			if tc.declared >= 0 && (got.Declared == nil || *got.Declared != tc.declared) || tc.declared < 0 && got.Declared != nil {
				t.Fatalf("incorrect declared length: %+v", got)
			}
		})
	}
}

func TestRequestBodyMiddlewareImportLimitHeader(t *testing.T) {
	previous := security.MaxImportBodySize
	security.MaxImportBodySize = 150
	t.Cleanup(func() { security.MaxImportBodySize = previous })
	router := gin.New()
	configureRequestBodyMiddleware(router, 100)
	router.POST("/api/admin/accounts/import", func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if len(body) != 150 {
			t.Errorf("read bytes=%d, want 150", len(body))
		}
		if _, ok := err.(*http.MaxBytesError); !ok {
			t.Errorf("read error=%v, want import size limit", err)
		}
		c.Status(http.StatusBadRequest)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", strings.NewReader(strings.Repeat("x", 200))))
	if recorder.Code != http.StatusBadRequest || recorder.Header().Get("X-Codex2API-Max-Request-Body-Bytes") != "150" {
		t.Fatalf("incorrect import limit: status=%d headers=%v", recorder.Code, recorder.Header())
	}
}
