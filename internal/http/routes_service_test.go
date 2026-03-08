package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func TestRegisterServiceRoutes_HealthWithoutAPIKey(t *testing.T) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := &Server{
		cfg:            &config.Config{},
		privateHandler: gin.New(),
	}

	srv.registerServiceRoutes()

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.privateHandler.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.JSONEq(t, `{"api":"service","status":"ok"}`, w.Body.String())
}
