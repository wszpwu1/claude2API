package admin

import (
	"embed"
	"net/http"

	"github.com/gin-gonic/gin"
)

//go:embed web/index.html
var webAssets embed.FS

// RegisterWebRoutes mounts the management console page.
func RegisterWebRoutes(router *gin.Engine) {
	router.GET("/admin", serveAdminConsole)
	router.GET("/admin/", serveAdminConsole)
}

func serveAdminConsole(c *gin.Context) {
	content, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "management console is unavailable")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", content)
}
