package ginreq

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestGinBindJSONRejectsLegacyPayableField(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(`{"name":"ada","subject_type":"legacy"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	var body struct {
		Name string `json:"name" binding:"required"`
	}
	err := ginTransport{c: c}.BindJSON(&body)
	if err == nil || !strings.Contains(err.Error(), "subject_type is no longer supported") {
		t.Fatalf("expected subject_type rejection, got %v", err)
	}
}
