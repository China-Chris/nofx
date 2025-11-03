package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// registerSwaggerRoutes exposes the Swagger UI and specification if the spec is available.
func (s *Server) registerSwaggerRoutes() {
	if len(swaggerSpec) == 0 {
		return
	}

	s.router.GET("/swagger/doc.json", func(c *gin.Context) {
		c.Data(http.StatusOK, "application/json", swaggerSpec)
	})
	s.router.GET("/swagger", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, swaggerHTML)
	})
	s.router.GET("/swagger/", func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, swaggerHTML)
	})
}

const swaggerHTML = `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8" />
  <title>NoFX API Docs</title>
  <link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui.css" />
  <style>
    html, body {
      margin: 0;
      padding: 0;
      height: 100%;
    }
    #swagger-ui {
      height: 100%;
    }
  </style>
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-bundle.js"></script>
  <script src="https://cdn.jsdelivr.net/npm/swagger-ui-dist@5/swagger-ui-standalone-preset.js"></script>
  <script>
    window.addEventListener('load', function() {
      window.ui = SwaggerUIBundle({
        url: '/swagger/doc.json',
        dom_id: '#swagger-ui',
        presets: [
          SwaggerUIBundle.presets.apis,
          SwaggerUIStandalonePreset
        ],
        layout: 'StandaloneLayout'
      });
    });
  </script>
</body>
</html>`
