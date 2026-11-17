package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/dtg-corp/dsoft/internal/store"
)

// APIKeyAuth validates the Bearer token as an API key and injects the product context.
func APIKeyAuth(s *store.Store) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := extractBearer(c)
		if raw == "" {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "missing api key")
			return
		}

		hash := store.HashAPIKey(raw)
		product, apiKey, err := s.FindProductByAPIKey(c, hash)
		if err != nil {
			abortWithError(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid api key")
			return
		}

		// auth_type is what RequireScope switches on; without it a
		// scoped API-key route would refuse every key as unauthenticated.
		c.Set("auth_type", "api_key")
		// A system-wide key is not bound to one product. Handlers that
		// care read product_id; a key with no product must not turn into
		// a nil dereference here.
		if product != nil {
			c.Set("product_id", product.ID)
			c.Set("product", product)
		}
		c.Set("api_key", apiKey)
		c.Next()
	}
}

func extractBearer(c *gin.Context) string {
	h := c.GetHeader("Authorization")
	if after, ok := strings.CutPrefix(h, "Bearer "); ok {
		return after
	}
	return ""
}
